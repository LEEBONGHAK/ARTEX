package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit 위챗워크 그룹 봇 markdown content의 하드 상한이다（바이트, 문자 아님）.
// 여섯 채널 전체에서 가장 빡빡한 제한이고, TruncateBytes가 존재하는 주된 이유이기도 하다.
const weComMarkdownLimit = 4096

// weComChannel 위챗워크 그룹 봇을 구현한다.
//
// 플랫폼 특성:
//   - 유일하게 URL의 key로 인증하고, 서명 추가를 지원하지 않는다——그래서 webhook 주소 자체가 전부 자격 증명이다.
//   - markdown content markdown content 상한 4096 **바이트**, 초과 길이는 전체가 거부된다（절단 아님）. 중국어 3바이트/자,
//     본문에 천여 자만 쓸 수 있다는 뜻이라, 반드시 클라이언트에서 절단해야 한다.
//   - 레이트 리밋 20건/분, 마찬가지로 클라이언트 레이트 리밋으로 받아낸다.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// 위챗워크는 Webhook 한 곳만 자격 증명이고（URL의 key）, 서명 추가를 지원하지 않는다——
// 전체 주소가 곧 전부 자격 증명이라, 마스킹할 다른 필드가 없다.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// 위챗워크는 Webhook 한 필드만 있고, 그것이 목적지이자 자격 증명이라, 「주소 변경 후 남은 자격 증명」이라는 게 없다.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소 누락")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소 무효: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 요약 배치가 매우 길 수 있어（50건 × 건당 한 줄 + 접두사）, 4096바이트를 쉽게 초과한다.
	// 절단을 여기서 하고 플랫폼 에러에 기대지 않는다:거부는 이 배치 전체를 잃는다는 뜻이고, 절단은 적어도 앞 몇 건은 송달한다.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("위챗워크 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 45009는 인터페이스 호출이 제한을 초과함——플랫폼의 레이트 리밋 윈도우가 롤링하므로, 백오프 후 재시도가 유효하다,
		// 그래서 명시적으로 재시도 가능으로 분류한다. 여기까지 왔다는 것은 클라이언트 rate_per_min이 너무 공격적으로 설정됐다는 뜻이라,
		// 재시도는 폴백일 뿐이고, 진짜 해결법은 그 채널의 레이트 리밋 값을 낮추는 것이다.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("위챗워크 레이트 리밋 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 93000은 webhook key 무효——영구 실패, 재시도해도 자가 치유되지 않는다.
		return 0, Permanent(fmt.Errorf("위챗워크 반환 오류 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
