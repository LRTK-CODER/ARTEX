package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/artex/llmpool"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/llm"
)

// *llmauth.TokenSource 가 agent 의 좁은 인터페이스를 만족하는지 컴파일 시점에 확인한다.
var _ OAuthTokenSource = (*llmauth.TokenSource)(nil)

const (
	fakeAccessToken  = "fake-access-token-7f3a9c"
	fakeRefreshedTok = "fake-access-token-refreshed-51be02"
	fakeAccountID    = "acct-test-1"
	fakeEnvAPIKey    = "sk-env-must-not-leak-c4d1"
)

// fakeTokens 는 차례로 정한 토큰을 돌려주는 손으로 만든 토큰 공급자다.
// 실제 TokenSource 처럼 지금 토큰을 Invalidate 했을 때만 다음 토큰으로 넘어간다.
type fakeTokens struct {
	mu            sync.Mutex
	accessTokens  []string
	err           error
	tokenCalls    int
	invalidations int
}

func (f *fakeTokens) Token(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++
	if f.err != nil {
		return "", "", f.err
	}
	i := min(f.invalidations, len(f.accessTokens)-1)
	return f.accessTokens[i], fakeAccountID, nil
}

func (f *fakeTokens) Invalidate(staleAccessToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current := f.accessTokens[min(f.invalidations, len(f.accessTokens)-1)]
	if staleAccessToken == current {
		f.invalidations++
	}
}

// codexServer 는 Codex 백엔드처럼 받은 요청을 기록하고, statuses 의 상태를 차례로 돌려준다.
// statuses 가 다 떨어지면 200 과 sseBody 를 준다.
type codexServer struct {
	mu       sync.Mutex
	statuses []int
	sseBody  string
	headers  []http.Header
	bodies   []map[string]any
}

func (s *codexServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.headers = append(s.headers, r.Header.Clone())
	s.bodies = append(s.bodies, body)
	status := http.StatusOK
	if len(s.statuses) > 0 {
		status, s.statuses = s.statuses[0], s.statuses[1:]
	}
	s.mu.Unlock()
	if status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"detail":"Unauthorized"}`)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	_, _ = io.WriteString(w, s.sseBody)
}

func (s *codexServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.headers)
}

func sseEvent(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

var textTurnSSE = sseEvent("response.created", `{"type":"response.created"}`) +
	sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","delta":"안녕"}`) +
	sseEvent("response.completed", `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`)

