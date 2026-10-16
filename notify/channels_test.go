package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg 따옴표와 줄바꿈이 든 단발 메시지를 구성한다. 의도적으로 `"`와 `\n`이 든 제목/요약 사용:
// 템플릿 보간이 비합법 JSON을 가장 쉽게 만들어내는 입력이 바로 이것이다.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `登录处 "SQL注入" 风险`,
			VulnClass: "SQL注入",
			Severity:  "high",
			Summary:   "参数 id\n未过滤 导致注入",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg 요약 메시지 묶음을 구성한다.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "漏洞" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "反射型跨站脚本",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost 가짜 수신단을 띄워, 받은 요청 본문과 헤더를 단언 함수로 되돌린다.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("요청 본문이 합법 JSON이 아님: %v\n원문: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("백링크 있으면 actionCard를 보내야 하는데 %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("백링크 유실: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("요약 메시지는 markdown을 보내야 하는데 %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "최근 30분") {
			t.Errorf("요약 본문에 시간 창이 없음: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent 「HTTP 200이지만 errcode 비0」 판정을 고정한다.
// errcode를 검사하지 않으면 전송 실패를 성공으로 기록함——중국 IM 플랫폼 공통의 함정.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode 비0이면 에러여야 함")
	}
	if !IsPermanent(err) {
		t.Fatalf("키워드 불일치는 구성 오류라 영구 실패로 표시해야 하는데 %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("에러 메시지에 플랫폼 에러코드가 있어야 하는데 %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("절단 후 합법 UTF-8이 아님——WeCom이 메시지 전체를 거부")
		}
	})
	// 충분히 긴 중국어 요약 묶음을 만들면 반드시 4096바이트 초과.
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("본문 %d바이트가 WeCom 상한 %d 초과", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("본문이 비어 있음")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009는 롤링 윈도우 레이트리밋이라 재시도 가능해야 하는데 %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000은 key 무효라 재시도로 자연 복구 안 됨, 영구 실패여야 하는데 %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("인터랙티브 카드를 보내야 하는데 %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high 등급은 orange 배색이어야 하는데 %v", header["template"])
		}
		// secret을 설정하면 서명 파라미터가 있어야 함, 아니면 Feishu가 19021로 거부.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("서명 파라미터 누락: %v", body)
		}
		// 카드 요소에 버튼 하나가 있어야 하고, 그 url은 취약점 상세를 가리킴.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("카드에 상세 페이지를 가리키는 버튼이 없음")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret 미설정 시 서명 파라미터가 없어야 하는데 %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML 파싱 모드를 써야 하는데 %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// 제목과 요약은 피검 대상/모델 출력에서 오며 신뢰할 수 없는 콘텐츠.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML 미이스케이프, 주입 존재: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔티티를 기대했으나 %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& 미이스케이프, %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429는 재시도 가능해야 하는데 %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403은 구성 문제라 영구 실패여야 하는데 %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// 이것이 기본 템플릿이 존재하는 이유: 제목에 따옴표와 줄바꿈이 있을 때, 순진한
	// `"title": "{{.Title}}"` 작성법은 모두 비합법 JSON을 만든다. {{json .}}만이 그렇지 않다.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 고위험] 登录处 "SQL注入" 风险` {
			t.Errorf("제목이 올바르게 복원되지 않음: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items 개수는 1이어야 하는데 %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "参数 id\n未过滤 导致注入" {
			t.Errorf("요약이 올바르게 복원되지 않음: %v", it["summary"])
		}
		// 수치는 문자열이 아니라 JSON 숫자여야 함(json:"...,string" 같은 작성법이 이 함정에 빠짐).
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id는 숫자여야 하는데 %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("커스텀 헤더 유실: %v", r.Header)
		}
		if body["msg"] != "3 条" {
			t.Errorf("커스텀 템플릿 렌더링 오류: %v", body["msg"])
		}
		if body["first"] != "漏洞1" {
			t.Errorf("range 추출 오류: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d 条" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("렌더 결과가 JSON이 아니면 영구 실패여야 함(템플릿 오작성, 재시도 무용)인데 %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("%d번째 구성은 거부돼야 하는데: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("이메일 조립 실패: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From 헤더 오류:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To 헤더 오류:\n%s", msg)
	}
	// 비 ASCII 제목은 RFC 2047 인코딩 필수, 아니면 클라이언트가 깨진 문자로 표시.
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("제목이 RFC 2047 인코딩 안 됨:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("제목 디코딩 불가: %v", err)
	} else if !strings.Contains(dec, "SQL注入") {
		t.Fatalf("제목 디코딩 후 내용 오류: %q", dec)
	}

	// 본문은 base64, 디코딩하면 합법 HTML이어야 함.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("이메일에 헤더/본문 구분자 없음")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("본문 base64 디코딩 실패: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("본문이 HTML이 아님: %.80s", html)
	}
	// 제목이 텍스트 위치에 원문 그대로 나타남: HTML 텍스트 내용의 큰따옴표는 합법 문자라 이스케이프 불필요.
	// 여기서 「원문 보존」을 단언하는 것은 나중에 누군가 따옴표 이스케이프를 한 겹 잘못 추가해
	// 중국어 따옴표가 &quot;로 표시되는 것을 막기 위함.
	if !strings.Contains(html, `"SQL注入"`) {
		t.Fatalf("제목의 따옴표는 텍스트 위치에서 원문 보존돼야 하는데: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection 이메일 본문이 실제로 막아야 할 주입을 커버:
// 취약점 제목과 요약은 피검 대상과 모델 출력에서 오는 신뢰 불가 콘텐츠. 텍스트 위치는
// & < >를 반드시 이스케이프(아니면 태그 주입 가능), 속성 위치는 따옴표도 이스케이프 필수(아니면 href 닫기 가능).
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("제목 미이스케이프, 태그 주입 가능: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("이스케이프된 엔티티를 기대: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& 와 > 미이스케이프: %s", html)
	}
	// 백링크는 관리자가 설정 가능한 public_base_url이라 신뢰도는 높지만, 속성 위치는 여전히
	// 따옴표를 이스케이프해야 함——아니면 따옴표가 든 주소가 href를 닫고 이벤트 핸들러를 주입함.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 속성이 올바르게 이스케이프 안 됨: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("속성 위치의 따옴표는 이스케이프돼야 하는데: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("%s 헤더를 찾을 수 없음", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 검증 에러는 설정자에게 직접 표시되므로 무엇이 빠졌는지 명확히 말해야 하며, 막연한 「구성 무효」가 아니어야 함.
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "포트"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "수신자"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("채널 %s 미등록", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s 구성 %v는 검증 실패해야 함", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s의 에러 메시지는 %q를 언급해야 하는데 %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification SMTP 4xx/5xx의 의미 구분을 고정한다.
// 4xx도 영구 실패로 판정하면, 그레이리스트를 켠 메일 서버가 모든 푸시를 첫
// 시도 후 failed로 떨어뜨림 —— 그레이리스트야말로 자동 재시도가 가장 필요한 상황인데.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 응답 코드를 못 얻으면 「재시도 가능」으로 처리: 한 번 더 시도할지언정, 일시적
		// 장애일 수 있는 것을 사망 판정하지 않음.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("수신자 거부됨", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("응답 %q: permanent=%v 기대 %v", tc.reply, tc.permanent, got)
		}
		// 어떻게 분류하든 원문은 사용자 조사용으로 보존해야 함.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("응답 %q의 원문이 버려짐: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 여섯 채널은 하나도 빠지면 안 됨——하나 적으면 UI 드롭다운에서 조용히 사라짐.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("채널 수는 %d여야 하는데 %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("채널 %s 미등록", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("채널 %s의 Kind()가 등록 키와 불일치", k)
		}
	}
	if ValidKind("nope") {
		t.Error("미등록 유형은 검증을 통과하면 안 됨")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("영구 실패로 인식해야 함")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("에러 메시지는 하위를 투과해야 하는데: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil)은 nil을 반환해야 함")
	}
	if IsPermanent(nil) {
		t.Fatal("nil은 영구 실패가 아님")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
