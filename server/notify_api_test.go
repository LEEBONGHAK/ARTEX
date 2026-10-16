package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 푸시 기능의 엔드투엔드 동작을 커버: 취약점 DB 저장 → 이벤트 → 분배 → 실제 HTTP 전송.
//
// 보안상 주의점: 이 케이스들은 **전역 Notifier.step()을 호출하지 않고**, 자기가 만든
// 채널에만 stepRealtime/stepDigest를 호출한다. step()이 DB의 모든 활성 채널을 순회하기 때문——
// 실제 DingTalk/WeCom 봇이 이미 구성된 개발 DB에서 테스트를 돌리면, 전역 step이 테스트 중
// 생성된 취약점을 그 그룹들에 실제로 푸시함. 채널별 호출은 영향 범위를 테스트가 만든 가짜 수신단으로 엄격히 제한.
//
// 정리: 케이스 종료 시 이 케이스가 생성한 이벤트(전송 캐스케이드 삭제)와 채널을 삭제, 실제 채널에 적체를 남기지 않음.
//
// 단언 기준: stepRealtime/stepDigest는 값을 반환하지 않고 내부에서 로그를 남기므로, 여기서 단언하는 것은
// **관찰 가능한 외부 동작**(가짜 수신단이 무엇을 받았는지, 전송 행이 어떤 상태로 떨어졌는지)이지 함수의
// 반환값이 아님——이것이 반환값을 스텁하는 것보다 실제 호출 경로에 더 가깝다.

// notifyFixture 이 파일 케이스들의 공용 장치.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 자체 생성 task/exploration: 케이스가 취약점을 여기 기록, 다른 케이스 데이터와 격리.
	taskID int64
	expID  int64
	// cleanupMark 이후 생성된 이벤트는 정리 시 함께 삭제.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 이 파일의 모든 가짜 수신단은 127.0.0.1에서 돌고, 전송은 기본적으로 루프백 주소를 거부
	// (동일 머신 서비스와 클라우드 메타데이터로의 SSRF 방지). 테스트는 이 스위치를 명시적으로 켬;
	// 가드의 「기본 거부」 동작은 notify 패키지의 ssrf_test.go가 커버.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// task를 하나 자체 생성: 공용 장치 trafficEvidenceServer가 만든 task는 exploration id를 얻지 못하는데,
	// 취약점 기록에는 그것을 반드시 제공해야 함.
	task, err := s.m.CreateTask("통지 푸시 테스트", "푸시 동작 검증", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 장치를 자족적으로 닫음: fixture 생성 전에 이미 있던 이벤트를 한 번에 분배됨으로 표시.
	//
	// 왜 반드시 해야 하나: FanOutPendingEvents는 **전역**이라, DB의 모든 미분배 이벤트를
	// 매칭되는 모든 채널로 펼침. 그런데 공용 장치 trafficEvidenceServer 자체가 취약점을 하나 기록하고
	// (바로 그것이 반환하는 초기 finding), 다른 케이스의 잔여물도 있을 수 있음. 격리하지 않으면,
	// 이 잡다한 이벤트들이 이 케이스의 채널로 분배되어 「N건 전송이 있어야 함」류 단언이
	// 맞았다 틀렸다 함——게다가 틀리는 양상이 케이스 실행 순서에 달려 있어 바로 실패하는 것보다 추적이 어려움.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("통지 이벤트 정리 실패: %v", err)
		}
	})
	// 마스터 스위치는 켜져 있어야 함(다른 케이스가 꺼놨을 수 있음).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record 실제 증거 쓰기 경로를 거쳐 취약점을 하나 기록하고 finding id를 반환.
// 이 경로는 **같은 트랜잭션** 안에서 푸시 이벤트를 등록함——바로 이 기능의 훅 지점.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 요약",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("취약점 기록 실패: %v", err)
	}
	return out.FindingID
}

// channel 채널 구성을 읽어옴(채널별 stepX 호출용).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("채널 읽기 실패: %v", err)
	}
	return ch
}

