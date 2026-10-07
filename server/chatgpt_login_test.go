package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
)

// 응답·로그에 나오면 안 되는 가짜 로그인 값.
const (
	fakeLoginCode         = "fake-auth-code-issue46-5d2a"
	fakeLoginRefreshToken = "fake-login-refresh-issue46-8e1f"
)

// fakeAuthServer 는 OpenAI 인증 서버처럼 토큰 교환과 디바이스 코드 엔드포인트를 흉내 낸다.
type fakeAuthServer struct {
	t *testing.T
	// deviceStatus 는 디바이스 사용자 코드 시작 응답 상태다. 0 이면 200.
	deviceStatus int
	// isDevicePending 이면 디바이스 토큰 폴링에 403(아직 입력 전)을 계속 준다.
	isDevicePending bool

	mu        sync.Mutex
	verifiers []string
	codes     []string
	access    string
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	return &fakeAuthServer{t: t, access: fakePlanJWT(t, time.Now().Add(time.Hour), "pro")}
}

func (a *fakeAuthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/oauth/token":
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("code") == "" || r.PostForm.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		a.mu.Lock()
		a.verifiers = append(a.verifiers, r.PostForm.Get("code_verifier"))
		a.codes = append(a.codes, r.PostForm.Get("code"))
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": a.access, "refresh_token": fakeLoginRefreshToken})
	case "/api/accounts/deviceauth/usercode":
		if a.deviceStatus != 0 {
			w.WriteHeader(a.deviceStatus)
			return
		}
		_, _ = w.Write([]byte(`{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"1"}`))
	case "/api/accounts/deviceauth/token":
		if a.isDevicePending {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"authorization_code": fakeLoginCode, "code_verifier": "device-verifier-issue46"})
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeAuthServer) exchanged() (verifiers, codes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.verifiers...), append([]string(nil), a.codes...)
}

func newLoginFixture(t *testing.T, auth *fakeAuthServer) *oauthFixture {
	t.Helper()
	issuer := httptest.NewServer(auth)
	t.Cleanup(issuer.Close)
	return newOAuthFixture(t, issuer)
}

// serveLogin 은 실제 라우트 패턴으로 요청을 보낸다. 경로 값({id})도 라우터가 채운다.
func serveLogin(t *testing.T, s *Server, method, path string, body any) (int, map[string]any, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/start", s.startChatGPTLogin)
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/complete", s.completeChatGPTLogin)
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/device", s.startChatGPTDeviceLogin)
	mux.HandleFunc("GET /api/llm/oauth/chatgpt/device/{id}", s.chatGPTDeviceLoginStatus)
	mux.HandleFunc("POST /api/llm/oauth/chatgpt/disconnect", s.disconnectChatGPT)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(string(raw))))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
	}
	return rec.Code, out, rec.Body.String()
}

// startPaste 는 붙여넣기 로그인을 시작하고 흐름 ID 와 authorize URL 의 state 를 돌려준다.
func startPaste(t *testing.T, f *oauthFixture) (flowID, state string) {
	t.Helper()
	code, out, body := serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/start", map[string]any{"profile_id": f.profile.ID})
	if code != http.StatusOK {
		t.Fatalf("start status %d: %s", code, body)
	}
	u, err := url.Parse(out["authorize_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("redirect_uri"); got != llmauth.RedirectURI {
		t.Fatalf("redirect_uri = %q, want %q", got, llmauth.RedirectURI)
	}
	return out["flow_id"].(string), u.Query().Get("state")
}

func storedCredentials(t *testing.T, f *oauthFixture) (db.OAuthCredentials, error) {
	t.Helper()
	return f.pg.OAuthCredentials(context.Background(), f.reg.cipher, f.profile.ID)
}

func TestChatGPTPasteLoginSavesAndReplacesTokenSource(t *testing.T) {
	auth := newFakeAuthServer(t)
	f := newLoginFixture(t, auth)
	logs := captureLogs(t)
	f.reg.recordRefresh(f.profile.ID, &llmauth.TokenError{Op: "refresh", StatusCode: 400, Code: llmauth.ErrorCodeInvalidGrant})
	before := f.reg.source(f.profile.ID)

	flowID, state := startPaste(t, f)
	callback := llmauth.RedirectURI + "?code=" + fakeLoginCode + "&state=" + url.QueryEscape(state)
	code, out, body := serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/complete",
		map[string]any{"profile_id": f.profile.ID, "flow_id": flowID, "callback_url": callback})
	if code != http.StatusOK || out["connected"] != true || out["plan_type"] != "pro" {
		t.Fatalf("complete status %d: %s", code, body)
	}

	verifiers, codes := auth.exchanged()
	if len(codes) != 1 || codes[0] != fakeLoginCode || len(verifiers) != 1 {
		t.Fatalf("exchange requests codes=%v verifiers=%d, want one with the pasted code", codes, len(verifiers))
	}
	cred, err := storedCredentials(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if cred.RefreshToken != fakeLoginRefreshToken || cred.PlanType != "pro" {
		t.Fatalf("stored credentials plan=%q refresh matches=%v", cred.PlanType, cred.RefreshToken == fakeLoginRefreshToken)
	}
	if f.reg.source(f.profile.ID) == before {
		t.Fatal("login kept the old TokenSource")
	}
	if f.reg.loginRequired(f.profile.ID) {
		t.Fatal("login did not clear the re-login mark")
	}
	// 흐름은 한 번만 쓴다.
	if code, out, _ := serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/complete",
		map[string]any{"profile_id": f.profile.ID, "flow_id": flowID, "code": fakeLoginCode, "state": state}); code != http.StatusBadRequest || out["code"] != string(loginErrFlowNotFound) {
		t.Fatalf("reused flow status %d code %v, want flow_not_found", code, out["code"])
	}
	secrets := []string{fakeLoginCode, fakeLoginRefreshToken, auth.access, verifiers[0]}
	assertNoTokens(t, "complete response", body, secrets...)
	assertNoTokens(t, "logs", logs.String(), secrets...)
}

