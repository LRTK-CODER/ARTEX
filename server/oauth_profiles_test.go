package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/norma/llm"
)

// 응답·로그에 나오면 안 되는 가짜 토큰.
const (
	fakeOAuthAccessToken  = "fake-access-token-issue39-7c1e"
	fakeOAuthRefreshToken = "fake-refresh-token-issue39-2b9d"
	fakeOAuthAccountID    = "acct-issue39"
)

// syncBuffer 는 여러 goroutine 이 log 로 써도 안전한 버퍼다.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs 는 테스트가 끝날 때까지 표준 log 출력을 모은다.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// assertNoTokens 는 out 에 가짜 토큰이 없는지 본다.
func assertNoTokens(t *testing.T, where, out string, tokens ...string) {
	t.Helper()
	for _, tok := range append([]string{fakeOAuthAccessToken, fakeOAuthRefreshToken}, tokens...) {
		if strings.Contains(out, tok) {
			t.Fatalf("%s contains token %q:\n%s", where, tok, out)
		}
	}
}

// fakeAccessJWT 는 llmauth 가 읽는 클레임(exp, chatgpt_account_id)만 담은 서명 없는 JWT 다.
func fakeAccessJWT(t *testing.T, exp time.Time, marker string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"exp":                         exp.Unix(),
		"marker":                      marker,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": fakeOAuthAccountID},
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// oauthFixture 는 실제 PostgreSQL 에 chatgpt_oauth 프로필 하나와 그 레지스트리를 만든다.
type oauthFixture struct {
	s       *Server
	pg      *db.DB
	reg     *oauthTokenRegistry
	profile *db.LLMProfile
}

// newOAuthFixture 는 PostgreSQL 이 없으면 건너뛴다. issuer 는 갱신 요청을 받을 가짜 인증 서버다.
func newOAuthFixture(t *testing.T, issuer *httptest.Server) *oauthFixture {
	t.Helper()
	skipWithoutPostgres(t)
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	key, err := db.LoadOrCreateCredentialKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := db.NewTokenCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	client := &llmauth.Client{Issuer: "http://127.0.0.1:1"} // 부르면 연결이 거절되는 주소
	if issuer != nil {
		client = &llmauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}
	}
	reg := newOAuthTokenRegistry(pg, cipher, client)
	// 저장된 형식·수신 방식이 어긋나도 OAuth 규칙이 덮어쓰는지 보려고 일부러 anthropic·비스트리밍으로 둔다.
	p := &db.LLMProfile{Name: "t-issue39-" + strings.ReplaceAll(t.Name(), "/", "-"), Format: "anthropic",
		Model: "gpt-5-codex", Streaming: false, AuthType: db.AuthChatGPTOAuth}
	id, err := pg.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	p.ID = id
	s := &Server{m: &Manager{pg: pg}, oauth: reg, provByProfile: map[int64]*provEntry{}}
	return &oauthFixture{s: s, pg: pg, reg: reg, profile: p}
}

// connect 는 프로필에 가짜 자격 증명을 저장한다.
func (f *oauthFixture) connect(t *testing.T, expiresAt time.Time) {
	t.Helper()
	f.connectWith(t, fakeOAuthAccessToken, expiresAt)
}

// connectWith 는 access token 을 골라 저장한다. 다시 로그인하면 access token 이 바뀐다.
func (f *oauthFixture) connectWith(t *testing.T, accessToken string, expiresAt time.Time) {
	t.Helper()
	err := f.pg.SaveOAuthCredentials(context.Background(), f.reg.cipher, db.OAuthCredentials{
		ProfileID: f.profile.ID, AccessToken: accessToken, RefreshToken: fakeOAuthRefreshToken,
		ExpiresAt: expiresAt, AccountID: fakeOAuthAccountID,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// codexBackend 는 Codex 백엔드처럼 /responses 에 짧은 SSE 를, /models 에 모델 목록을 준다.
type codexBackend struct {
	mu       sync.Mutex
	requests []*http.Request
	status   int // 0 이면 200
}

func (b *codexBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	b.mu.Lock()
	b.requests = append(b.requests, r.Clone(context.Background()))
	status := b.status
	b.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"detail":"rejected"}`)
		return
	}
	switch r.URL.Path {
	case "/responses":
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	case "/models":
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-5.5","visibility":"list","priority":1},`+
			`{"slug":"internal-hidden","visibility":"hide"},{"slug":"gpt-5.5-mini","visibility":"list"}]}`)
	default:
		http.NotFound(w, r)
	}
}