// deliver 이벤트를 분배하고 지정 채널에만 전송 한 라운드를 돌린다.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel HTTP 인터페이스로 채널을 생성, 겸사겸사 인터페이스 자체의 검증 경로를 커버.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("채널 생성 실패 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("채널 생성 반환 이상: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook 받은 요청 본문을 기록하는 가짜 수신단.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("가짜 수신단이 %d건 요청만 받음, %d번째를 가져올 수 없음", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("가짜 수신단이 아무 요청도 받지 못함")
	}
	return f.body(t, f.count()-1)
}

// markdownText 요청 본문에서 본문을 꺼내며, 각 플랫폼의 필드명 차이를 호환:
// DingTalk markdown은 `text`、ActionCard는 `text`、WeCom markdown은 `content`.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("요청 본문에 식별 가능한 본문이 없음: %v", body)
	return ""
}

// agePendingBatch 해당 채널의 대기 전송을 강제로 늙혀, 요약 배치 만료 테스트에 사용.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "실시간 푸시",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL注入", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("메시지 1건을 발송해야 하는데 실제 %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL注入", "고위험", "요약"} {
		if !strings.Contains(text, want) {
			t.Fatalf("메시지 본문에 %q 없음:\n%s", want, text)
		}
	}
	// 전송은 sent로 전이돼야 함.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("전송 후에도 %d건이 sent로 표시 안 됨", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "마스킹 케이스",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("채널 목록 실패 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("인터페이스 회신이 자격 증명을 누출: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("새로 만든 채널이 목록에 나타나지 않음")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("자격 증명 필드는 마스킹 값이어야 함: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("인터페이스는 어떤 필드가 자격 증명인지 프런트에 알려야 함")
	}

	// PATCH 이름만 변경 + 마스킹 자격 증명 회신: 실제 자격 증명은 원문 보존돼야 함.
	body, _ := json.Marshal(map[string]any{
		"name":   "이름 변경 후",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("업데이트 실패 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("마스킹 회신이 실제 자격 증명을 덮음: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("마스킹 회신이 secret을 덮음: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "이름 변경 후" {
		t.Fatal("이름이 업데이트 안 됨")
	}

	// secret 명시적 비우기는 적용돼야 함(「마스킹 회신=변경 없음」과 구별).
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret 비우기 실패 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("빈 문자열은 secret을 비워야 함")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"유형 비합법", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "채널 유형 무효"},
		{"이름 누락", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "채널 이름 누락"},
		{"webhook 누락", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook 프로토콜 비합법", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook 주소 무효"},
		{"모드 비합법", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "푸시 모드 무효"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400을 반환해야 하는데 %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("에러 메시지는 %q를 언급해야 하는데 %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("존재하지 않는 채널 삭제는 404여야 하는데 %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "심각만",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "저위험 문제", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("임계값 미만 취약점은 전송을 만들면 안 되는데 %d건", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("필터링된 취약점은 메시지를 보내면 안 됨")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "요약 푸시",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("요약 취약점%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 미만료: 안 보냄.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("요약 배치가 만료 전에 발송됨")
	}

	// 배치를 늙힌 뒤: 3건이 한 메시지로 합쳐짐.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("3건은 한 메시지로 요약돼야 하는데 실제 %d건 발송", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "최근") || !strings.Contains(text, "3개 취약점") {
		t.Fatalf("요약 메시지에 건수/시간 창 문안이 없음:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("요약 취약점%d", i)) {
			t.Fatalf("요약 메시지에 %d번째 항목이 없음:\n%s", i, text)
		}
	}
	// 같은 배치는 batch_id를 공유해야 함.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("3건 전송은 batch_id 하나를 공유해야 하는데 distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "비활성 채널",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "비활성 기간 취약점", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("비활성 채널은 전송을 만들면 안 되는데 %d건", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 구독",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "상태 변경 케이스", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 두 건이 있어야 함: fixed 건은 상태 변경; finding_created 건도 같은 라운드에 발송될 수 있음.
	// 상태 변경이 실제로는 더 늦게 생성되지만 순서에 의존하지 않고 전량 탐색.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("「상태 변경 → 수정됨」이 든 메시지를 받지 못함(총 %d건)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 미구독",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "변경 미구독", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("상태 변경 미구독 채널은 상태 변경 전송을 받으면 안 되는데 %d건", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "테스트 전송",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("테스트 전송 실패 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("가짜 수신단은 테스트 메시지 1건을 받아야 하는데 %d", hook.count())
	}
	// 테스트 메시지는 한눈에 테스트임을 알 수 있어야 하며 실제 취약점으로 오인되면 안 됨.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("테스트 메시지는 테스트임을 명시해야 함: %s", text)
	}
	// 구성이 깨지면 채널의 원본 에러를 사용자에게 사실대로 돌려줘야 함.
	badID := f.createChannel(t, map[string]any{
		"name":   "잘못된 주소",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("전송 실패는 502를 반환해야 하는데 %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 반드시 실패하는 주소를 가리켜 failed 전송을 만듦.
	chID := f.createChannel(t, map[string]any{
		"name":   "실패 재시도",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "실패할 푸시", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 재시도 예산을 소진할 때까지 연속 전송.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("재시도 소진 후 failed여야 하는데 %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("이력 조회 실패 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("실패 전송 1건을 조회해야 하는데 total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("이력에 실패 원인이 있어야 함, 아니면 사용자가 조사 불가")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("시도 횟수가 기록돼야 하는데 %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "실패할 푸시" {
		t.Fatalf("이력은 취약점 제목을 담아야 하는데 %q", hist.Deliveries[0].Title)
	}

	// 수동 재발송: pending으로 돌아가고 카운트 0으로 초기화돼야 함.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("재발송 실패 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("재발송 후 pending이고 attempts=0이어야 하는데 %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 실패: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta는 전체 %d개 채널을 나열해야 하는데 %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("채널 %s가 자격 증명 필드를 보고 안 함", k.Kind)
		}
	}

	// 전역 설정 3개 왕복. 끝 슬래시는 정규화돼야 함, 아니면 백링크가 "//function/..."로 조립됨.
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("설정 쓰기 실패 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("백링크 주소가 정규화 안 됨: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("요약 주기가 적용 안 됨: %v", payload["notify_digest_interval_min"])
	}

	// 비합법 값은 거부돼야 함.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s는 400을 반환해야 하는데 %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL 백링크 조립을 커버: public_base_url이 설정되면
// 단건 메시지는 버튼이 있는 ActionCard를 써야 하고, 링크는 취약점 상세 페이지를 가리켜야 함.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "백링크",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "백링크 있는 취약점", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("백링크 있으면 ActionCard를 써야 하는데 msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("백링크 틀림\n기대 %s\n실제 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL 역방향 커버: 외부 주소 미설정 시 잘못된 링크를 만들면 안 됨
// (예: localhost나 상대 경로 지향), 순수 markdown으로 되돌아가야 함.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "백링크 없음",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "백링크 없는 취약점", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("외부 주소 미설정 시 markdown을 보내야 하는데 %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "상세 보기") {
		t.Fatalf("외부 주소 미설정 시 상세 링크가 나오면 안 됨:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder 「조용한 유실」 수정의 엔드투엔드 증거.
//
// 요약 메시지는 채널 길이 상한의 제약을 받음(WeCom 4096바이트), 한 배치가 안 들어가면 반드시 **항목 단위**로 분할:
// 이 메시지에 들어간 것은 송달됨으로 표시하고, 나머지는 큐로 돌아가 다음 메시지를 기다림. 예전 구현은 배치 전체를
// 성공으로 표시——잘려나간 것들은 메시지에도 없고 실패 목록에도 없는데 전송 이력은 성공으로 표시,
// 취약점이 그렇게 사라짐.
//
// 네 가지를 단언: ① 실제 담긴 건수만 표시됨 ② 나머지는 여전히 대기 ③ 연기된 항목은
// **재시도 횟수를 소비하지 않음** ④ 한 라운드 더 돌리면 나머지를 발송 가능(멈추지 않음).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// WeCom 사용: markdown 상한 4096바이트로 여섯 채널 중 가장 빠듯함.
	chID := f.createChannel(t, map[string]any{
		"name":   "분할 요약",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 제목을 길게 잡아 60건이 4096바이트를 훨씬 넘겨 반드시 분할되게 함.
	longName := strings.Repeat("초장문 취약점 이름", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("한 메시지만 발송해야 하는데 %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("일부 항목이 송달됨으로 표시돼야 함")
	}
	if pending == 0 {
		t.Fatalf("배치 %d건이 4096바이트에 전부 들어갈 수 없음, 대기 잔여가 있어야 함; sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("항목 수 불일치: sent=%d pending=%d total=%d(송달도 대기도 아님=유실)", sent, pending, total)
	}
	// 메시지 본문은 이 메시지에 포함 안 된 항목 수를 사실대로 알려야 함.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("메시지는 이 메시지에 포함 안 된 항목이 있음을 밝혀야 함:\n%.400s", text)
	}

	// 연기된 항목은 재시도 예산을 소비하면 안 됨: 수령 시 attempts를 낙관적으로 +1 했으니, 연기 시 다시 빼야 함.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("연기된 항목은 재시도 횟수를 소비하면 안 됨(아니면 몇 건 뒤 실패로 판정됨), attempts=%d", maxAttempts)
	}

	// 수렴할 때까지 반복 실행. 단언하는 것은 **최종적으로 전부 송달**되고 중간에 실제로 여러 라운드로 나뉘었다는 것——
	// 이것이 「두 번째 라운드에 다 보냄」보다 강함: 분할이 멈추지 않고 남은 항목을 잃지도 않음을 증명.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("분할 전송이 수렴 안 함: %d 라운드 돌렸는데 여전히 %d건 미결", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d 라운드에서 아무 진전 없음, 남은 %d건이 영원히 묶임", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096바이트 메시지 하나에 긴 제목 취약점 %d건이 안 들어감, 여러 라운드로 발송해야 하는데 실제 %d 라운드만 씀", total, rounds)
	}
	// 첫 라운드 이후 각 라운드는 **순수 연속 발송**이어야 하며, 채널이 거부한 항목이 없어야 함.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("가짜 수신단이 항상 성공을 반환하므로 실패 항목이 없어야 하는데 %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget 드리프트 방지 단언.
//
// 재시도 예산(db.MaxNotifyAttempts)과 백오프 시퀀스 표(notifyBackoff)는 두 패키지에 나뉘어 있음:
// 전자는 상태 머신의 정책, 후자는 엔진의 실행 박자. 둘 중 하나만 바꾸면——예: 예산을 5회로
// 올리고 백오프 단계 추가를 잊으면——코드는 에러를 안 내고, 4、5번째 재시도가 마지막 단계 간격을 그대로 써,
// 「재시도 리듬이 영문 모르게 느려짐」으로 나타나 조사 시 여기를 떠올리기 어려움.
// 둘의 길이가 같음을 단언해 이 드리프트가 CI에서 바로 드러나게 함.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("백오프 단계 수(%d)와 최대 시도 횟수(%d)가 불일치——하나를 바꾸면 다른 하나도 함께 바꿔야 함",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// 백오프 간격은 단조 비감소여야 함, 아니면 재시도가 점점 급해져 오히려 레이트리밋을 악화시킴.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("백오프 간격은 단조 비감소여야 함: %d단계 %v < %d단계 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget 「토큰 먼저 취득 후 수령」 순서를 고정.
// 반대면(먼저 수령 후 폐기), 레이트리밋에 막힌 전송이 이미 attempts를 한 번 센 상태라,
// 예산이 순수한 대기로 소진되어 결국 failed로 떨어짐.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// 토큰 버킷 자체만 테스트, Server 불필요(그걸 위해 하나 만들어서도 안 됨).
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 분당 1건: 버킷이 가득 차도 최대 1건.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("버킷이 가득 찼을 때 분당 1건은 토큰 1개를 취해야 하는데 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("토큰 소진 후 즉시 0을 반환해야 하는데 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("중간 시점에 토큰 하나를 채우면 안 되는데 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("한 주기를 채우면 토큰 1개를 보충해야 하는데 %d", got)
	}
	// 무제한 채널은 유한 상한을 쓰며, 단일 라운드가 무한 적체에 발목 잡히는 것을 방지.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("무제한은 라운드당 상한 %d를 반환해야 하는데 %d", notifyUnlimitedBurstPerTick, got)
	}
	// 채널 간 토큰 버킷은 서로 독립.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("채널 1의 버킷은 여전히 비어 있어야 하는데 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens 「want 개만 취득」 의미를 고정.
//
// 예전 구현은 버킷을 통째로 비운 뒤에야 호출 측이 잘랐음, 그래서 rate=100/min 채널이 버킷을 채우고
// 한 라운드에 5건만 쓰면 남은 95개 토큰을 그냥 폐기; 채널에 이번 라운드 대기 전송이 없을 때도 똑같이 차감.
// 결과적으로 주석이 주장한 「적체 시 rate_per_min 건을 한 번에 몰아 보낼 수 있음」이 어떤 경우에도 불가능.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 버킷 초기값은 가득(100), 이번 라운드엔 5개만 필요.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5일 때 토큰을 정확히 5개 취해야 하는데 %d", got)
	}
	// 핵심 단언: 남은 95개는 반드시 버킷에 있어야 하며, 비워져 폐기되면 안 됨.
	// 시간을 진행하지 않아, 취한 것이 보충이 아닌 기존 재고에서만 나오도록 보장.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("남은 토큰은 여전히 취득 가능해야 함(기대 95), %d——버킷이 라운드 통째로 비워짐", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("버킷이 다 취해져 0을 반환해야 하는데 %d", got)
	}
	// want<=0은 어떤 토큰도 차감하면 안 됨(빈 라운드는 과금 안 함).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0은 0을 반환해야 하는데 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0인 그 호출은 토큰을 소비하면 안 되고 여전히 20을 가득 취할 수 있어야 하는데 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget 요약 모드의 두 차원을 고정.
//
// 요약 배치 크기가 라운드당 요청 예산과 묶이면, rate_per_min=20 채널은 매
// 3초 tick마다 토큰 1개만 보충되어, 요약 메시지마다 취약점 1건만 담음——기능상 요약이
// 없는 것과 같은데, 메시지 머리말엔 「최근 30분간 신규 1개 취약점」이라 쓰임. 이 퇴화는 에러를 안 내고,
// 기존 엔드투엔드 케이스도 못 잡음(그들은 stepDigest에 충분히 큰 limit을 수동으로 넘겨
// step 안의 할당량 계산을 우회함), 그래서 여기서 결정 자체를 직접 단언.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 한 배치 = 한 메시지 = 한 요청 = 토큰 하나. 토큰의 단위는 메시지지 취약점이 아님.
	if tokens != 1 {
		t.Fatalf("요약 한 배치는 한 메시지만 보내므로 토큰 1개를 정확히 소비해야 하는데 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("요약 배치 크기는 메모리 상한 db.MaxDigestBatchSize=%d여야 하는데 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 핵심 관계: 배치 크기는 라운드당 요청 예산보다 훨씬 커야 함. 둘이 같은 자릿수가 되면,
	// 「메시지 몇 건 보내는지」와 「한 배치에 취약점 몇 건 담는지」를 다시 한 숫자로 뒤섞은 것.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("요약 배치 크기 %d는 라운드당 요청 예산 %d에 묶이면 안 됨——"+
			"요청 예산은 리스로부터 역산한 「요청을 몇 번 보내는지」이고, 「한 배치에 취약점 몇 건 담는지」와는 서로 다른 차원",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease 또 하나의 드리프트 방지 단언.
//
// 채널당 라운드당 전송 건수 상한(notifyMaxSendsPerChannelPerTick)은 리스 지속시간으로부터 역산한 것:
// 한 라운드에서 직렬 전송의 최악 소요 시간은 반드시 리스보다 작아야 함. 아니면 뒤쪽 몇 건을 다 보내기 전에 리스가 만료돼,
// 다중 인스턴스 배포 시 상대가 그것들을 다시 가져가 중복 전송함. 이 세 상수는 서로 다른 위치에 흩어져 있고,
// 아무거나 하나를 바꿔도 관계가 깨지지만 아무 에러도 안 남——그래서 여기에 못박음.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("단일 채널 한 라운드의 최악 소요 시간 %v는 리스 %v에 도달하거나 초과하면 안 됨"+
			"(notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v)——"+
			"이 세 상수 중 하나를 바꾸면 반드시 나머지 둘도 함께 점검할 것",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
