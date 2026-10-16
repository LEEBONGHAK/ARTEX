package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel 딩톡 커스텀 봇을 구현한다.
//
// 플랫폼 특성（여기 구현 취사를 결정함）:
//   - 단일 봇 레이트 리밋 20건/분, 초과 발송은 조용히 폐기됨（HTTP는 여전히 200일 수 있음）,
//     그래서 레이트 리밋은 반드시 클라이언트에서 해야 한다, DefaultRatePerMin 참조.
//   - 보안 설정 3택1:서명 추가 / 커스텀 키워드 / IP 화이트리스트. 서명 추가는 유일하게 의존하지 않는
//     메시지 내용에 의존하지 않는 방안이라, 서명 추가만 지원한다（셋 다 안 켠 맨 webhook도 지원）.
//   - 성공/실패 모두 HTTP 200을 반환하고, body의 errcode로 구분한다——errcode를 검사하지 않으면 errcode
//     전송 실패를 성공으로 기록하게 된다.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// 딩톡의 Webhook 주소에는 access_token이 들어 있어, 그 자체가 자격 증명이므로, 전체를 마스킹한다.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 목적지는 딩톡의 Webhook 주소 자체다；주소 변경 시 반드시 새 주소에 대해 서명 키를 다시 표명해야 한다.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소 누락")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소 무효: %w", err)
	}
	return nil
}

// Send 메시지를 한 번 전송한다. 백링크가 있고 단건일 때는 ActionCard（버튼 포함）를 쓰고, 아니면 markdown을 쓴다.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// 딩톡 markdown 본문은 명확한 바이트 상한이 없지만, 그래도 상한 보호를 해, 증거 필드의 비정상 팽창을 피한다.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "상세 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// 딩톡은 비즈니스 에러를 200 응답 안에 숨긴다.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("딩톡 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 301000은 서명 검증 실패、310000은 키워드 불일치——모두 설정 오류라,
		// 재시도해도 자가 치유되지 않는다.
		return 0, Permanent(fmt.Errorf("딩톡 반환 오류 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL 공식 서명 규칙에 따라 webhook에 timestamp와 sign 파라미터를 추가한다.
//
// 규칙:서명 대상 문자열 = timestamp + "\n" + secret, HMAC-SHA256의 **키도 secret**이다,
// 결과를 base64 후 URL 인코딩한다. timestamp는 밀리초다. secret이 비면 원본 그대로 반환하고,
// 서명을 켜지 않은 봇을 지원하기 위함이다.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err를 그대로 전달하지 않는다:url.Parse의 에러 텍스트에 전체 주소가 들어 있다（access_token 포함）.
		return "", fmt.Errorf("Webhook 주소 파싱 실패: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL 주소 사용 가능、프로토콜 지원을 검증하고, 리터럴 IP 대상에 대해 내부망 판단을 한다.
//
// 두 가지 주의점:
//
//  1. ****에러 정보는 반드시 마스킹**. url.Parse 자체가 반환하는 것은 *url.Error이고, 그 Error()는
//     ****완전한 원본 주소**를 지닌다, 그런데 이 기능 이들의 주소에는 자격 증명이 박혀 있다（딩톡 access_token、
//     위챗워크 key、Telegram의 bot token、페이슈 hook id）. 예전엔 여기서 바로 `return err`를 했는데,
//     그래서「주소 형식 부적법」이라는 에러가 자격 증명을 가지고 나가, 테스트 인터페이스의 400 응답、
//     매 전송 저장 시의 last_error、서버 측 로그와 전송 히스토리 인터페이스로 흘러간다.
//
//  2. ****리터럴 IP는 바로 내부망으로 판정**, 도메인은 다이얼 단계 판정에 맡긴다（blockInternalDial이야말로 최종
//     적용 지점이고, DNS 리바인딩도 커버할 수 있다）. 여기서 한 번 하는 것은 설정 저장 시 바로 안내를 받게 하기 위함이고,
//     첫 전송 실패까지 기다리는 게 아니다.
//
// 프로토콜 제한은 방어적이다:file:///gopher:// 같은 것은 http.Client가 예상 밖의
// 동작을 일으킨다（이미 scheme 검사로 막혔지만, 이 면을 열어둘 이유가 없다）.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없음（%s）", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http/https만 지원, 받음: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("호스트명 누락")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬 머신/링크 로컬 주소 %s로의 전송 거부（로컬 머신 서비스로 전송이 꼭 필요하면 %s=1 설정）", ip, AllowLocalTargetsEnv)
	}
	return nil
}