func (b *codexBackend) lastRequest(t *testing.T) *http.Request {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		t.Fatal("codex backend got no request")
	}
	return b.requests[len(b.requests)-1]
}

func TestOAuthStoreLoadWithoutCredentialsIsNoTokens(t *testing.T) {
	f := newOAuthFixture(t, nil)
	store := oauthStore{pg: f.pg, cipher: f.reg.cipher, profileID: f.profile.ID}
	_, err := store.Load(context.Background())
	if !errors.Is(err, llmauth.ErrNoTokens) || !errors.Is(err, db.ErrOAuthCredentialsNotFound) {
		t.Fatalf("Load error = %v, want ErrNoTokens wrapping ErrOAuthCredentialsNotFound", err)
	}
	_, err = store.Refresh(context.Background(), "stale", func(context.Context, llmauth.Tokens) (llmauth.Tokens, error) {
		t.Fatal("refresh called without stored credentials")
		return llmauth.Tokens{}, nil
	})
	if !errors.Is(err, llmauth.ErrNoTokens) {
		t.Fatalf("Refresh error = %v, want ErrNoTokens", err)
	}
}

// TestOAuthStoreRefreshPassesDeadline 는 행 잠금을 쥔 채 부르는 갱신 함수에 호출자 ctx 의
// 시간 상한이 그대로 가는지, 받은 토큰이 저장되는지 본다.
func TestOAuthStoreRefreshPassesDeadline(t *testing.T) {
	f := newOAuthFixture(t, nil)
	f.connect(t, time.Now().Add(time.Hour))
	store := oauthStore{pg: f.pg, cipher: f.reg.cipher, profileID: f.profile.ID}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	newExpiry := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	got, err := store.Refresh(ctx, fakeOAuthAccessToken, func(ctx context.Context, current llmauth.Tokens) (llmauth.Tokens, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(want) {
			t.Errorf("refresh ctx deadline = %v (set %v), want %v", deadline, ok, want)
		}
		if current.RefreshToken != fakeOAuthRefreshToken {
			t.Errorf("refresh got the wrong refresh token")
		}
		return llmauth.Tokens{AccessToken: "rotated-access", RefreshToken: "rotated-refresh",
			AccountID: fakeOAuthAccountID, ExpiresAt: newExpiry}, nil
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.AccessToken != "rotated-access" {
		t.Fatalf("Refresh returned the old access token")
	}
	stored, err := f.pg.OAuthCredentials(context.Background(), f.reg.cipher, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "rotated-access" || stored.RefreshToken != "rotated-refresh" || !stored.ExpiresAt.Equal(newExpiry) {
		t.Fatalf("stored credentials were not rotated (expires %v)", stored.ExpiresAt)
	}
}

// TestOAuthRefreshRecordsLoginRequired 는 갱신이 재로그인 필요로 거절되면 목록 응답에 needs_login 이
// 보이고, 다음 갱신이 성공하면 지워지는지 본다.
func TestOAuthRefreshRecordsLoginRequired(t *testing.T) {
	var mu sync.Mutex
	rejectRefresh := true
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reject := rejectRefresh
		mu.Unlock()
		if reject {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  fakeAccessJWT(t, time.Now().Add(time.Hour), "refreshed"),
			"refresh_token": "rotated-refresh-token",
		})
	}))
	defer issuer.Close()
	f := newOAuthFixture(t, issuer)
	logs := captureLogs(t)
	// 만료가 5분 안이라 Token 이 곧바로 갱신한다.
	f.connect(t, time.Now().Add(time.Minute))
	src := f.reg.source(f.profile.ID)

	if _, _, err := src.Token(context.Background()); err == nil {
		t.Fatal("Token succeeded although the refresh was rejected")
	}
	if !f.reg.loginRequired(f.profile.ID) {
		t.Fatal("rejected refresh did not mark the profile as needing login")
	}
	if body := listProfilesBody(t, f.s); !strings.Contains(body, `"needs_login":true`) {
		t.Fatalf("profile list does not report needs_login:\n%s", body)
	}

	// 다시 로그인한 것처럼 새 자격 증명을 저장하면 TokenSource 가 그것을 읽어 갱신에 성공한다.
	mu.Lock()
	rejectRefresh = false
	mu.Unlock()
	const relogin = "fake-access-token-issue39-relogin"
	f.connectWith(t, relogin, time.Now().Add(time.Minute))
	if _, _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token after new login: %v", err)
	}
	if f.reg.loginRequired(f.profile.ID) {
		t.Fatal("successful refresh did not clear needs_login")
	}
	assertNoTokens(t, "logs", logs.String(), "rotated-refresh-token", relogin)
}

