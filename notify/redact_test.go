package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 이 파일은 불변식 테스트: 채널 구현에서 나오는 **어떤** 에러 텍스트도 자격 증명을 담아선 안 됨.
//
// 왜 파일을 따로 뽑았나: 초기 채널 케이스는 성공 경로와 플랫폼 비즈니스 에러만 커버하고,
// 전송 계층 실패는 전혀 보지 않았음. 그런데 하필 전송 계층 에러(연결 거부/DNS 실패/타임아웃)가 가장 위험함——
// http.Client.Do가 반환하는 *url.Error는 **전체 URL**을 에러 텍스트에 박아 넣고, 이 기능의
// 이 채널들은 자격 증명이 URL 안에 있음. 자격 증명이 이 경로를 따라 네 출구로 흘러감:
//
//	notification_deliveries.last_error  → 평문 DB 저장
//	GET /api/notify/deliveries 응답     → 채널 구성 마스킹 우회, 브라우저에 그대로 표시
//	서버 로그                           → 자주 외부로 반출·보존됨
//	테스트 전송 인터페이스의 502 응답   → 프런트에 바로 튀어나옴
//
// 그래서 여기서는 함수 하나만 테스트하는 게 아니라, 채널마다 반드시 실패하는 요청을 실제로 한 번 보내,
// 에러 텍스트에서 그 자격 증명을 찾을 수 없음을 단언한다.

// credentialCases 「자격 증명이 URL 안에」인 모든 채널 형태를 커버:
// DingTalk/WeCom은 query, Feishu는 경로 끝부분, Telegram은 경로 중간.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk access_token은 query에",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom key는 query에",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu hook id는 경로 끝부분에",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token은 경로 중간에",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk 서명 키",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken 실제 자격 증명일 리 없는 센티넬 값, 에러 텍스트에서 이것을 검색하는 데 쓴다.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials 핵심 불변식.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 반드시 실패하는 대상: 127.0.0.1:1은 아무도 리슨 안 함, 연결 거부 경로를 탐.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "누출 프로브"}},
			})
			if err == nil {
				t.Fatal("도달 불가 주소는 에러여야 함")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath 영구 실패 분기를 커버:
// URL 검증 실패、플랫폼 비즈니스 에러 등도 에러 텍스트를 외부로 내보내므로 마찬가지로 자격 증명을 담으면 안 됨.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 주소에 자격 증명이 있으나 형식이 비합법 → validateHTTPURL / url.Parse 분기를 트리거.
		{"DingTalk 주소 비합법", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"WeCom 주소 비합법", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Feishu 주소 비합법", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API 주소 비합법", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"범용 Webhook 주소 비합법", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("비합법 구성은 에러여야 함")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("에러 텍스트가 자격 증명 %q를 누출:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, 기대 %q", in, got, want)
		}
		// 마스킹 결과 자체는 원 주소의 어떤 경로/쿼리 조각도 다시 포함하면 안 됨.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("마스킹 후에도 경로/쿼리 조각 %q 포함: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 파싱 불가 입력은 절대 원문을 되비치지 않음.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("파싱 불가 입력 %q가 %q로 되비침", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL *url.Error라는 구체 타입을 직접 겨냥:
// http.Client.Do의 반환 타입이자 누출의 1차 현장.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("조사 편의를 위해 host는 보존해야 하는데 %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("조사 편의를 위해 하위 원인은 보존해야 하는데 %q", got)
	}
	// Op도 보존해야 함(POST인지 GET인지가 조사에 의미 있음).
	if !strings.Contains(got, "Post") {
		t.Errorf("조작명을 보존해야 하는데 %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback 폴백 경로: *url.Error가 아닌 커스텀 에러
// (예: 리다이렉트 정책이 반환한 에러) 안의 주소도 마찬가지로 제거돼야 함.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("크로스 호스트 리다이렉트 거부(a.example → http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("주소를 마스킹 형태로 바꿔야 하는데 %q", got)
	}
	// 주소를 포함하지 않는 텍스트는 원문 보존.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("주소를 포함하지 않는 텍스트는 바뀌면 안 됨")
	}
}

// TestCrossHostRedirectRefused 「자격 증명이 URL 안에 + 크로스 호스트 리다이렉트 따라감 = 자격 증명 넘겨줌」을 커버.
// httptest의 두 서버가 127.0.0.1의 다른 포트에서 리슨, 포트가 다르면 Host가 달라
// 딱 크로스 호스트 리다이렉트를 구성.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("크로스 호스트 리다이렉트는 거부돼야 함")
	}
	if hit {
		t.Fatal("리다이렉트 대상이 접근됨——자격 증명이 리다이렉트를 따라 외부로 누출됨")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed 역방향 케이스: 동일 호스트 리다이렉트(예: 끝에 슬래시 추가)는 여전히 작동해야 함,
// 아니면 정상 워크플로까지 막아버림.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 동일 호스트、동일 포트 리다이렉트.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("동일 호스트 리다이렉트는 거부되면 안 됨: %v", err)
	}
}
