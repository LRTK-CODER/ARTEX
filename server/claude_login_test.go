package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func serveClaude(t *testing.T, s *Server, action string, body any, owner string) (int, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/llm/oauth/claude/"+action, strings.NewReader(string(data)))
	r.Header.Set("Authorization", "Bearer "+owner)
	w := httptest.NewRecorder()
	switch action {
	case "start":
		s.startClaudeLogin(w, r)
	case "complete":
		s.completeClaudeLogin(w, r)
	case "disconnect":
		s.disconnectClaude(w, r)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("응답 %s: %v", w.Body.String(), err)
	}
	assertNoTokens(t, "Claude 로그인 응답", w.Body.String(), "fake-claude-access", "fake-claude-refresh", "fake-claude-code")
	return w.Code, out
}

func newClaudeFixture(t *testing.T) *oauthFixture {
	t.Helper()
	f := newOAuthFixture(t, nil)
	f.profile.AuthType = db.AuthClaudeOAuth
	f.profile.Model = "claude-sonnet-test"
	if _, err := f.pg.SaveProfile(f.profile); err != nil {
		t.Fatal(err)
	}
	return f
}

func startClaudeFlow(t *testing.T, f *oauthFixture, owner string) (string, string) {
	t.Helper()
	status, out := serveClaude(t, f.s, "start", map[string]any{"profile_id": f.profile.ID}, owner)
	if status != 200 {
		t.Fatalf("로그인 시작: %d %v", status, out)
	}
	u, err := url.Parse(out["authorize_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "claude.ai" || u.Query().Get("redirect_uri") != "http://localhost:53692/callback" {
		t.Fatal("인가 주소가 잘못됐다")
	}
	return out["flow_id"].(string), u.Query().Get("state")
}

func TestClaudeLoginRoundTrip(t *testing.T) {
	for _, input := range []string{"url", "code-state"} {
		t.Run(input, func(t *testing.T) {
			f := newClaudeFixture(t)
			calls := 0
			f.reg.claudeClient = &llmauth.ClaudeClient{HTTPClient: &http.Client{Transport: loginClaudeTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var b map[string]string
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Fatal(err)
				}
				if b["code"] != "fake-claude-code" || b["code_verifier"] == "" || b["state"] == "" {
					t.Fatal("교환 필드가 없다")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fake-claude-access","refresh_token":"fake-claude-refresh","expires_in":3600}`))}, nil
			})}}
			flow, state := startClaudeFlow(t, f, "owner-a")
			callback := "http://localhost:53692/callback?code=fake-claude-code&state=" + url.QueryEscape(state)
			if input == "code-state" {
				callback = "fake-claude-code#" + state
			}
			body := map[string]any{"profile_id": f.profile.ID, "flow_id": flow, "callback_url": callback}
			status, out := serveClaude(t, f.s, "complete", body, "owner-a")
			if status != 200 || out["connected"] != true {
				t.Fatalf("로그인 완료: %d %v", status, out)
			}
			stored, err := storedCredentials(t, f)
			if err != nil || stored.AccessToken != "fake-claude-access" || stored.RefreshToken != "fake-claude-refresh" {
				t.Fatal("토큰이 저장되지 않았다")
			}
			if status, _ = serveClaude(t, f.s, "complete", body, "owner-a"); status != 400 || calls != 1 {
				t.Fatal("일회용 흐름을 재사용했다")
			}
			cfg, ok := f.s.profileConfig(f.profile)
			if !ok || cfg.AuthType != db.AuthClaudeOAuth || cfg.Provider() != "anthropic" || cfg.Retry.StreamAttempts != -1 {
				t.Fatalf("Claude 프로필 배선이 틀리다: ok=%v", ok)
			}
			if status, _ = serveClaude(t, f.s, "disconnect", map[string]any{"profile_id": f.profile.ID}, "owner-a"); status != 200 {
				t.Fatal("해제 실패")
			}
			if _, err = storedCredentials(t, f); err == nil {
				t.Fatal("해제 후 자격 증명이 남았다")
			}
		})
	}
}

type loginClaudeTransport func(*http.Request) (*http.Response, error)