func TestOAuthRegistryReusesTokenSourcePerProfile(t *testing.T) {
	reg := newOAuthTokenRegistry(nil, nil, &llmauth.Client{})
	first := reg.source(1)
	if reg.source(1) != first {
		t.Fatal("registry built a second TokenSource for the same profile")
	}
	if reg.source(2) == first {
		t.Fatal("two profiles share one TokenSource")
	}
	reg.forget(1)
	if reg.source(1) == first {
		t.Fatal("forget kept the old TokenSource")
	}
	var none *oauthTokenRegistry
	if none.source(1) != nil || none.loginRequired(1) {
		t.Fatal("nil registry offered an OAuth token source")
	}
	if _, err := none.connectedSource(context.Background(), 1); err == nil {
		t.Fatal("nil registry reported a connected profile")
	}
}

func TestLoadOAuthTokenRegistryUsesKeyDir(t *testing.T) {
	pg := &db.DB{} // 키 로드만 보므로 연결은 쓰지 않는다
	dir := t.TempDir()
	if reg := loadOAuthTokenRegistry(pg, dir); reg == nil {
		t.Fatal("registry is nil with a fresh key directory")
	}
	info, err := os.Stat(filepath.Join(dir, db.CredentialKeyFilename))
	if err != nil {
		t.Fatalf("key file was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}

	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, db.CredentialKeyFilename), []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	if reg := loadOAuthTokenRegistry(pg, badDir); reg != nil {
		t.Fatal("registry was built from a malformed key file")
	}
	if !strings.Contains(logs.String(), "chatgpt_oauth profiles disabled") {
		t.Fatalf("malformed key was not logged: %q", logs.String())
	}
}

// TestProfileConfigChatGPTOAuth 는 OAuth 프로필의 설정이 형식·스트리밍을 강제하고, 같은 TokenSource 를
// 재사용하며, providerForProfile 의 프로바이더가 저장된 토큰으로 Codex 백엔드를 부르는지 본다.
func TestProfileConfigChatGPTOAuth(t *testing.T) {
	f := newOAuthFixture(t, nil)
	backend := &codexBackend{}
	ts := httptest.NewServer(backend)
	defer ts.Close()
	f.profile.BaseURL = ts.URL
	if _, err := f.pg.SaveProfile(f.profile); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)

	if _, ok := f.s.loadProfileConfig(f.profile.ID); ok {
		t.Fatal("profile without stored credentials is usable")
	}
	f.connect(t, time.Now().Add(time.Hour))
	cfg, ok := f.s.loadProfileConfig(f.profile.ID)
	if !ok {
		t.Fatal("connected OAuth profile is not usable")
	}
	if cfg.AuthType != db.AuthChatGPTOAuth || cfg.Format != llm.FormatOpenAIResponses || !cfg.Stream || cfg.OAuthTokens == nil {
		t.Fatalf("cfg auth=%q format=%q stream=%v tokens=%v, want chatgpt_oauth/openai-responses/true/set",
			cfg.AuthType, cfg.Format, cfg.Stream, cfg.OAuthTokens != nil)
	}
	again, _ := f.s.loadProfileConfig(f.profile.ID)
	if again != cfg {
		t.Fatal("two loads of the same OAuth profile differ (TokenSource not reused)")
	}

	prov, _, ok := f.s.providerForProfile(f.profile.ID)
	if !ok {
		t.Fatal("providerForProfile failed for a connected OAuth profile")
	}
	var reply strings.Builder
	for ev, err := range prov.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.Type == llm.SETextDelta {
			reply.WriteString(ev.Text)
		}
	}
	if reply.String() != "OK" {
		t.Fatalf("reply = %q, want OK", reply.String())
	}
	req := backend.lastRequest(t)
	if req.URL.Path != "/responses" || req.Header.Get("Authorization") != "Bearer "+fakeOAuthAccessToken ||
		req.Header.Get("ChatGPT-Account-ID") != fakeOAuthAccountID {
		t.Fatalf("backend got %s with account %q (bearer match %v)", req.URL.Path,
			req.Header.Get("ChatGPT-Account-ID"), req.Header.Get("Authorization") == "Bearer "+fakeOAuthAccessToken)
	}
	assertNoTokens(t, "logs", logs.String())
}

