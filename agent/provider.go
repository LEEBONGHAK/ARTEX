// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType 사고「스위치」필드를 독립적으로 제어(thinking.type):
	//   "" = 미전송(기본값, 이 필드를 지원하지 않는 모델과 호환); "disabled" = 명시적 끄기;
	//   "enabled" = 켜기. ReasoningEffort와 완전히 분리된다——어떤 인터페이스는 thinking 필드가 없고、
	//   강도 파라미터만으로도 사고를 활성화할 수 있어, 둘을 각자 따로 설정할 수 있다..
	ThinkingType string
	// ReasoningEffort 사고「강도」필드를 독립적으로 제어한다:
	//   "" = 미전송(기본값); "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도..
	//   OpenAI 는 최상위 reasoning_effort로 매핑;Anthropic은 다음으로 매핑: output_config.effort.
	ReasoningEffort string
	// Stream 이 profile이 스트리밍(SSE) 인터페이스를 쓸지 제어한다. true(기본값)= 스트리밍;false = 진짜·
	// 비스트리밍(stream:false 전송, 완전한 JSON을 한 번에 받고, Provider.Complete 경로). 비스트리밍이 우회할 수 있는 것은
	// 일부 게이트웨이의 형편없는 SSE 구현(빈 프레임、사고 필드 프레임 손실)이고, 대가는 실행 중의 실시간 진행도/실시간
	// token token 카운트를 잃는 것이다. agentcore.Options.NonStreaming = !Stream으로 매핑된다.
	Stream bool
	// MaxTokens 단일 응답의 출력 상한이다(token). 0 = 이 필드를 보내지 않음, 서버 측 기본값이 결정
	// (역사적 동작). ContextWindowK와 다르다:후자는 모델 총 용량이고, 로컬에서 압축 임계값 계산에만 쓰여,,
	// 요청에는 나타나지 않는다;이 값은 매 요청과 함께 나간다. agentcore.Options.MaxTokens로 매핑된다.
	MaxTokens int
	// MaxTokensField MaxTokens가 어느 요청 필드명을 쓸지 선택하며, format=openai에만 적용된다:
	//   "" = max_tokens(기본값); "max_completion_tokens" = 새 필드.
	// OpenAI OpenAI 추론 모델(o 시리즈/GPT-5)은 후자만 인식하고, max_tokens를 받으면 바로
	// unsupported_parameter;unsupported_parameter;그런데 대부분의 호환 게이트웨이는 전자만 인식하므로, 자동 추론은 하지 않고, 사용자가 엔드포인트에 따라 선택하게 맡긴다.
	MaxTokensField string
	// SessionHeaderKey,비어있지 않으면, 매 LLM 요청에 커스텀 HTTP 헤더 하나를 붙이게 하고, 헤더명은 이 값、
	// 헤더 값은【현재 세션의 session id】다(chat 세션=conv-<id>,worker=exp<x>-worker-i<intent>
	// 등, WorkerSessionID 참조). session-id 헤더로 프롬프트 캐싱/스티키 라우팅을 하는 일부 게이트웨이에 쓴다.
	// 빈 값 = 미전송. 값은 transcript.WithSessionID가 요청 context에 걸고, RoundTripper가 RoundTripper
	// 읽어 채워 넣으므로, 같은 공유 provider라도 세션별로 다른 헤더 값을 내보낼 수 있다.
	SessionHeaderKey string
	// Retry 이 설정을 파싱한 뒤의 재시도 파라미터다(profile 오버라이드 → 전역 정책 → 내장 기본값,
	// server 측이 파싱). 세 계층의 의미는 RetryConfig 참조;제로값 = 내장 기본값을 완전히 그대로 사용.
	Retry RetryConfig
}