func (f loginClaudeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeLoginRejectsWrongFlow(t *testing.T) {
	f := newClaudeFixture(t)
	flow, state := startClaudeFlow(t, f, "owner-a")
	for _, tc := range []struct{ name, input, owner string }{{"wrong owner", "code#" + state, "owner-b"}, {"wrong state", "code#different", "owner-a"}, {"missing state", "code", "owner-a"}, {"wrong port", "http://localhost:1455/callback?code=code&state=" + state, "owner-a"}, {"duplicate code", "http://localhost:53692/callback?code=one&code=two&state=" + state, "owner-a"}} {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := serveClaude(t, f.s, "complete", map[string]any{"profile_id": f.profile.ID, "flow_id": flow, "callback_url": tc.input}, tc.owner)
			if status != 400 {
				t.Fatalf("잘못된 흐름을 수락했다: %d", status)
			}
		})
	}
	f.s.claudeLoginFlows.now = func() time.Time { return time.Now().Add(time.Hour) }
	status, out := serveClaude(t, f.s, "complete", map[string]any{"profile_id": f.profile.ID, "flow_id": flow, "callback_url": "code#" + state}, "owner-a")
	if status != 400 || out["code"] != "flow_expired" {
		t.Fatalf("만료된 흐름: %d %v", status, out)
	}
}

func TestClaudeDisconnectDuringExchange(t *testing.T) {
	f := newClaudeFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.reg.claudeClient = &llmauth.ClaudeClient{HTTPClient: &http.Client{Transport: loginClaudeTransport(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fake-claude-access","refresh_token":"fake-claude-refresh","expires_in":3600}`))}, nil
	})}}
	flow, state := startClaudeFlow(t, f, "owner-a")
	done := make(chan int, 1)
	go func() {
		status, _ := serveClaude(t, f.s, "complete", map[string]any{"profile_id": f.profile.ID, "flow_id": flow, "callback_url": "fake-claude-code#" + state}, "owner-a")
		done <- status
	}()
	<-entered
	_, _ = serveClaude(t, f.s, "disconnect", map[string]any{"profile_id": f.profile.ID}, "owner-a")
	close(release)
	if status := <-done; status != 400 {
		t.Fatalf("취소한 흐름을 저장했다: %d", status)
	}
	if _, err := f.pg.OAuthCredentials(context.Background(), f.reg.cipher, f.profile.ID); err == nil {
		t.Fatal("연결 해제 뒤 토큰이 되살아났다")
	}
}

func TestClaudeProfileSaveRules(t *testing.T) {
	f := newClaudeFixture(t)
	body := strings.NewReader(`{"id":` + fmt.Sprint(f.profile.ID) + `,"name":"t-claude-save-rules","format":"openai","model":"claude-sonnet-test","auth_type":"claude_oauth","base_url":"https://untrusted.example","api_key":"fake-key","streaming":false}`)
	w := httptest.NewRecorder()
	f.s.pgSaveProfile(w, httptest.NewRequest(http.MethodPost, "/api/llm/profiles", body))
	if w.Code != 200 {
		t.Fatalf("프로필 저장: %d %s", w.Code, w.Body.String())
	}
	p, err := f.pg.ProfileByID(f.profile.ID)
	if err != nil || p.Format != "anthropic" || p.BaseURL != "" || p.APIKey != "" || p.Streaming {
		t.Fatalf("프로필 규칙이 적용되지 않았다: %v", err)
	}
}

func TestClaudeModelsUseStoredCredentials(t *testing.T) {
	f := newClaudeFixture(t)
	f.connect(t, time.Now().Add(time.Hour))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+fakeOAuthAccessToken || r.Header.Get("x-api-key") != "" {
			t.Error("모델 목록에 저장된 OAuth 인증이 적용되지 않았다")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-test"},{"id":"claude-opus-test"}]}`)
	}))
	defer ts.Close()
	f.s.claudeBaseURL = ts.URL
	body := strings.NewReader(`{"profile_id":` + fmt.Sprint(f.profile.ID) + `,"auth_type":"claude_oauth","base_url":"https://untrusted.example","api_key":"fake-input-key"}`)
	w := httptest.NewRecorder()
	f.s.pgListModels(w, httptest.NewRequest(http.MethodPost, "/api/llm/models", body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"claude-sonnet-test"`) {
		t.Fatalf("모델 목록 실패: %s", w.Body.String())
	}
	assertNoTokens(t, "모델 목록", w.Body.String(), "fake-input-key")
}

func TestClaudeFailureDisablesIntentAndStreamReplay(t *testing.T) {
	f := newClaudeFixture(t)
	f.connect(t, time.Now().Add(time.Hour))
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"Too many requests"}}`)
	}))
	defer ts.Close()
	f.s.claudeBaseURL = ts.URL
	cfg, ok := f.s.profileConfig(f.profile)
	if !ok {
		t.Fatal("연결된 프로필이 없다")
	}
	p, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = p.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}})
	var noReplay *agent.ClaudeRequestError
	if !errors.As(err, &noReplay) || retryableWorkerModelError(harness.ReasonModelError, err) || isRetryableStreamError(err) || calls != 1 {
		t.Fatal("Claude 실패가 상위 재시도로 넘어간다")
	}
}