func postJSON(t *testing.T, handler http.HandlerFunc, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
	return rec.Code, rec.Body.String()
}

func listProfilesBody(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.pgListProfiles(rec, httptest.NewRequest(http.MethodGet, "/api/llm/profiles", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list profiles status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestTestLLMChatGPTOAuth(t *testing.T) {
	f := newOAuthFixture(t, nil)
	backend := &codexBackend{}
	ts := httptest.NewServer(backend)
	defer ts.Close()
	logs := captureLogs(t)
	id := f.profile.ID

	cases := []struct {
		name      string
		connected bool
		body      map[string]any
		wantOK    bool
		wantError string
	}{
		{"unsaved oauth profile", false, map[string]any{"auth_type": "chatgpt_oauth", "model": "gpt-5.5"}, false, chatGPTSaveFirstMessage},
		{"not logged in", false, map[string]any{"profile_id": id, "model": "gpt-5.5", "base_url": ts.URL}, false, chatGPTLoginRequiredMessage},
		// 화면이 형식을 anthropic·비스트리밍으로 보내도 OAuth 규칙으로 바꿔 Codex 백엔드를 부른다.
		{"connected", true, map[string]any{"profile_id": id, "provider": "anthropic", "streaming": false, "model": "gpt-5.5", "base_url": ts.URL}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.connected {
				f.connect(t, time.Now().Add(time.Hour))
			}
			code, body := postJSON(t, f.s.testLLM, tc.body)
			var got struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil || code != http.StatusOK {
				t.Fatalf("status %d body %s", code, body)
			}
			if got.OK != tc.wantOK || got.Error != tc.wantError {
				t.Fatalf("ok=%v error=%q, want ok=%v error=%q", got.OK, got.Error, tc.wantOK, tc.wantError)
			}
			assertNoTokens(t, "response", body)
		})
	}
	if req := backend.lastRequest(t); req.URL.Path != "/responses" || req.Header.Get("Authorization") != "Bearer "+fakeOAuthAccessToken {
		t.Fatalf("connection test did not reach the Codex backend with the stored token (path %s)", req.URL.Path)
	}
	assertNoTokens(t, "logs", logs.String())
}

func TestListModelsChatGPTOAuth(t *testing.T) {
	f := newOAuthFixture(t, nil)
	backend := &codexBackend{}
	ts := httptest.NewServer(backend)
	defer ts.Close()
	logs := captureLogs(t)
	id := f.profile.ID
	type result struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
		Error  string   `json:"error"`
	}
	list := func(t *testing.T, body map[string]any) result {
		t.Helper()
		code, raw := postJSON(t, f.s.pgListModels, body)
		var got result
		if err := json.Unmarshal([]byte(raw), &got); err != nil || code != http.StatusOK {
			t.Fatalf("status %d body %s", code, raw)
		}
		assertNoTokens(t, "response", raw)
		return got
	}

	if got := list(t, map[string]any{"profile_id": id, "base_url": ts.URL}); got.OK || got.Error != chatGPTLoginRequiredMessage || got.Models == nil {
		t.Fatalf("not logged in: %+v", got)
	}
	f.connect(t, time.Now().Add(time.Hour))

	got := list(t, map[string]any{"profile_id": id, "base_url": ts.URL + "/responses"})
	if !got.OK || strings.Join(got.Models, ",") != "gpt-5.5,gpt-5.5-mini" {
		t.Fatalf("models = %+v, want the two listed models", got)
	}
	req := backend.lastRequest(t)
	if req.URL.Path != "/models" || req.URL.Query().Get("client_version") != codexClientVersion ||
		req.Header.Get("Authorization") != "Bearer "+fakeOAuthAccessToken || req.Header.Get("ChatGPT-Account-ID") != fakeOAuthAccountID {
		t.Fatalf("models request %s?%s account %q", req.URL.Path, req.URL.RawQuery, req.Header.Get("ChatGPT-Account-ID"))
	}

	failures := []struct {
		name   string
		status int
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, chatGPTLoginRequiredMessage},
		{"server error", http.StatusInternalServerError, chatGPTModelsFailedMessage},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			backend.mu.Lock()
			backend.status = tc.status
			backend.mu.Unlock()
			got := list(t, map[string]any{"profile_id": id, "base_url": ts.URL})
			if got.OK || got.Error != tc.want || got.Models == nil || len(got.Models) != 0 {
				t.Fatalf("got %+v, want empty list and %q", got, tc.want)
			}
		})
	}
	assertNoTokens(t, "logs", logs.String())
}

