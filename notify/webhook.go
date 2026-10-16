package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel 범용 Webhook 어댑터다:사용자가 URL、메서드、요청 헤더와 JSON 템플릿을 직접 정의한다.
// 그것의 존재로 이 기능이 Slack / Mattermost / Discord / 자체 구축 시스템마다 구현을 따로 쓸 필요가 없어진다——
// 그 플랫폼들은 하나의 설정 가능한 템플릿으로 커버된다.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 범용 Webhook은 공식 제한이 없어, 0을 반환하면 기본 레이트 리밋 없음을 의미하고, 사용자가 대상단 능력에 따라 직접 정한다.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url과 headers를 마스킹한다:대상 주소 자체가 흔히 token을 지니고, 커스텀 헤더에는 보통 인증 자격 증명이 들어 있어,
// 둘 다 인터페이스 되비침에 나타나므로, 둘 다 막아야 한다.
// 대가는 편집 시 그중 한 헤더를 바꾸려면, 헤더 전체 조를 다시 입력해야 한다는 것이다（마스킹 값은「원래 값 유지」로 해석됨）——
// 이 취사는 의도적이다:한 번 더 입력할지언정, 자격 증명을 브라우저에 되비치지 않는다.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 목적지는 url이다. url 변경 시 반드시 headers를 다시 표명해야 한다 —— 아니면 원본 Authorization 헤더
// 가 원본 그대로 새 주소로 발송되는데, 이것이 바로 마스킹 우회의 주 경로다.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate 템플릿 미입력 시의 폴백 요청 본문이다:직관적인 JSON 구조,
// 「JSON 한 건을 받아 DB 저장」하는 대다수 자체 구축 수신단을 커버한다.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData 사용자 템플릿에 노출되는 컨텍스트다.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt 이번 전송 시간이다（RFC3339）, 수신단 기록용.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel 상태 변경의 가독 설명이다, 예「처리 대기 → 수정됨」；상태 변경이 아닐 때는 비어 있음.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL 누락")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("대상 URL 무효: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드 %s（GET/POST/PUT/PATCH 사용 가능）", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 구문 오류: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET GET은 요청 본문을 지니지 않는다:내용을 query에 쑤셔 넣는 것은 템플릿 능력 범위를 넘고, GET 의미에도 맞지 않아,
	// 그래서 GET은「명중 즉시 훅 트리거」류의 수신단에만 적합하다.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// 템플릿이 렌더링한 것은 문자열 형태의 JSON이고, 여기서 json.RawMessage로 변환해 원본 그대로 발송한다,
		// 이중 이스케이프가 사용자가 공들여 만든 구조를 하나의 JSON 문자열 안에 가두는 것을 피한다.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿 렌더링 결과가 유효하지 않은 JSON"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 덮어쓰기를 허용하지만, headers 뒤에 적용해, 명시적 설정이 우선함을 보장한다.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 범용 Webhook은 본문을 절단하지 않는다（수신단은 사용자 자신의 서비스이고, 크기는 body_template이 결정）,
	// 따라서 전체 배치를 송달로 친다.
	return len(m.Items), nil
}

// renderWebhookBody 사용자 템플릿（또는 기본 템플릿）으로 요청 본문을 렌더링한다.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 구문 오류: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 렌더링 실패: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate 템플릿을 파싱한다.
//
// missingkey=zero 누락된 map 키를 에러가 아니라 제로값으로 렌더링하게 한다——하지만 이 파일의 컨텍스트는 구조체라,
// 주된 작용은 .Items가 비었을 때 range가 오류 나지 않게 하는 것이다. 진짜 막아야 할 것은 .Items가 nil인 경우다.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs 템플릿에 노출되는 보조 함수다.
var webhookTemplateFuncs = template.FuncMap{
	// json 임의 값을 JSON으로 직렬화한다.
	//
	// 이 함수는 금상첨화가 아니라 필수다:그것을 없애면, 사용자는 {{.Title}}로 직접 보간할 수밖에 없고,
	// 그런데 취약점 제목에 따옴표나 줄바꿈이 있기만 하면, 요청 본문 전체가 더는 올바른 JSON이 아니게 된다——수신단이
	// 거부하고, 에러 메시지가「JSON 파싱 실패」를 가리켜, 제목에 따옴표가 있다는 걸 전혀 연상하지 못한다.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons JSON 조각을 다른 JSON 문자열 값 내부에 끼워 넣는 데 쓴다（문자열 이스케이프 한 겹）.
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 바깥 따옴표를 제거한다:따옴표를 붙일지는 호출 측이 스스로 결정한다.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
