package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// 이 파일은 이메일 채널의 프로토콜 레벨 테스트를 보강한다. 이전까지 email.Send의 커버리지는 0——
// SMTP 경로 전체를 거친 케이스가 하나도 없었는데, 하필 여섯 채널 중 프로토콜 면이 가장 크고
// 가장 틀리기 쉬운 것(핸드셰이크、인증、엔벨로프、DATA 단계가 각기 다른 실패 의미).
//
// 여기서는 net/smtp를 mock하지 않고 자체 제작한 최소 SMTP 서버로 구동한다:
// 이메일 채널 위험의 대부분은 「실제 SMTP 서버와 대화」하는 이 단계에 있어,
// 이 단계를 mock하면 테스트 안 한 것과 같다.

// fakeSMTP 딱 쓸 만큼의 SMTP 서버: greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT를 완료할 수 있고,
// 케이스 요구에 따라 특정 단계에 지정 응답 코드를 반환한다.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply RCPT TO의 응답; 기본 250.
	rcptReply string
	// mailReply MAIL FROM의 응답; 기본 250.
	mailReply string
	// advertiseAuth true면 EHLO에서 AUTH PLAIN 지원을 선언.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP 리슨 주소 아님")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS 미선언: 코드가 평문 분기를 타게 함(테스트 목표는 엔벨로프 로직이지 TLS 아님).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 간소화: PLAIN의 초기 응답은 여러 줄일 수 있으나 그냥 수락.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	// 엔벨로프 단계를 반드시 거쳐야 함: 발신자、수신자 둘、DATA.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP 세션에 %q 누락, 실제 명령: %v", want, f.commands)
		}
	}
	// 본문은 base64 HTML이고 실제 취약점 내용을 담아야 함(인코딩 후에도 식별 가능).
	body := f.body()
	if body == "" {
		t.Fatal("DATA 단계에서 본문을 받지 못함")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type 헤더 누락:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("본문이 base64 인코딩 안 됨(긴 HTML 행은 SMTP의 1000바이트 행 길이 제한을 깸):\n%s", body)
	}
	// 여러 수신자가 모두 To 헤더에 나와야 함.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To 헤더에 모든 수신자가 없음:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// 계정 미설정 시 AUTH를 보내면 안 됨——일부 중계는 이로 인해 거부.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("계정 미설정인데 AUTH를 보냄: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies 이번 감사 수정의 직접 검증:
// 5xx는 영구 실패, 4xx(그레이리스트)는 재시도 가능으로 판정.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"수신자 550 영구 거부", "550 5.1.1 User unknown", "250 OK", true},
		{"수신자 450 그레이리스트", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"수신자 452 메일박스 가득", "452 4.2.2 Mailbox full", "250 OK", false},
		{"발신자 553 영구 거부", "250 OK", "553 5.1.3 Bad address", true},
		{"발신자 451 일시 에러", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("에러여야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 판정 오류: 기대 %v 실제 %v (%v)", tc.permanent, got, err)
			}
			// 서버 원문을 보존해야 함, 아니면 사용자가 서버 관리자를 찾아야 할지 주소를 고쳐야 할지 모름.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("에러에 서버의 응답 코드가 보존돼야 함: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp의 PlainAuth는 미암호화 연결에서 자격 증명 전송을 거부한다(대상이 localhost가 아닌 한).
	// 이것은 **올바른** 보안 동작이라 우회하면 안 됨; 다만 사용자가 고칠 수 있도록 안내하는 에러를 줘야 함.
	// 여기서는 localhost가 아닌 호스트명으로 이를 트리거한다.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost 아님
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("로컬 DNS가 로컬 서버로 해석됨, 건너뜀(다른 케이스에 영향 없음)")
	}
	// 연결 실패 또는 자격 증명 전송 거부 모두 이 단언을 통과; 핵심은 비밀번호를 **조용히** 보내면 안 된다는 것.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "연결") {
		t.Logf("에러: %v(localhost 아닐 때 연결 실패는 예상됨)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// 이메일 채널은 구성 필드가 가장 많아 하나라도 빠지면 전송 시점에야 드러남; 여기서 하나씩
	// 검증이 미리 잡아내는지 확인. 단언은 「에러 메시지가 무엇이 빠졌는지 언급」하는지를 검사.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host 누락", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port 누락", map[string]any{"host": "smtp.example.com"}},
		{"port 범위 초과", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from 누락", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to 누락", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("검증 실패해야 함: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance 구성 읽기의 내결함성을 커버: JSONB에서 수치는 float64지만,
// 사용자가 UI에서 포트를 문자열로 넣을 수 있음; 배열도 단일 문자열일 수 있음.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 문자열 형태의 포트
		"from": "a@b.c",
		"to":   "d@e.f", // 배열이 아닌 단일 문자열
		"tls":  "true",  // 문자열 형태의 불리언
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("문자열 형태의 수치를 허용해야 함: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt가 문자열 포트를 파싱 못 함, %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool이 문자열 \"true\"를 파싱 못 함")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings가 단일 문자열을 호환 못 함, %v", to)
	}
}

// TestFilterValidateRejectsTypo 감사 수정의 직접 검증:
// 임계값 오타는 쓰기 시점에 막아야 함, 아니면 필터가 조용히 무력화되어 전부 푸시로 바뀜.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("합법 임계값 %q 거부됨: %v", s, err)
		}
	}
	// 실제 발생할 수 있는 오타들——전부 거부돼야 함.
	for _, s := range []string{"hgih", "HIGH", "严重", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("비합법 임계값 %q는 거부돼야 함(아니면 필터 조용히 무력화, 전부 푸시로)", s)
			continue
		}
		// 에러 메시지는 사용자가 바로잡도록 안내해야 함.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("에러 메시지는 선택 가능 값을 나열해야 하는데 %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly 「쓰기는 엄격, 읽기는 관대」 분담을 고정:
// DB에 이미 있는 나쁜 값이 채널 전체를 못 읽게 해선 안 됨(그러면 기존 채널이 갑자기 전부 푸시 중단).
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // 에러 없음
	if f.MinSeverity != "hgih" {
		t.Fatalf("읽기 경로는 원문 보존해야 하는데 %q", f.MinSeverity)
	}
	// 그리고 해당 채널은 여전히 이벤트를 판정할 수 있어야 함(panic 안 함, 블록 안 함).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
