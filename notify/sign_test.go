package notify

import (
	"net/url"
	"testing"
	"time"
)

// 서명 기준값은 OpenSSL로 독립 산출, 이 패키지 자체 구현으로 생성한 게 아님——
// 아니면 「코드가 안 변함」만 증명할 뿐 「알고리즘이 맞음」은 증명 못 함.
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("서명 실패: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("산출 주소 파싱 불가: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("서명 불일치\n기대 %s\n실제 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("타임스탬프는 밀리초이고 원문 그대로여야 하는데 %q", q.Get("timestamp"))
	}
	// 기존 query 파라미터(access_token)가 서명으로 덮이면 안 됨.
	if q.Get("access_token") != "tok" {
		t.Errorf("기존 query 파라미터 유실, %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("서명 불일치\n기대 %s\n실제 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer 두 플랫폼의 알고리즘 차이를 고정한다. 둘은 마침 서로의 파라미터 순서가 반대라
// (DingTalk key=secret, Feishu key=서명 대상 문자열), 다른 쪽을 베끼면 반드시 검증 실패,
// 이 케이스는 향후 리팩터링이 둘을 같은 함수로 합치지 않도록 보장한다.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk과 Feishu 서명이 동일, 둘 중 한 쪽의 알고리즘 구현이 틀림")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 서명 미활성 봇: timestamp/sign 파라미터를 임의로 추가하면 안 됨.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret 미설정 시 주소가 바뀌면 안 되는데 %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q는 수락돼야 하는데: %v", s, err)
		}
	}
	// file:// 류는 통과시키면 안 됨——http.Client의 처리가 예상 범위를 벗어남.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q는 거부돼야 함", s)
		}
	}
}