func TestChatGPTPasteLoginRejects(t *testing.T) {
	cases := []struct {
		name     string
		prepare  func(f *oauthFixture, now *time.Time)
		request  func(f *oauthFixture, flowID, state string, otherProfileID int64) map[string]any
		wantCode loginErrorCode
	}{
		{
			name: "state mismatch",
			request: func(f *oauthFixture, flowID, _ string, _ int64) map[string]any {
				return map[string]any{"profile_id": f.profile.ID, "flow_id": flowID, "code": fakeLoginCode, "state": "other-state"}
			},
			wantCode: loginErrStateMismatch,
		},
		{
			name:    "expired after ten minutes",
			prepare: func(_ *oauthFixture, now *time.Time) { *now = now.Add(loginFlowTTL + time.Second) },
			request: func(f *oauthFixture, flowID, state string, _ int64) map[string]any {
				return map[string]any{"profile_id": f.profile.ID, "flow_id": flowID, "code": fakeLoginCode, "state": state}
			},
			wantCode: loginErrFlowExpired,
		},
		{
			name: "other profile",
			request: func(_ *oauthFixture, flowID, state string, otherProfileID int64) map[string]any {
				return map[string]any{"profile_id": otherProfileID, "flow_id": flowID, "code": fakeLoginCode, "state": state}
			},
			wantCode: loginErrFlowNotFound,
		},
		{
			name: "authorization denied",
			request: func(f *oauthFixture, flowID, state string, _ int64) map[string]any {
				return map[string]any{"profile_id": f.profile.ID, "flow_id": flowID,
					"callback_url": llmauth.RedirectURI + "?error=access_denied&state=" + url.QueryEscape(state)}
			},
			wantCode: loginErrAuthorizationDenied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := newFakeAuthServer(t)
			f := newLoginFixture(t, auth)
			now := time.Now()
			f.s.loginFlows.now = func() time.Time { return now }
			other := &db.LLMProfile{Name: f.profile.Name + "-other", Format: "openai-responses", Model: "gpt-5-codex", AuthType: db.AuthChatGPTOAuth}
			otherID, err := f.pg.SaveProfile(other)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, otherID) })
			flowID, state := startPaste(t, f)
			if tc.prepare != nil {
				tc.prepare(f, &now)
			}
			code, out, body := serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/complete", tc.request(f, flowID, state, otherID))
			if code < 400 || out["code"] != string(tc.wantCode) {
				t.Fatalf("status %d body %s, want code %s", code, body, tc.wantCode)
			}
			if _, codes := auth.exchanged(); len(codes) != 0 {
				t.Fatal("rejected login still exchanged the code")
			}
			if _, err := storedCredentials(t, f); err == nil {
				t.Fatal("rejected login stored credentials")
			}
		})
	}
}

// TestChatGPTLoginWithoutCredentialKey 는 oauth.key 를 쓰지 못한 서버가 로그인을 고정 문구로 거절하는지 본다.
func TestChatGPTLoginWithoutCredentialKey(t *testing.T) {
	f := newLoginFixture(t, newFakeAuthServer(t))
	f.s.oauth = nil
	for _, path := range []string{"/api/llm/oauth/chatgpt/start", "/api/llm/oauth/chatgpt/device"} {
		code, out, _ := serveLogin(t, f.s, http.MethodPost, path, map[string]any{"profile_id": f.profile.ID})
		if code != http.StatusServiceUnavailable || out["code"] != string(loginErrKeyUnavailable) {
			t.Fatalf("%s status %d code %v, want 503 credential_key_unavailable", path, code, out["code"])
		}
	}
}

// deviceFlow 는 흐름 ID 로 디바이스 흐름을 꺼낸다. goroutine 이 끝나기를 기다릴 때 쓴다.
func deviceFlow(t *testing.T, s *Server, flowID string) *loginFlow {
	t.Helper()
	s.loginFlows.mu.Lock()
	defer s.loginFlows.mu.Unlock()
	flow := s.loginFlows.flows[flowID]
	if flow == nil || flow.done == nil {
		t.Fatalf("device flow %q not found", flowID)
	}
	return flow
}

