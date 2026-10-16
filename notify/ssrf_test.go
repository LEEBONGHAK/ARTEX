package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 이 파일은 관련된 두 가지 강화를 커버:
//   ① 전송 주소가 서버를 발판 삼아 내부망 / 클라우드 메타데이터를 치면 안 됨(SSRF)
//   ② 주소 검증 에러 메시지가 주소 안의 자격 증명을 내보내면 안 됨
//
// 테스트 환경에 대해: 이 패키지의 많은 케이스가 127.0.0.1의 httptest 가짜 수신단을 쓰고, 가드는 기본적으로
// 이를 막는다. 그래서 TestMain에서 AllowLocalTargetsEnv를 일괄로 켜고, 아래 각 SSRF 케이스는
// 명시적으로 이를 비워 **기본 거부** 동작을 단언한다.

func TestMain(m *testing.M) {
	// 일반 케이스가 로컬 가짜 수신단에 연결되게 함; SSRF 케이스는 스스로 임시로 비움.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault SSRF 방어의 핵심 단언:
// 기본 구성에서 루프백 주소로의 전송은 반드시 **연결 계층**에서 거부돼야 함.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구 끄기 = 기본 동작
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("기본값은 루프백 주소로의 전송을 허용하면 안 됨")
	}
	if hit {
		t.Fatal("요청이 로컬 서비스에 도달함——가드가 작동 안 함")
	}
	// 에러 메시지는 사용자가 어떻게 열어주는지 안내해야 함(로컬 SMTP 중계는 합법 구성).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지는 명시적으로 여는 방법을 설명해야 하는데: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn 역방향 케이스: 명시적으로 켠 뒤에는 반드시 쓸 수 있어야 함,
// 아니면 로컬 postfix / 내부망 중계 같은 합법 배포가 일괄로 폐기됨.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("명시적으로 연 뒤에는 전송 가능해야 함: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // 클라우드 메타데이터 엔드포인트——이 함수가 존재하는 주된 이유
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped 형태는 복원 후 판정해야 함, 아니면 우회 통로
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s는 거부돼야 함", s)
		}
	}
	// RFC1918 사설망 **의도적 허용**: 내부망 자체 구축 Mattermost / SMTP 중계는 흔한 합법 용도.
	// 이 단언은 이 취사선택을 고정한다——나중에 누군가 사설망 판정을 슬쩍 추가하면 여기서 실패해,
	// 의식적인 결정을 강제한다(일괄로 배포를 조용히 폐기하는 대신).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s는 통과돼야 함(사설망은 흔한 합법 전송 대상)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets 구성 단계의 사전 안내를 커버:
// 리터럴 IP는 저장 시점에 거부돼야지 첫 전송 실패를 기다리면 안 됨.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s는 구성 단계에서 거부돼야 함", raw)
		}
	}
	// 공인 주소와 사설망 주소는 평소대로 통과(사설망은 다이얼 단계에 맡김, 거기선 안 막음).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s는 검증을 통과해야 함: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials 감사에서 지적된, 지난번 내가 누락한 분기.
//
// url.Parse가 **실패**하면 *url.Error를 반환하고, 그 Error()는 완전한 원본 주소를 포함. 지난번엔
// http.Client.Do의 반환 에러만 마스킹하고 여기를 놓침; 당시 보강한 「영구 실패 경로」 케이스
// (file://、gopher://、ftp://)는 사실 전부 url.Parse가 성공해 scheme 분기를 타므로,
// 전부 초록이어도 이 경로의 안전을 증명 못 함——가짜 보증.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 비합법 퍼센트 이스케이프
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // 포트가 숫자 아님
		"http://[::1?access_token=" + leakProbeToken,                  // 괄호 짝 안 맞음
	}
	for _, raw := range cases {
		// 먼저 이 입력이 **실제로** url.Parse를 실패시키는지 확인. 이 단계를 안 하면 케이스가
		// 모르는 사이 다른 분기를 탈 수 있음(지난번 가짜 보증이 이렇게 생김).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q는 파싱 실패해야 함, 아니면 이 케이스가 목표 분기를 커버 못 함", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q는 검증 실패해야 함", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// 채널 계층의 래핑도 주소를 내보내지 않는지 확인.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("비합법 주소는 검증 실패해야 함")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault SMTP 채널의 다이얼 가드를 커버.
//
// 이메일 채널은 예전에 벌거벗은 net.Dialer를 썼고, SSRF 방어 전체에서 유일한 구멍이었음: host를
// 169.254.169.254나 127.0.0.1로 넣으면 바로 연결됐고, smtp.NewClient 핸드셰이크 실패 시
// 상대가 반환한 그 줄을 에러에 담아 last_error를 통해 전송 이력 인터페이스로 되비침——바로 다른 채널들이
// 이미 막은 반 맹목 읽기 원시형; 「연결 거부 vs 타임아웃」의 소요 시간 차로 포트를 탐지할 수도 있음.
//
// 이 패키지의 TestMain은 AllowLocalTargetsEnv를 전역으로 켰고(많은 케이스가 127.0.0.1의
// 가짜 수신단을 씀), 그래서 이 케이스는 스스로 이를 비워야 함——아니면 가드가 있든 없든 통과해버림,
// 이것이 바로 구멍이 애초에 어떤 테스트에도 발견되지 않은 이유.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구 끄기 = 기본 동작
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("기본값은 이메일을 루프백 주소로 전송하는 것을 허용하면 안 됨")
	}
	// 연결 자체가 수립되면 안 됨: 가드가 Control 훅에서 막아, EHLO는 영원히 못 나감.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 세션이 수립됨——가드가 작동 안 함")
	}
	// 에러 메시지는 사용자가 어떻게 열어주는지 안내해야 함(로컬 postfix 중계는 합법 구성).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("거부 메시지는 명시적으로 여는 방법을 설명해야 하는데: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn 짝을 이루는 역방향 케이스: 명시적으로 켠 뒤에는
// 정상 전송돼야 함. 내부망 자체 구축 SMTP / 로컬 중계는 매우 흔한 배포라 가드가 일괄 차단하면 안 됨.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("명시적으로 연 뒤 로컬 SMTP는 전송 가능해야 함: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO를 못 봄——세션이 실제로 수립 안 됨")
	}
}