func TestClaudeConcurrentSourcesRotateRefreshOnce(t *testing.T) {
	f := newClaudeFixture(t)
	f.connect(t, time.Now().Add(-time.Minute))
	var refreshes atomic.Int32
	f.reg.claudeClient = &llmauth.ClaudeClient{HTTPClient: &http.Client{Transport: loginClaudeTransport(func(r *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		var fields map[string]string
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil || fields["refresh_token"] != fakeOAuthRefreshToken {
			t.Error("잠금 안에서 현재 회전 토큰을 읽지 않았다")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fake-rotated-access","refresh_token":"fake-rotated-refresh","expires_in":3600}`))}, nil
	})}}
	store := oauthStore{pg: f.pg, cipher: f.reg.cipher, profileID: f.profile.ID}
	sources := []*llmauth.TokenSource{
		llmauth.NewTokenSource(f.reg.claudeClient, store, nil),
		llmauth.NewTokenSource(f.reg.claudeClient, store, nil),
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, source := range sources {
		go func() {
			<-start
			token, _, err := source.Token(t.Context())
			if err == nil && token != "fake-rotated-access" {
				err = errors.New("회전 토큰이 다르다")
			}
			results <- err
		}()
	}
	close(start)
	for range sources {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	stored, err := storedCredentials(t, f)
	if err != nil || refreshes.Load() != 1 || stored.RefreshToken != "fake-rotated-refresh" {
		t.Fatal("일회용 refresh token을 중복 사용했거나 회전 결과를 저장하지 않았다")
	}
}

func TestClaudeStaleSourceCannotReadAnotherProviderToken(t *testing.T) {
	f := newClaudeFixture(t)
	f.profile.AuthType = db.AuthChatGPTOAuth
	if _, err := f.pg.SaveProfile(f.profile); err != nil {
		t.Fatal(err)
	}
	f.connect(t, time.Now().Add(time.Hour))
	// disconnect/forget와 source 생성 사이의 경쟁을 낡은 인증 방식으로 늦게 만든 소스로 재현한다.
	src := f.reg.sourceForAuth(f.profile.ID, db.AuthClaudeOAuth)
	if token, _, err := src.Token(t.Context()); !errors.Is(err, llmauth.ErrNoTokens) || token != "" {
		t.Fatal("낡은 Claude 소스가 ChatGPT 토큰을 읽었다")
	}
}

func TestClaudeStaleSourceCannotRefreshAnotherProviderToken(t *testing.T) {
	f := newClaudeFixture(t)
	f.connect(t, time.Now().Add(time.Hour))
	var refreshCalls atomic.Int32
	f.reg.claudeClient = &llmauth.ClaudeClient{HTTPClient: &http.Client{Transport: loginClaudeTransport(func(*http.Request) (*http.Response, error) {
		refreshCalls.Add(1)
		return nil, errors.New("사용하면 안 되는 갱신 경로")
	})}}
	src := f.reg.sourceForAuth(f.profile.ID, db.AuthClaudeOAuth)
	token, _, err := src.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.profile.AuthType = db.AuthChatGPTOAuth
	if _, err := f.pg.SaveProfile(f.profile); err != nil {
		t.Fatal(err)
	}
	f.connect(t, time.Now().Add(time.Hour))
	src.Invalidate(token)
	if value, _, err := src.Token(t.Context()); !errors.Is(err, llmauth.ErrNoTokens) || value != "" || refreshCalls.Load() != 0 {
		t.Fatal("다른 제공자의 토큰을 Claude 인증 서버에서 갱신했다")
	}
}

func TestClaudeSwitchCancelsPendingChatGPTFlow(t *testing.T) {
	f := newOAuthFixture(t, nil)
	flow := &loginFlow{profileID: f.profile.ID, isDevice: true, expiresAt: time.Now().Add(time.Minute)}
	f.s.loginFlows.add("old-chatgpt", flow)
	body := strings.NewReader(fmt.Sprintf(`{"id":%d,"name":"t-claude-flow-switch","model":"claude-sonnet-test","auth_type":"claude_oauth"}`, f.profile.ID))
	w := httptest.NewRecorder()
	f.s.pgSaveProfile(w, httptest.NewRequest(http.MethodPost, "/api/llm/profiles", body))
	if w.Code != 200 || f.s.loginFlows.isRegistered(flow) {
		t.Fatal("Claude로 바꾼 프로필의 ChatGPT 로그인 흐름이 살아 있다")
	}
}

func TestClaudeProfileRejectsLateChatGPTSave(t *testing.T) {
	f := newClaudeFixture(t)
	err := f.s.saveChatGPTLogin(f.profile.ID, llmauth.Tokens{AccessToken: "fake-chatgpt-late", RefreshToken: "fake-chatgpt-refresh", ExpiresAt: time.Now().Add(time.Hour)})
	if !errors.Is(err, db.ErrOAuthCredentialsNotFound) {
		t.Fatal("Claude 인증 방식으로 바뀐 행에 ChatGPT 토큰을 저장했다")
	}
	if _, err := storedCredentials(t, f); !errors.Is(err, db.ErrOAuthCredentialsNotFound) {
		t.Fatal("잘못된 제공자 자격 증명이 남았다")
	}
}

func TestClaudeRoutesRequireWebLogin(t *testing.T) {
	f := newClaudeFixture(t)
	handler := f.s.Handler()
	for _, path := range []string{"start", "complete", "disconnect", "status/1"} {
		method := http.MethodPost
		if strings.HasPrefix(path, "status/") {
			method = http.MethodGet
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, "/api/llm/oauth/claude/"+path, strings.NewReader(`{}`)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("로그인 없는 Claude 경로가 열렸다: %s %d", path, w.Code)
		}
	}
}

func TestClaudeQuotaAdvancesCursorWithoutResending(t *testing.T) {
	f := newClaudeFixture(t)
	f.connect(t, time.Now().Add(time.Hour))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}))
	defer ts.Close()
	f.s.claudeBaseURL = ts.URL
	cfg, ok := f.s.profileConfig(f.profile)
	if !ok {
		t.Fatal("Claude 프로필이 없다")
	}
	p, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		backup := &scriptedLLMProvider{}
		advanced := false
		hooks := taskLLMStreamHooks{
			current: func() (taskLLMSelection, error) {
				if advanced {
					return taskLLMSelection{profileID: 2, provider: backup}, nil
				}
				return taskLLMSelection{profileID: 1, provider: p, retry: cfg.Retry}, nil
			},
			exhaust: func(taskLLMSelection, error) (db.TaskLLMTransition, error) {
				advanced = true
				next := int64(2)
				return db.TaskLLMTransition{Advanced: true, NextProfileID: &next}, nil
			},
		}
		req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}
		var callErr error
		if stream {
			for _, err := range streamTaskLLM(t.Context(), "1", req, hooks) {
				if err != nil {
					callErr = err
				}
			}
		} else {
			_, _, _, callErr = completeTaskLLM(t.Context(), "1", req, hooks)
		}
		if callErr == nil || !advanced || backup.calls != 0 {
			t.Fatal("한도 오류 뒤 같은 요청을 백업에서 재전송했다")
		}
	}
	// 백업 선택 저장 자체가 실패해도 원래 재전송 금지 오류를 잃지 않는다.
	failedHooks := taskLLMStreamHooks{
		current: func() (taskLLMSelection, error) {
			return taskLLMSelection{profileID: 1, provider: p, retry: cfg.Retry}, nil
		},
		exhaust: func(taskLLMSelection, error) (db.TaskLLMTransition, error) {
			return db.TaskLLMTransition{}, errors.New("fake database unavailable")
		},
	}
	req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}
	_, _, _, failedErr := completeTaskLLM(t.Context(), "1", req, failedHooks)
	if failedErr == nil || retryableWorkerModelError(harness.ReasonModelError, failedErr) {
		t.Fatal("백업 선택 저장 실패가 Claude 요청 재실행으로 이어진다")
	}
	for _, err := range streamTaskLLM(t.Context(), "1", req, failedHooks) {
		if err != nil && retryableWorkerModelError(harness.ReasonModelError, err) {
			t.Fatal("스트림 상태 저장 실패가 Claude 요청 재실행으로 이어진다")
		}
	}
}
