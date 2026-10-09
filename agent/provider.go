// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph.
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
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
	// ThinkingType 은 사고 '스위치' 필드(thinking.type)를 따로 제어한다.
	//   "" = 보내지 않음(기본값, 이 필드를 지원하지 않는 모델과 호환); "disabled" = 명시적으로 끔;
	//   "enabled" = 켬. ReasoningEffort와 완전히 분리된다. thinking 필드가 없고
	//   사고 강도 파라미터만으로 사고를 켜는 API 도 있어, 둘을 따로 설정할 수 있게 둔다.
	ThinkingType string
	// ReasoningEffort 는 사고 '강도' 필드를 따로 제어한다.
	//   "" = 보내지 않음(기본값); "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도.
	//   OpenAI는 최상위 reasoning_effort로, Anthropic은 output_config.effort로 매핑한다.
	ReasoningEffort string
	// Stream 은 이 프로필이 스트리밍(SSE) API를 쓸지 정한다. true(기본값) = 스트리밍,
	// false = 진짜 비스트리밍(stream:false를 보내 완전한 JSON을 한 번에 받고 Provider.Complete를 탄다).
	// 비스트리밍은 일부 게이트웨이의 나쁜 SSE 구현(빈 프레임, 사고 필드 누락)을 피할 수 있지만,
	// 실행 중 실시간 진행 상황과 실시간 토큰 집계를 잃는다. agentcore.Options.NonStreaming = !Stream으로 매핑한다.
	Stream bool
	// MaxTokens 는 한 번의 응답에 대한 출력 최대 토큰이다. 0 = 이 필드를 보내지 않고 서버 기본값에 맡긴다
	// (기존 동작). ContextWindowK와 다르다. 후자는 모델 전체 용량이라 압축 임계값을 로컬에서 계산할 때만 쓰고
	// 요청에는 넣지 않지만, 이 값은 요청마다 보낸다. agentcore.Options.MaxTokens로 매핑한다.
	MaxTokens int
	// MaxTokensField 는 MaxTokens를 어느 요청 필드 이름으로 보낼지 고르며, format=openai 에만 적용된다.
	//   "" = max_tokens(기본값); "max_completion_tokens" = 새 필드.
	// OpenAI 추론 모델(o 계열/GPT-5)은 후자만 받고 max_tokens를 받으면 바로 unsupported_parameter를 낸다.
	// 반면 호환 게이트웨이 대부분은 전자만 받으므로, 자동 추론하지 않고 사용자가 엔드포인트에 맞게 고르게 한다.
	MaxTokensField string
	// SessionHeaderKey 가 비어 있지 않으면, LLM 요청마다 사용자 지정 HTTP 헤더를 붙인다. 헤더 이름은 이 값이고,
	// 헤더 값은 현재 세션의 session id 다(chat 세션=conv-<id>, worker=exp<x>-worker-i<intent> 등, WorkerSessionID 참고).
	// session-id 헤더로 프롬프트 캐싱이나 고정 라우팅을 하는 일부 게이트웨이에 쓴다.
	// 빈 값 = 보내지 않음. 값은 transcript.WithSessionID가 요청 context에 실어 두고 RoundTripper가 읽어 채우므로,
	// 같은 공유 provider 라도 세션마다 다른 헤더 값을 보낼 수 있다.
	SessionHeaderKey string
	// Retry 는 이 설정이 해석된 뒤의 재시도 파라미터다(프로필 덮어쓰기 → 전역 정책 → 내장 기본값 순으로
	// server 쪽에서 해석한다). 세 층의 뜻은 RetryConfig에 있다. 값을 비우면 내장 기본값을 쓴다.
	Retry RetryConfig
	// 구독 인증은 APIKey 대신 OAuthTokens로 제공자별 고정 백엔드를 부른다.
	// 빈 값은 db.AuthAPIKey 다.
	AuthType db.AuthType
	// OAuthTokens 는 ChatGPT·Claude 구독 인증일 때 필요하다.
	OAuthTokens OAuthTokenSource
}