func waitDone(t *testing.T, flow *loginFlow) {
	t.Helper()
	select {
	case <-flow.done:
	case <-time.After(10 * time.Second):
		t.Fatal("device polling goroutine did not stop")
	}
}

func startDevice(t *testing.T, f *oauthFixture) string {
	t.Helper()
	code, out, body := serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/device", map[string]any{"profile_id": f.profile.ID})
	if code != http.StatusOK || out["user_code"] != "ABCD-1234" {
		t.Fatalf("device start status %d: %s", code, body)
	}
	return out["flow_id"].(string)
}

func deviceStatus(t *testing.T, s *Server, flowID string) map[string]any {
	t.Helper()
	code, out, body := serveLogin(t, s, http.MethodGet, "/api/llm/oauth/chatgpt/device/"+flowID, nil)
	if code != http.StatusOK {
		t.Fatalf("device status %d: %s", code, body)
	}
	assertNoTokens(t, "device status", body, fakeLoginCode, fakeLoginRefreshToken, "device-verifier-issue46")
	return out
}

func TestChatGPTDeviceLoginSaves(t *testing.T) {
	f := newLoginFixture(t, newFakeAuthServer(t))
	logs := captureLogs(t)
	before := f.reg.source(f.profile.ID)
	flowID := startDevice(t, f)
	waitDone(t, deviceFlow(t, f.s, flowID))
	if out := deviceStatus(t, f.s, flowID); out["status"] != string(deviceLoginSucceeded) {
		t.Fatalf("device status = %v, want succeeded", out)
	}
	cred, err := storedCredentials(t, f)
	if err != nil || cred.PlanType != "pro" {
		t.Fatalf("stored credentials plan=%q err=%v", cred.PlanType, err)
	}
	if f.reg.source(f.profile.ID) == before {
		t.Fatal("device login kept the old TokenSource")
	}
	assertNoTokens(t, "logs", logs.String(), fakeLoginCode, fakeLoginRefreshToken, "device-verifier-issue46")
}

func TestChatGPTDeviceLoginDisabled(t *testing.T) {
	auth := newFakeAuthServer(t)
	auth.deviceStatus = http.StatusNotFound
	f := newLoginFixture(t, auth)
	code, out, _ := serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/device", map[string]any{"profile_id": f.profile.ID})
	if code != http.StatusBadRequest || out["code"] != string(loginErrDeviceLoginDisabled) {
		t.Fatalf("status %d code %v, want device_login_disabled", code, out["code"])
	}
}

// TestChatGPTDeviceLoginStops 는 서버 종료와 연결 해제가 폴링 goroutine 을 끝내고 저장하지 않는지 본다.
func TestChatGPTDeviceLoginStops(t *testing.T) {
	cases := []struct {
		name string
		stop func(t *testing.T, f *oauthFixture, shutdown context.CancelFunc)
	}{
		{name: "server shutdown", stop: func(_ *testing.T, _ *oauthFixture, shutdown context.CancelFunc) { shutdown() }},
		{name: "disconnect", stop: func(t *testing.T, f *oauthFixture, _ context.CancelFunc) {
			serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/disconnect", map[string]any{"profile_id": f.profile.ID})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := newFakeAuthServer(t)
			auth.isDevicePending = true
			f := newLoginFixture(t, auth)
			ctx, shutdown := context.WithCancel(context.Background())
			defer shutdown()
			f.s.ctx = ctx
			flowID := startDevice(t, f)
			flow := deviceFlow(t, f.s, flowID)
			if out := deviceStatus(t, f.s, flowID); out["status"] != string(deviceLoginPending) {
				t.Fatalf("device status = %v, want pending", out)
			}
			tc.stop(t, f, shutdown)
			waitDone(t, flow)
			if _, err := storedCredentials(t, f); err == nil {
				t.Fatal("stopped device login stored credentials")
			}
		})
	}
}

func TestChatGPTDisconnect(t *testing.T) {
	f := newLoginFixture(t, newFakeAuthServer(t))
	f.connect(t, time.Now().Add(time.Hour))
	before := f.reg.source(f.profile.ID)
	code, out, body := serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/disconnect", map[string]any{"profile_id": f.profile.ID})
	if code != http.StatusOK || out["connected"] != false {
		t.Fatalf("disconnect status %d: %s", code, body)
	}
	if _, err := storedCredentials(t, f); err == nil {
		t.Fatal("disconnect kept credentials")
	}
	if f.reg.source(f.profile.ID) == before {
		t.Fatal("disconnect kept the TokenSource")
	}
	code, out, _ = serveLogin(t, f.s, http.MethodPost, "/api/llm/oauth/chatgpt/disconnect", map[string]any{"profile_id": f.profile.ID})
	if code != http.StatusNotFound || out["code"] != string(loginErrNotConnected) {
		t.Fatalf("second disconnect status %d code %v, want not_connected", code, out["code"])
	}
}