func TestSaveProfileValidatesAuthType(t *testing.T) {
	f := newOAuthFixture(t, nil)
	cases := []struct {
		name       string
		authType   string
		wantStatus int
	}{
		{"unknown", "bogus_auth", http.StatusBadRequest},
		{"api key", "api_key", http.StatusOK},
		{"chatgpt oauth", "chatgpt_oauth", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"name": "t-issue39-save-" + tc.authType, "format": "anthropic", "model": "m",
				"streaming": false, "auth_type": tc.authType, "api_key": "sk-issue39-should-not-leak"}
			code, raw := postJSON(t, f.s.pgSaveProfile, body)
			if code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", code, tc.wantStatus, raw)
			}
			if code != http.StatusOK {
				if strings.Contains(raw, "bogus_auth") || !strings.Contains(raw, "auth_type must be api_key or chatgpt_oauth") {
					t.Fatalf("rejection body = %s", raw)
				}
				return
			}
			var saved struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal([]byte(raw), &saved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, saved.ID) })
			p, err := f.pg.ProfileByID(saved.ID)
			if err != nil || p == nil {
				t.Fatalf("ProfileByID: %v", err)
			}
			if p.AuthType != db.AuthType(tc.authType) {
				t.Fatalf("auth_type = %q", p.AuthType)
			}
			isOAuth := p.AuthType == db.AuthChatGPTOAuth
			if isOAuth && (p.Format != "openai-responses" || !p.Streaming || p.APIKey != "") {
				t.Fatalf("oauth profile saved as format=%q streaming=%v with api key %v", p.Format, p.Streaming, p.APIKey != "")
			}
			if !isOAuth && p.Format != "anthropic" {
				t.Fatalf("api key profile format changed to %q", p.Format)
			}
		})
	}
}

func TestListProfilesShowsOAuthStateWithoutTokens(t *testing.T) {
	f := newOAuthFixture(t, nil)
	expiresAt := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	f.connect(t, expiresAt)
	body := listProfilesBody(t, f.s)
	assertNoTokens(t, "profile list", body)

	var got struct {
		Profiles []LLMProfileDTO `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	var found *LLMProfileDTO
	for i := range got.Profiles {
		if got.Profiles[i].Name == f.profile.Name {
			found = &got.Profiles[i]
		}
	}
	if found == nil {
		t.Fatalf("profile %q not in list", f.profile.Name)
	}
	if found.AuthType != db.AuthChatGPTOAuth || found.OAuth == nil || !found.OAuth.Connected ||
		!found.OAuth.ExpiresAt.Equal(expiresAt) || found.OAuth.NeedsLogin {
		t.Fatalf("profile dto auth=%q oauth=%+v, want connected, expires %v, no relogin", found.AuthType, found.OAuth, expiresAt)
	}
}