// RetryConfig 는 하나의 LLM 설정을 따라다니는 재시도 파라미터다. 각 층의 '횟수'는 뜻이 같다.
// 0 = 내장 기본 횟수를 쓴다; 음수 = 그 층의 재시도를 끈다; >0 = 그 값을 쓴다. 각 층의 '간격'은
// 0 = 그 층 본래의 지수 백오프를 쓴다; >0 = 이 고정 간격으로 바꾼다.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK 연결 수립 재시도(연결 리셋/시간 초과/429/5xx, 스트림 시작 전)로,
	// llm.Config.MaxRetries / RetryInterval로 바로 매핑한다. 기본값은 3회, 0.5s 부터 지수 증가(최대 8s).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK 빈 응답 재시도(완료했지만 content block이 없음, openai 형식만)로,
	// llm.Config.EmptyResponseRetries / EmptyResponseInterval로 매핑한다.
	// 기본값은 2회, 같은 지수 증가 단계.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: 같은 provider 안전 구간 재시도다. 이 프로젝트가 SDK 위에 덧댄 한 층으로,
	// '아직 호출자에게 어떤 출력도 전달하지 않은' 때만 스트림 끊김/과부하/스트림 안 429를 다시 보낸다.
	// SDK는 이를 보지 못하고 server/task_llm.go가 소비한다. 기본값은 2회, 0.5s 부터 지수 증가(최대 4s).
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
		// 기본은 스트리밍이다. ARTEX_LLM_STREAM=false/0/off로 명시해 끄면 비스트리밍을 탄다.
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
		Stream:  true, // 기본은 스트리밍이다. 호출자가 프로필에 따라 덮어쓴다.
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
//
// db.AuthChatGPTOAuth는 openai-responses 형식과 스트리밍만 받는다. Codex 백엔드가 stream:true 만
// 받으므로 비스트리밍 경로(Provider.Complete)는 SSE 응답을 JSON으로 읽다가 실패하기 때문이다.
func (c Config) NewProvider() (llm.Provider, error) {
	apiKey, baseURL := c.APIKey, c.BaseURL
	var tokens OAuthTokenSource
	switch c.AuthType {
	case "", db.AuthAPIKey:
	case db.AuthChatGPTOAuth:
		if c.OAuthTokens == nil {
			return nil, fmt.Errorf("llm: auth type %s requires a token source", c.AuthType)
		}
		if c.Format != llm.FormatOpenAIResponses {
			return nil, fmt.Errorf("llm: auth type %s requires format openai-responses", c.AuthType)
		}
		if !c.Stream {
			return nil, fmt.Errorf("llm: auth type %s requires streaming", c.AuthType)
		}
		tokens = c.OAuthTokens
		// 비우면 Norma가 OPENAI_API_KEY로 채운다. 실제 헤더는 oauthTransport가 덮어쓴다.
		apiKey = oauthAPIKeyPlaceholder
		if baseURL == "" {
			baseURL = CodexBaseURL
		}
	case db.AuthClaudeOAuth:
		if c.OAuthTokens == nil || c.Format != llm.FormatAnthropic {
			return nil, errors.New("llm: Claude OAuth requires anthropic format and a token source")
		}
		tokens = c.OAuthTokens
		apiKey = "claude-oauth"
		if baseURL == "" {
			baseURL = ClaudeBaseURL
		}
	default:
		return nil, fmt.Errorf("llm: unknown auth type %q", c.AuthType)
	}
	client, err := oauthHTTPClient(c.Proxy, c.SessionHeaderKey, tokens, c.AuthType)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 사고 스위치와 강도 두 필드를 각각 그대로 전달한다(빈 값 = 그 필드를 보내지 않음). 둘은 분리돼 있어
	// thinking.type 만, effort 만, 둘 다, 또는 둘 다 안 보낼 수 있다.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 출력 최대 토큰의 필드 이름 선택(빈 값 = max_tokens 사용). 최대 토큰의 '값'은 여기 있지 않다. 값은 매 회
	// agentcore.Options.MaxTokens를 따라가고, provider는 그것을 어느 키에 넣을지만 정한다.
	lc.MaxTokensField = c.MaxTokensField
	// 재시도 파라미터는 SDK와 뜻이 같아(횟수 0=기본/음수=끔, 간격 0=지수 백오프/>0=고정) 그대로 전달한다.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.AuthType == db.AuthClaudeOAuth {
		// 구독 요청은 transport의 401 갱신 한 번 외에는 자동으로 다시 보내지 않는다.
		lc.MaxRetries, lc.EmptyResponseRetries = -1, -1
	}
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	p, err := llm.NewProvider(lc)
	if err == nil && c.AuthType == db.AuthClaudeOAuth {
		p = claudeProvider{Provider: p}
	}
	return p, err
}

// IsQuotaExhaustedMessage는 잔액·결제·크레딧·쿼터·구독 한도가 소진됐다고 명시한 신호만
// 쿼터 소진으로 본다. 일반 429·rate limit 문구, 인증 실패, 네트워크 오류, 서버 오류는 뺀다.
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
		// Codex(ChatGPT 구독) 백엔드가 429 본문의 error.type으로 주는 값이다. 주간·5시간
		// 한도는 같은 프로필로 재시도해도 풀리지 않으므로 다음 프로필로 넘겨야 한다.
		"usage_limit_reached", "usage_not_included",
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

