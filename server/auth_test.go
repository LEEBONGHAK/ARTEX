package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting 은 "키 없음"과 "읽기 오류"에 대한 반환값이 error 하나만 다르다: 두 경우 모두
// value 는 빈 문자열이다. 비밀번호 관련 handler 가 error 를 "아직 비밀번호 미설정"으로
// 간주해 버리면, 데이터베이스가 흔들리는 동안 초기화 입구를 활짝 열어 준다 — authInit 은
// 미인증 요청이 기존 관리자 비밀번호를 덮어쓰도록 통과시키고, authStatus 는 프런트엔드를
// 곧장 /setup 으로 보내 바로 그 일을 하게 만든다.
//
// 이 두 테스트는 handler 의 커넥션 풀을 닫아 읽기 실패를 만들고, 두 곳 모두 fail closed 됨을 단언한다.
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// 풀을 닫아, 이후 GetSetting 이 sql.ErrNoRows 가 아니라 error 를 반환하게 한다.
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (읽기 실패를 미초기화로 간주하면 사용자를 /setup 으로 보내 비밀번호를 덮어쓰게 됨); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("읽기 실패 시 initialized 를 응답해서는 안 됨, 받은 값 %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (읽기 실패 시 통과시키면 미인증 요청이 기존 비밀번호를 덮어씀); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("읽기 실패 시 token 을 발급해서는 안 됨: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"빈 문자열", "", true},
		{"7자", "1234567", true},
		{"8자", "12345678", false},
		{"한자 8개, 바이트 수가 아니라 문자 수로 계산", "密码密码密码密码", false},
		{"한자 3개는 9바이트지만 문자는 3개뿐", "密码强", true},
		{"72바이트", strings.Repeat("a", 72), false},
		{"73바이트, bcrypt 상한 초과", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
