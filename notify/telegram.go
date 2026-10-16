package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit Telegram sendMessage의 text 필드 상한이다（문자 수）.
const telegramTextLimit = 4096

// telegramChannel Telegram Bot API를 구현한다.
//
// 플랫폼 특성:
//   - 인증이 전부 URL path에 있다（/bot<token>/sendMessage）, 서명 추가 불필요.
//   - MarkdownV2가 아니라 HTML 파싱 모드를 쓴다:MarkdownV2는 다음을 이스케이프해야 함: `_*[]()~`>#+-=|{}.!`
//     총 18개 문자, 하나 빠지면 전체 메시지가 거부된다；HTML은 & < > 셋만 이스케이프하면 된다.
//   - 비즈니스 에러도 마찬가지로 HTTP 200 안에 숨어, ok 필드로 판단한다.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram 단일 채팅 약 1건/초、그룹 20건/분. 보수적 값을 취한다.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token Bot Token은 완전한 자격 증명이다；chat_id는 수신자일 뿐, 비밀이 아니다（그것을 얻어도 Token 없이는 메시지를 못 보냄）.
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url base_url은 Token이 어느 API 엔드포인트로 발송될지 결정한다（예: 자체 구축 리버스 프록시）, 그것을 바꾸면 반드시 Token을 다시 표명해야 한다.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token 누락")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID 누락")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API 주소 무효: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram 응답 파싱 실패: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 레이트 리밋이고, 백오프 후 재시도가 유효하다；나머지（400 파라미터 오류、401 token 오류、403 차단됨、
	// 404 chat 404 chat 없음）는 모두 설정 문제라, 재시도해도 자가 치유되지 않는다.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 레이트 리밋: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 반환 오류 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint sendMessage 주소를 조합한다. base_url이 비면 공식 API를 쓰고,
// 비어있지 않으면 자체 구축 Bot API 리버스 프록시에 쓴다（중국 네트워크 환경의 흔한 요구）.
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err를 그대로 전달하지 않는다:주소에 Bot Token이 들어 있고, 이때는 addr조차 되비쳐선 안 된다.
		return "", fmt.Errorf("API 주소 조합 실패（API 주소:%s）", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML HTML 본문을 렌더링하고, 본문과 실제로 쓴 항목 수를 반환한다（Channel.Send 참조）.
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram 의 상한은 **문자 수**라, 패킹도 문자로 계량한다（runeSize）.
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">플랫폼에서 전체 보기</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>상태 변경%s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>유형</b>：" + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>자산</b>：" + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>요약</b>：" + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">상세 보기</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes 메시지 제목과 나타날 수 있는 절단 안내에 예약한다（문자로 계산）.
const telegramReservedRunes = 160

// telegramBatchLine 요약의 한 건을 렌더링한다（이스케이프 안 됨, 호출 측이 통일 이스케이프）.
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle 요약 메시지의 제목 행을 렌더링한다. 건수는 **이 건이 실제로 포함한** 건수를 쓰고,
// 이 배치 총수가 아니다——아니면 읽는 사람은 메시지 머리에 쓰인 숫자가 전부라고 여긴다.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("취약점 요약 · 총 %d건", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("（앞 %d건 표시, 나머지 %d건 다음에 계속）", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("최근 %d분 %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape HTML 텍스트 내용을 이스케이프한다.
// Telegram 이 세 종류 실체만 인식하고, 이스케이프 후 &amp; 같은 기존 실체는 이중 이스케이프된다——이것이 바로
// 기대 동작이다:우리가 표시하려는 것은 원본 문자이지, 사용자가 HTML을 주입하게 하는 게 아니다.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr HTML 속성 값을 이스케이프한다. 텍스트 이스케이프 외에 따옴표도 처리해야 한다——
// URL 안에 따옴표가 있으면 href 속성을 조기에 닫아, 뒤의 내용을 주입점으로 만든다.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