// RetryConfig 하나의 LLM 설정을 따라다니는 재시도 파라미터다. 각 계층의「횟수」는 통일된 의미다:
// 0 = 내장 기본 횟수 사용;음수 = 이 계층 재시도 끄기;>0 = 이 값 사용. 각 계층의「간격」:
// 0 = 이 계층 본래의 지수 백오프 사용;>0 = 이 고정 간격으로 변경.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval:SDK 연결 수립 재시도(연결 리셋/타임아웃/429/5xx, 스트림 시작 전),
	// llm.Config.MaxRetries / RetryInterval로 바로 매핑된다. 기본 3회、0.5s부터 지수(최대 8s).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval:SDK 빈 응답 재시도(완료됐지만 content block 없음, 오직 openai
	// 형식), llm.Config.EmptyResponseRetries / EmptyResponseInterval로 매핑된다.
	// 기본 2회、동일한 지수 그래디언트.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval:동일 provider 안전 윈도우 재시도——이 프로젝트가 SDK 위에 덧붙인
	// 한 계층으로, 「아직 호출 측에 어떤 출력도 전달하지 않은」때만 끊긴 스트림/과부하/스트림 내 429를 재생한다. SDK는 그것을 보지 못하고,,
	// server/task_llm.go가 소비한다. 기본 2회、0.5s부터 지수(최대 4s).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// 기본 스트리밍;ARTEX_LLM_STREAM=false/0/off면 명시적으로 꺼서 비스트리밍으로 간다.
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 기본 스트리밍;호출 측이 profile로 오버라이드
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 사고 스위치와 강도 두 필드를 각자 그대로 전달한다(빈 값 = 이 필드 미전송). 둘은 분리됨:
	// thinking.type만 보내거나、effort만 보내거나、둘 다 보내거나、둘 다 안 보낼 수 있다.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 출력 상한의 필드명 선택(빈 값 = max_tokens 사용). 상한의「값」은 여기 없다:그것은 매 라운드마다
	// agentcore.Options.MaxTokens agentcore.Options.MaxTokens를 따라가고, provider는 그것을 어느 키에 넣을지만 결정한다.
	lc.MaxTokensField = c.MaxTokensField
	// 재시도 파라미터는 SDK와 같은 의미이며(횟수 0=기본/음수=끄기, 간격 0=지수 백오프/>0=고정), 원본 그대로 전달한다.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 비워 둠=직접 연결, HTTP_PROXY/HTTPS_PROXY 환경 변수로 폴백하지 않음
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "테스트 클릭" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP 요청을 전혀 보내지 않음(설정 파싱 또는 연결 수립에서 바로 실패)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 시도 %d/%d HTTP %d\\n응답 본문: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(비어있음)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(절단, 총 %d바이트)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 원시 wire 메시지를 캡처한다:연결 테스트에서 가장 봐야 할 것은 게이트웨이가 대체 무엇을 돌려줬는가다(상태 코드+응답 본문),
	// 그런데 norma가 응답을 StreamEvent로 디코딩한 뒤에는 이것들이 사라진다. quotaAwareTransport가
	// context context에서 이 Capture를 찾아 매 HTTP 시도의 상태 코드와 body를 채워 넣는다.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 연결 테스트는 단발 경로라, agentcore의 세션 루프를 거치지 않으므로, 아무도 context에 거는 것은
	// session id. SessionHeaderKey를 설정한 엔드포인트(예: opencode zen이 강제 요구하는
	// x-opencode-session 헤더, 없으면 바로 400 MissingSessionID), 이것은 "대화는 정상、
	// 테스트 클릭은 400"이라는 낙차다. 여기서 일회용 랜덤 session id를 하나 덧붙여, 테스트와 실제 대화가 같은
	// 헤더 발송 로직을 쓰게 한다;SessionHeaderKey를 설정하지 않은 엔드포인트는 그것을 읽지 않아, 부작용이 없다.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens MaxTokens를 넉넉히 줘야 한다:추론 모델(예: deepseek-v4-pro)은 답을 내기 전에 먼저 큰 덩어리를
	// 사고를 산출한다(실측으로 "ping" 한 마디에도 ~2900 token을 태움). 32만 주면, 모델은 계속 "사고 단계"
	// 에 머물다 출력 상한(finish=length)에 부딪혀、잘리고, 연결 테스트는 여전히 통과로 치지만(err=nil) 표시는
	// "중단됨/length/resume" 엉망진창이 된다. 예산을 넉넉히 줘 OK를 깔끔히 다 뱉게 한다(finish=stop).
	// EscalateMaxTokens false를 유지한다:절단 때문에 한도를 올려 재시도하지 않아, resume 루프 공회전을 방지한다.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"你是连接测试。直接输出两个字符 OK 即可，不要思考、不要解释、不要别的。"},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // 이 profile의 실제 송수신 모드로 연결 테스트를 한다
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil err==nil로는 아직 부족하다:요청은 통했지만 모델이 한 글자도 안 뱉는 경우가 실제로 존재한다(사고가 예산을 다 태움、
	// 본문이 보안 정책에 삼켜짐、호환 계층이 content를 잃음). 이런 설정은 세션에서 바로 "응답 안 함",
	// 테스트는 성공으로 보고된다——바로 이 항목이 없애려는 낙차다. 보이는 본문이 없으면 일률적으로 실패로 판정한다.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("모델이 응답 내용 없음（요청은 통했지만, 어떤 텍스트도 반환하지 않음）")
	}
	return lat, reply, nil
}