var toolTurnSSE = sseEvent("response.created", `{"type":"response.created"}`) +
	sseEvent("response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"Bash"}}`) +
	sseEvent("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","delta":"{\"command\":\"id\"}"}`) +
	sseEvent("response.completed", `{"type":"response.completed","response":{"status":"completed"}}`)

func newOAuthProvider(t *testing.T, baseURL string, tokens OAuthTokenSource) llm.Provider {
	t.Helper()
	c := ConfigFrom("openai-responses", "gpt-5-codex", baseURL, "", "")
	c.AuthType = db.AuthChatGPTOAuth
	c.OAuthTokens = tokens
	c.Retry.ConnectAttempts = -1 // Norma 재시도를 끄고 이 transport 의 재시도만 센다
	prov, err := c.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return prov
}

// streamAll 은 한 턴을 끝까지 받아 메시지와 마지막 오류를 돌려준다.
func streamAll(ctx context.Context, prov llm.Provider, req llm.CompletionRequest) (llm.Message, error) {
	acc := llm.NewAccumulator()
	for ev, err := range prov.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), nil
}

// TestOAuthProviderStreamsTextAndToolCall 은 Norma 실제 provider(FormatOpenAIResponses)를
// 인증 transport 와 함께 SSE 서버에 붙여, 헤더·본문·스트리밍 결과를 확인한다.
func TestOAuthProviderStreamsTextAndToolCall(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", fakeEnvAPIKey)
	cases := []struct {
		name  string
		sse   string
		check func(t *testing.T, msg llm.Message)
	}{
		{"text", textTurnSSE, func(t *testing.T, msg llm.Message) {
			if msg.Text() != "안녕" {
				t.Fatalf("text = %q, want 안녕", msg.Text())
			}
		}},
		{"tool call", toolTurnSSE, func(t *testing.T, msg llm.Message) {
			uses := msg.ToolUses()
			if len(uses) != 1 || uses[0].ID != "call_1" || uses[0].Name != "Bash" || string(uses[0].Input) != `{"command":"id"}` {
				t.Fatalf("tool uses = %+v", uses)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &codexServer{sseBody: tc.sse}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			prov := newOAuthProvider(t, ts.URL, &fakeTokens{accessTokens: []string{fakeAccessToken}})

			msg, err := streamAll(context.Background(), prov, llm.CompletionRequest{
				Messages:  []llm.Message{llm.UserText("hi")},
				MaxTokens: 1024,
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			tc.check(t, msg)

			if srv.calls() != 1 {
				t.Fatalf("upstream calls = %d, want 1", srv.calls())
			}
			h := srv.headers[0]
			if got := h.Get("Authorization"); got != "Bearer "+fakeAccessToken {
				t.Fatalf("Authorization = %q", got)
			}
			if got := h.Get("ChatGPT-Account-ID"); got != fakeAccountID {
				t.Fatalf("ChatGPT-Account-ID = %q", got)
			}
			if got := h.Get("originator"); got != llmauth.Originator {
				t.Fatalf("originator = %q", got)
			}
			for name, values := range h {
				for _, v := range values {
					if strings.Contains(v, fakeEnvAPIKey) || strings.Contains(v, oauthAPIKeyPlaceholder) {
						t.Fatalf("header %s carries an API key value", name)
					}
				}
			}
			body := srv.bodies[0]
			if _, ok := body["max_output_tokens"]; ok {
				t.Fatalf("body still has max_output_tokens: %v", body)
			}
			if body["stream"] != true || body["store"] != false {
				t.Fatalf("stream=%v store=%v, want true/false", body["stream"], body["store"])
			}
		})
	}
}

// TestOAuthTransportRewritesCloneOnly 는 거부 필드를 빼고 stream:true 를 넣되 원본 요청은
// 그대로 두는지 확인한다.
func TestOAuthTransportRewritesCloneOnly(t *testing.T) {
	const original = `{"model":"m","input":[],"stream":false,"store":true,"max_output_tokens":64,"max_tokens":64,"metadata":{"k":"v"}}`
	base := &fakeRT{}
	rt := oauthTransport{base: base, tokens: &fakeTokens{accessTokens: []string{fakeAccessToken}}}
	req, err := http.NewRequest(http.MethodPost, "https://codex.example/responses", strings.NewReader(original))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer placeholder")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if base.seen == req {
		t.Fatal("transport sent the original request instead of a clone")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer placeholder" {
		t.Fatalf("original Authorization changed to %q", got)
	}
	if req.Header.Get("ChatGPT-Account-ID") != "" {
		t.Fatal("original request gained ChatGPT-Account-ID")
	}
	if got := requestBodySnapshot(req); got != original {
		t.Fatalf("original GetBody = %q, want unchanged", got)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(requestBodySnapshot(base.seen)), &sent); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	for _, field := range []string{"max_output_tokens", "max_tokens", "metadata"} {
		if _, ok := sent[field]; ok {
			t.Fatalf("sent body still has %s", field)
		}
	}
	if sent["stream"] != true || sent["store"] != false || sent["model"] != "m" {
		t.Fatalf("sent body = %v", sent)
	}
	if base.seen.ContentLength != int64(len(requestBodySnapshot(base.seen))) {
		t.Fatalf("ContentLength = %d does not match body", base.seen.ContentLength)
	}
}

// TestOAuthRetriesOnceOn401 은 401 이면 Invalidate 뒤 한 번만 다시 보내는지 확인한다.
func TestOAuthRetriesOnceOn401(t *testing.T) {
	cases := []struct {
		name          string
		statuses      []int
		wantCalls     int
		wantErrStatus bool
	}{
		{"second attempt succeeds", []int{http.StatusUnauthorized}, 2, false},
		{"second 401 is returned", []int{http.StatusUnauthorized, http.StatusUnauthorized}, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &codexServer{statuses: tc.statuses, sseBody: textTurnSSE}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			tokens := &fakeTokens{accessTokens: []string{fakeAccessToken, fakeRefreshedTok}}
			prov := newOAuthProvider(t, ts.URL, tokens)

			_, err := streamAll(context.Background(), prov, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
			if tc.wantErrStatus {
				if err == nil || !strings.Contains(err.Error(), "status 401") {
					t.Fatalf("err = %v, want the second 401", err)
				}
			} else if err != nil {
				t.Fatalf("stream: %v", err)
			}
			if srv.calls() != tc.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", srv.calls(), tc.wantCalls)
			}
			if tokens.invalidations != 1 {
				t.Fatalf("invalidations = %d, want 1", tokens.invalidations)
			}
			if got := srv.headers[1].Get("Authorization"); got != "Bearer "+fakeRefreshedTok {
				t.Fatalf("retry Authorization = %q, want the refreshed token", got)
			}
			if got, want := requestBodyJSON(t, srv.bodies[1]), requestBodyJSON(t, srv.bodies[0]); got != want {
				t.Fatalf("retry body differs:\n%s\n%s", got, want)
			}
		})
	}
}

func requestBodyJSON(t *testing.T, body map[string]any) string {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestOAuthLoginRequiredIsHardFailure 는 다시 로그인해야 하는 토큰 오류가 서버를 부르지 않고
// 401 로 올라가, Norma 가 재시도하지 않고 llmpool 이 hard failure 로 보는지 확인한다.
func TestOAuthLoginRequiredIsHardFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"no stored tokens", fmt.Errorf("db: load: %w", llmauth.ErrNoTokens)},
		{"refresh token revoked", fmt.Errorf("llmauth: refresh tokens: %w",
			&llmauth.TokenError{Op: "refresh", StatusCode: http.StatusBadRequest, Code: llmauth.ErrorCodeInvalidGrant})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &codexServer{sseBody: textTurnSSE}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			tokens := &fakeTokens{err: tc.err}
			c := ConfigFrom("openai-responses", "gpt-5-codex", ts.URL, "", "")
			c.AuthType = db.AuthChatGPTOAuth
			c.OAuthTokens = tokens
			prov, err := c.NewProvider() // Norma 기본 재시도를 켠 채로 둔다
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}

			health := llmpool.NewRegistry(nil, nil)
			pool := llmpool.New([]*llmpool.Member{{ID: 7, Name: "chatgpt", Prov: prov}}, health)
			_, err = streamAll(context.Background(), pool, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
			if err == nil {
				t.Fatal("stream succeeded, want login required")
			}
			msg := err.Error()
			if !strings.Contains(msg, "status 401") || !strings.Contains(msg, "chatgpt_login_required") {
				t.Fatalf("err = %q, want status 401 with chatgpt_login_required", msg)
			}
			if srv.calls() != 0 {
				t.Fatalf("upstream calls = %d, want 0", srv.calls())
			}
			if tokens.tokenCalls != 1 || tokens.invalidations != 0 {
				t.Fatalf("token calls = %d, invalidations = %d, want 1 and 0 (no retry)", tokens.tokenCalls, tokens.invalidations)
			}
			if !health.IsOpen(7) {
				t.Fatal("llmpool did not open the breaker on the first failure (not a hard failure)")
			}
		})
	}
}

// TestOAuthTransientTokenErrorIsReturned 는 갱신 서버 장애처럼 로그인과 무관한 토큰 오류는
// 서버를 부르지 않고 오류로 올리는지 확인한다(Norma 의 일시 오류 재시도에 맡긴다).
func TestOAuthTransientTokenErrorIsReturned(t *testing.T) {
	refreshFailed := &llmauth.TokenError{Op: "refresh", StatusCode: http.StatusBadGateway}
	base := &fakeRT{}
	rt := oauthTransport{base: base, tokens: &fakeTokens{err: refreshFailed}}
	req, err := http.NewRequest(http.MethodPost, "https://codex.example/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if resp != nil || !errors.Is(err, refreshFailed) {
		t.Fatalf("resp = %v, err = %v, want the token error", resp, err)
	}
	if base.seen != nil {
		t.Fatal("upstream was called without a token")
	}
}

// TestOAuthTokensNotInCaptureOrLogs 는 401 재시도를 거친 요청에서도 llmrec 캡처와
// 연결 테스트 로그에 토큰이 남지 않는지 확인한다.
func TestOAuthTokensNotInCaptureOrLogs(t *testing.T) {
	srv := &codexServer{statuses: []int{http.StatusUnauthorized}, sseBody: textTurnSSE}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var logs bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	tokens := &fakeTokens{accessTokens: []string{fakeAccessToken, fakeRefreshedTok}}
	prov := newOAuthProvider(t, ts.URL, tokens)
	ctx, capt := llmrec.NewCapture(context.Background())
	if _, err := streamAll(ctx, prov, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}); err != nil {
		t.Fatalf("stream: %v", err)
	}

	c := ConfigFrom("openai-responses", "gpt-5-codex", ts.URL, "", "")
	c.AuthType = db.AuthChatGPTOAuth
	c.OAuthTokens = tokens
	if _, _, err := TestConnection(context.Background(), c); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !strings.Contains(logs.String(), "[llm-test]") {
		t.Fatal("connection test wrote no log; the leak check would be vacuous")
	}

	recorded := []string{capt.RawRequest(), capt.RawResponse(), logs.String()}
	for _, a := range capt.Attempts() {
		recorded = append(recorded, a.Body)
	}
	for _, s := range recorded {
		for _, secret := range []string{fakeAccessToken, fakeRefreshedTok} {
			if strings.Contains(s, secret) {
				t.Fatalf("token leaked into capture or log: %q", s)
			}
		}
	}
}

func TestNewProviderRejectsInvalidOAuthConfig(t *testing.T) {
	tokens := &fakeTokens{accessTokens: []string{fakeAccessToken}}
	valid := func() Config {
		c := ConfigFrom("openai-responses", "gpt-5-codex", "https://codex.example", "", "")
		c.AuthType = db.AuthChatGPTOAuth
		c.OAuthTokens = tokens
		return c
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"missing token source", func(c *Config) { c.OAuthTokens = nil }},
		{"chat completions format", func(c *Config) { c.Format = llm.FormatOpenAI }},
		{"non-streaming", func(c *Config) { c.Stream = false }},
		{"unknown auth type", func(c *Config) { c.AuthType = "cookie" }},
	}
	if _, err := valid().NewProvider(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c)
			if _, err := c.NewProvider(); err == nil {
				t.Fatal("NewProvider accepted an invalid OAuth config")
			}
		})
	}
}

// TestOAuthEmptyBaseURLIgnoresOpenAIBaseURLEnv 는 BaseURL 이 빈 OAuth 프로필이 OPENAI_BASE_URL
// 환경 변수의 주소로 토큰을 보내지 않는지 확인한다. Norma 는 빈 BaseURL 을 그 변수로 채운다.
// 모든 요청을 테스트 프록시로 보내 실제 서버에는 닿지 않게 하고, 프록시가 본 대상과 헤더를 검사한다.
func TestOAuthEmptyBaseURLIgnoresOpenAIBaseURLEnv(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://relay.example.test/v1")
	var mu sync.Mutex
	var seen []string
	var authorizations []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host)
		if a := r.Header.Get("Authorization"); a != "" {
			authorizations = append(authorizations, a)
		}
		mu.Unlock()
		// CONNECT 를 거절해 TLS 와 그 안의 헤더가 나가지 않게 한다.
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()

	c := ConfigFrom("openai-responses", "gpt-5-codex", "", "", proxy.URL)
	c.AuthType = db.AuthChatGPTOAuth
	c.OAuthTokens = &fakeTokens{accessTokens: []string{fakeAccessToken}}
	c.Retry.ConnectAttempts = -1
	prov, err := c.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := streamAll(context.Background(), prov, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}); err == nil {
		t.Fatal("stream succeeded through a proxy that rejects everything")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(authorizations) != 0 {
		t.Fatalf("proxy saw Authorization headers %q (requests %v)", authorizations, seen)
	}
	if len(seen) != 1 || seen[0] != "CONNECT chatgpt.com:443" {
		t.Fatalf("proxy saw %v, want one CONNECT to the Codex backend", seen)
	}
}

// TestAPIKeyEmptyBaseURLFollowsOpenAIBaseURLEnv 는 빈 BaseURL 을 Codex 주소로 채우는 규칙이 OAuth
// 프로필에만 적용되는지 확인한다. API 키 프로필까지 채우면 API 키가 chatgpt.com 으로 나간다.
// 평문 HTTP 중계 주소를 써서 테스트 프록시가 대상과 헤더를 그대로 보게 하고, 실제 서버에는 닿지 않는다.
func TestAPIKeyEmptyBaseURLFollowsOpenAIBaseURLEnv(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://relay.example.test/v1")
	const apiKey = "sk-test-apikey"
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.String()+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()

	c := ConfigFrom("openai-responses", "gpt-5", "", apiKey, proxy.URL)
	c.Retry.ConnectAttempts = -1
	prov, err := c.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := streamAll(context.Background(), prov, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}); err == nil {
		t.Fatal("stream succeeded through a proxy that rejects everything")
	}

	mu.Lock()
	defer mu.Unlock()
	want := "POST http://relay.example.test/v1/responses Bearer " + apiKey
	if len(seen) != 1 || seen[0] != want {
		t.Fatalf("proxy saw %q, want [%q]", seen, want)
	}
}
