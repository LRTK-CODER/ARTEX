package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// codexUsageLimitBody 는 Codex 백엔드가 구독 한도를 다 썼을 때 429와 함께 돌려주는 본문 형태다
// (openai/codex codex-rs/codex-api/src/api_bridge.rs).
const codexUsageLimitBody = `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":1760000000,"resets_in_seconds":3600}}`

func TestIsQuotaExhaustedMessage(t *testing.T) {
	t.Parallel()
	positive := []string{
		`{"error":{"code":"insufficient_quota"}}`,
		`RESOURCE_EXHAUSTED`,
		`You exceeded your current quota, please check your plan and billing details.`,
		`billing_not_active`,
		`credit balance is too low`,
		`账户余额不足，请充值`,
		// Codex 구독 한도: 주간·5시간 한도 소진(usage_limit_reached)과 요금제 미포함(usage_not_included).
		codexUsageLimitBody,
		`{"error":{"type":"usage_not_included","message":"Your plan does not include Codex"}}`,
		`openai: status 402: ` + codexUsageLimitBody,
		`openai: status 429: ` + codexUsageLimitBody,
	}
	for _, message := range positive {
		if !IsQuotaExhaustedMessage(message) {
			t.Errorf("expected quota classification for %q", message)
		}
	}

	negative := []string{
		`status 429: rate limit exceeded`,
		`RESOURCE_EXHAUSTED: rate limit exceeded`,
		`too many requests per minute`,
		`quota exceeded for quota metric GenerateRequestsPerMinutePerProjectPerBaseModel`,
		`RESOURCE_EXHAUSTED: TPM quota exceeded`,
		`tokens per minute quota exceeded`,
		`rate_limit_exceeded: requests per second`,
		`status 401: invalid api key`,
		`status 401: insufficient_quota`,
		`HTTP 403: billing_hard_limit_reached`,
		`status 500: internal server error`,
		`status=503: insufficient_quota`,
		`context length exceeded`,
		`openai: status 429: {"error":{"type":"rate_limit_exceeded","message":"Rate limit reached"}}`,
		`status 401: usage_limit_reached`,
		`status 503: usage_not_included`,
	}
	for _, message := range negative {
		if IsQuotaExhaustedMessage(message) {
			t.Errorf("unexpected quota classification for %q", message)
		}
	}
}

func TestQuotaAwareTransportOnlyNormalizesExplicitQuota429(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{name: "quota", status: http.StatusTooManyRequests, body: `{"code":"insufficient_quota"}`, wantStatus: http.StatusPaymentRequired},
		{name: "codex usage limit", status: http.StatusTooManyRequests, body: codexUsageLimitBody, wantStatus: http.StatusPaymentRequired},
		{name: "ordinary rate limit", status: http.StatusTooManyRequests, body: `{"message":"rate limit exceeded"}`, wantStatus: http.StatusTooManyRequests},
		{name: "server error", status: http.StatusInternalServerError, body: `insufficient_quota`, wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := quotaAwareTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tt.status,
					Status:     http.StatusText(tt.status),
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Header:     make(http.Header),
				}, nil
			})}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://example.invalid", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status=%d, want %d", resp.StatusCode, tt.wantStatus)
			}
			gotBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotBody) != tt.body {
				t.Fatalf("body=%q, want %q", gotBody, tt.body)
			}
		})
	}
}

func TestProviderDoesNotRetryExplicitQuota429(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`)
	}))
	defer upstream.Close()

	provider, err := ConfigFrom("openai", "test-model", upstream.URL, "test-key", "").NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for _, err := range provider.Stream(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{llm.UserText("ping")},
	}) {
		if err != nil {
			streamErr = err
		}
	}
	if streamErr == nil || !IsQuotaExhaustedMessage(streamErr.Error()) {
		t.Fatalf("expected explicit quota error, got %v", streamErr)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("explicit quota request retried %d times, want exactly one request", got)
	}
}

// Codex 구독 한도 429는 같은 프로필로 재시도하지 않고 한 번에 쿼터 소진 오류로 올라와야
// 작업 체인이 다음 프로필로 넘어갈 수 있다.
func TestProviderDoesNotRetryCodexUsageLimit429(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("content-type", "application/json")
		w.Header().Set("x-codex-primary-used-percent", "100")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, codexUsageLimitBody)
	}))
	defer upstream.Close()

	provider, err := ConfigFrom("openai", "test-model", upstream.URL, "test-key", "").NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for _, err := range provider.Stream(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{llm.UserText("ping")},
	}) {
		if err != nil {
			streamErr = err
		}
	}
	if streamErr == nil || !IsQuotaExhaustedMessage(streamErr.Error()) {
		t.Fatalf("expected codex usage limit to be quota exhaustion, got %v", streamErr)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("codex usage limit request sent %d times, want exactly one", got)
	}
}
