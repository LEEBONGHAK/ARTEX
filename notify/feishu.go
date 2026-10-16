package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel 페이슈（Lark 포함）커스텀 봇을 구현하며, 인터랙티브 카드를 쓴다.
//
// 플랫폼 특성:
//   - 서명 알고리즘이 딩톡과 **다르고**, 매우 틀리기 쉽다, feishuSign 주석 참조.
//   - 딩톡과 마찬가지로 비즈니스 에러를 HTTP 200의 body에 넣는다（code != 0）.
//   - 카드 header는 색상 템플릿을 지원하고, 레벨로 색을 매핑해, 메시지 목록에서 심각도를 한눈에 보게 한다.
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// 페이슈 커스텀 봇은 약 5회/초, 환산하면 100회/분이다.
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook 주소 끝 구간이 곧 봇의 유일 식별자이고, 자격 증명에 속한다.
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 같은 이치:Webhook 주소 변경 시 반드시 새 주소에 대해 서명 키를 다시 표명해야 한다.
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소 누락")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소 무효: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 서명 파라미터는 메시지와 같은 계층이고, secret을 설정했을 때만 나타난다.
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 일부 버전의 페이슈 hook은 이 필드명 세트를 쓰므로, 함께 호환한다.
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("페이슈 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("페이슈 반환 오류 %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("페이슈 반환 오류 %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign 페이슈 공식 규칙에 따라 서명을 계산한다.
//
// 여기가 특히 함정에 빠지기 쉽다:공식 샘플은
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// 즉 **key = timestamp + "\n" + secret, message는 비움**이고, 직관적인
// 「key=secret, message=stringToSign」이 아니다——그것이 바로 딩톡의 알고리즘이다. 양쪽 알고리즘이 딱 반대라,
// 다른 쪽 구현을 보고 쓰면 반드시 서명 검증에 실패한다（19021 보고）.
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate 취약점 레벨을 카드 header 배색 템플릿에 매핑한다.
// 알 수 없는 레벨은 grey를 쓴다——blue를 쓰지 않는 것은 low와 혼동되지 않게 하기 위함이다.
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes 카드 내용의 보수적 상한이다. 페이슈는 카드에 크기 제한이 있어, 초과하면 전체가 거부된다；
// 공식 상한보다 명백히 낮은 값을 취하고, JSON 래핑 오버헤드도 계산에 넣는다.
const feishuMaxCardBytes = 24000

// feishuCard 인터랙티브 카드를 구성하고, 카드와 **실제로 쓴 항목 수**를 반환한다.
// kept 의 용도는 markdownBody와 같다:실제로 카드에 들어간 항목만 송달됨으로 표시돼야 한다.
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 먼저 건 단위로 패킹한 뒤 헤더를 조합한다:헤더에「나머지 N건은 다음 메시지에 계속」을 써야 하고,
		// N N은 반드시 실제로 담은 건수에서 와야 한다.
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("플랫폼에서 전체 보기", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("상세 보기", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines 단일 취약점의 lark_md 본문을 렌더링한다.
//
// lark_md markdown과 동족의 텍스트 형식이라, 똑같이 링크와 강조를 파싱하므로, 외부에서 온
// 필드는 일률적으로 markdownText를 거친다（단일 행화 + 이스케이프）——아니면 취약점 제목 하나가
// 페이슈에서 클릭 가능한 외부 링크가 된다.
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**상태 변경%s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**유형%s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**자산%s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**요약%s", s)
		}
	}
	return out
}

// feishuBatchLine 요약 카드의 한 건을 렌더링한다.
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
