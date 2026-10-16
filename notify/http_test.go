package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 이 파일은 doJSON의 HTTP 계층 에러 등급 분류를 커버한다.
//
// 왜 따로 테스트하나: 각 채널 어댑터는 플랫폼 자체의 비즈니스 에러코드(DingTalk errcode、
// Feishu code、Telegram ok 필드)만 다루고, **HTTP 계층** 등급 분류는 doJSON이 통일해서 하므로,
// 둘은 독립된 두 방어선이다. 이게 없으면, 503을 반환하는 중계 게이트웨이가 영구 실패로 간주되어
// 재시도를 바로 포기함; 403은 재시도 가능으로 간주되어 괜히 세 번 백오프함.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 성공은 에러 아님", 200, false},
		{"429 레이트리밋 재시도 가능", 429, false},
		{"408 요청 타임아웃 재시도 가능", 408, false},
		{"500 서버 에러 재시도 가능", 500, false},
		{"502 게이트웨이 에러 재시도 가능", 502, false},
		{"503 서비스 불가 재시도 가능", 503, false},
		{"400 파라미터 에러 영구 실패", 400, true},
		{"401 인증 실패 영구 실패", 401, true},
		{"403 접근 금지 영구 실패", 403, true},
		{"404 주소 없음 영구 실패", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx는 에러가 아니어야 하는데: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("비 2xx는 에러여야 함")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d의 permanent 판정 오류: 기대 %v 실제 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// 상태 코드가 반드시 에러에 나와야 함, 아니면 사용자가 자기가 잘못 설정한 건지 상대가 죽은 건지 판단 못 함.
			// Go의 영문 StatusText가 아니라 숫자를 단언: 이 패키지 문안은 한국어라
			// (프로젝트 다른 부분과 일치), 숫자가 언어 무관하고 안정적으로 단언 가능한 부분.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("에러 메시지에 HTTP 상태 코드 %d가 있어야 하는데 %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet snippet을 커버: 상대가 반환한 에러 설명을 가져와야 함,
// 아니면 사용자는 「실패함」만 알고 상대가 왜 거부했는지 모름.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("에러여야 함")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("에러 메시지는 상대의 설명을 가져와야 하는데 %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded snippet의 형태를 제약:
// 상대 응답이 그대로 last_error 열과 프런트 테이블로 들어가므로, 여러 줄/초장문은 레이아웃과 페이로드를 망침.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 줄바꿈、탭、5000자 초장문 내용이 든 응답.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("에러여야 함")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("에러 메시지는 한 줄로 압축돼야 하는데 %q", msg)
	}
	// snippet 상한 200자 + 고정 접두, 총량은 원본 응답보다 훨씬 작아야 함.
	if len(msg) > 400 {
		t.Errorf("에러 메시지가 너무 긺(%d바이트), snippet으로 절단돼야 함: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse 읽기에 상한이 있음을 확인: 상대가 비정상으로 초대용량 내용을 반환할 때
// 전체 응답을 메모리에 읽어선 안 됨(전송 이력에 매 건 last_error를 한 벌씩 저장).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("에러여야 함")
	}
	if len(err.Error()) > 400 {
		t.Errorf("초대용량 응답은 길이 제한 읽기 + 절단돼야 함, 에러 메시지 길이 %d", len(err.Error()))
	}
}