// quotaAwareHTTPClient 는 Norma가 모든 요청을 보낼 클라이언트를 만든다. tokens가 nil이 아니면
// base transport와 quotaAwareTransport 사이에 oauthTransport를 끼운다. quotaAwareTransport가
// 바깥이라 llmrec 캡처는 토큰을 싣기 전의 Norma 원본 본문을 보고, 헤더는 기록하지 않는다.
func quotaAwareHTTPClient(proxy, sessionHeaderKey string, tokens OAuthTokenSource) (*http.Client, error) {
	return oauthHTTPClient(proxy, sessionHeaderKey, tokens, db.AuthChatGPTOAuth)
}

func oauthHTTPClient(proxy, sessionHeaderKey string, tokens OAuthTokenSource, authType db.AuthType) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 비우면 직접 연결하고, HTTP_PROXY/HTTPS_PROXY 환경 변수로 되돌아가지 않는다.
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
	var base http.RoundTripper = transport
	if tokens != nil {
		if authType == db.AuthClaudeOAuth {
			base = claudeTransport{base: transport, tokens: tokens}
		} else {
			base = oauthTransport{base: transport, tokens: tokens}
		}
	}
	client := &http.Client{Transport: quotaAwareTransport{base: base, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}
	if tokens != nil {
		// 구독 Bearer 토큰은 리다이렉트 대상에 전달하지 않는다.
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so clicking "Test" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP 요청을 하나도 보내지 못함(설정 해석이나 연결 수립에서 실패)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 시도 %d/%d HTTP %d\n응답 본문: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(비어 있음)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(잘림, 총 %d바이트)", len(s))
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
	// 원본 wire 메시지를 잡는다. 연결 테스트에서 가장 봐야 하는 것은 게이트웨이가 실제로 무엇을 돌려줬는가(상태 코드+응답 본문)인데,
	// norma가 응답을 StreamEvent로 디코딩하고 나면 이것이 사라진다. quotaAwareTransport가
	// context 에서 이 Capture를 찾아 매 HTTP 시도의 상태 코드와 body를 채운다.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 연결 테스트는 단발 경로라 agentcore의 세션 루프를 거치지 않으므로 아무도 context에 session id를 달지 않는다.
	// SessionHeaderKey를 설정한 엔드포인트(예: opencode zen은 x-opencode-session 헤더를 강제하고,
	// 없으면 바로 400 MissingSessionID)에서는 '대화는 정상인데 테스트만 400' 이 되는 어긋남이 생긴다.
	// 여기서 일회성 무작위 session id를 달아, 테스트가 실제 대화와 같은 헤더 발송 로직을 타게 한다.
	// SessionHeaderKey를 설정하지 않은 엔드포인트는 이를 읽지 않으므로 부작용이 없다.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens는 넉넉히 준다. 추론 모델(예: deepseek-v4-pro)은 답을 내기 전에 사고를 한참 쏟아 낸다
	// ("ping" 한마디에도 실측 ~2900 토큰). 32 만 주면 모델이 '사고 단계'에 머물다 출력 최대 토큰에 걸려
	// (finish=length) 잘리고, 연결 테스트는 통과(err=nil)로 치면서도 화면에는
	// '중단됨/length/resume' 로 엉망으로 보인다. 예산을 넉넉히 줘서 OK를 깨끗이 끝까지 내게 한다(finish=stop).
	// EscalateMaxTokens는 false로 둔다. 잘림 때문에 최대 토큰을 올려 재시도하지 않아 resume 루프의 헛돎을 막는다.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"You are a connection test. Output exactly the two characters OK and nothing else — no thinking, no explanation, nothing more."},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // 이 프로필의 실제 송수신 방식으로 연결을 테스트한다.
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil 만으로는 부족하다. 요청은 통했지만 모델이 한 글자도 내지 않는 경우가 실제로 있다(사고가 예산을 다 태움,
	// 본문이 안전 정책에 삼켜짐, 호환 계층이 content를 잃음). 이런 설정은 대화에서 '대답을 안 하는' 상태인데도
	// 테스트는 성공으로 보고한다. 바로 이 어긋남을 없애려는 것이다. 보이는 본문이 없으면 모두 실패로 판정한다.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("모델이 응답 내용을 반환하지 않았습니다(요청은 통했으나 텍스트가 없음)")
	}
	return lat, reply, nil
}
