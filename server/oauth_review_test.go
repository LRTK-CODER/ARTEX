package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
	"github.com/Autumn-27/norma/llm"
)

// TestTaskResolutionChatGPTOAuth 는 작업 LLM 해석이 실제 실행 경로와 같은 판단을 하는지 본다.
// 연결된 OAuth 프로필은 API 키가 없어도 쓸 수 있고, 형식은 실제 형식으로 보인다.
func TestTaskResolutionChatGPTOAuth(t *testing.T) {
	f := newOAuthFixture(t, nil)
	cases := []struct {
		name          string
		connect       bool
		wantAvailable bool
		wantReason    string
	}{
		{"not logged in", false, false, chatGPTLoginRequiredMessage},
		{"connected", true, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.connect {
				f.connect(t, time.Now().Add(time.Hour))
			}
			p, err := f.pg.ProfileByID(f.profile.ID)
			if err != nil || p == nil {
				t.Fatalf("ProfileByID: %v", err)
			}
			got := f.s.resolutionFromProfile(p, "agent_binding")
			if got.Available != tc.wantAvailable || got.Reason != tc.wantReason || got.Format != string(llm.FormatOpenAIResponses) {
				t.Fatalf("resolution available=%v reason=%q format=%q, want %v %q openai-responses",
					got.Available, got.Reason, got.Format, tc.wantAvailable, tc.wantReason)
			}
		})
	}
}

// TestOAuthTokensStayOnCodexHost 는 구독 토큰이 서버가 정한 Codex 주소로만 가는지 본다.
// 요청의 base_url과 DB에 남은 BaseURL(예: API 키용 중계)이 다른 호스트를 가리켜도 그 호스트는
// 요청을 하나도 받지 않아야 한다.
func TestOAuthTokensStayOnCodexHost(t *testing.T) {
	f := newOAuthFixture(t, nil)
	backend := &codexBackend{}
	codex := httptest.NewServer(backend)
	defer codex.Close()
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer other.Close()
	f.s.codexBaseURL = codex.URL
	// 저장 API는 이제 BaseURL을 비우므로 예전 행처럼 DB에 직접 넣는다.
	if _, err := f.pg.Exec(`UPDATE llm_profiles SET base_url=$1 WHERE id=$2`, other.URL, f.profile.ID); err != nil {
		t.Fatal(err)
	}
	f.connect(t, time.Now().Add(time.Hour))
	id := f.profile.ID

	if code, body := postJSON(t, f.s.testLLM, map[string]any{"profile_id": id, "model": "gpt-5.5", "base_url": other.URL}); code != http.StatusOK || !json.Valid([]byte(body)) {
		t.Fatalf("test llm: %d %s", code, body)
	}
	if code, body := postJSON(t, f.s.pgListModels, map[string]any{"profile_id": id, "base_url": other.URL}); code != http.StatusOK || !json.Valid([]byte(body)) {
		t.Fatalf("list models: %d %s", code, body)
	}
	prov, _, ok := f.s.providerForProfile(id)
	if !ok {
		t.Fatal("providerForProfile failed")
	}
	for _, err := range prov.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hi")}}) {
		if err != nil {
			break
		}
	}

	if n := otherHits.Load(); n != 0 {
		t.Fatalf("other host received %d requests carrying the subscription token", n)
	}
	backend.mu.Lock()
	codexCalls := len(backend.requests)
	backend.mu.Unlock()
	if codexCalls < 3 {
		t.Fatalf("codex backend got %d requests, want test + models + stream", codexCalls)
	}
}

// TestSaveProfileOAuthFixedRules 는 OAuth로 저장하거나 바꾸면 BaseURL·API 키·힌트가 지워지고,
// auth_type 없이 수정해도 기존 OAuth 행에 같은 고정 규칙이 적용되는지 본다.
func TestSaveProfileOAuthFixedRules(t *testing.T) {
	f := newOAuthFixture(t, nil)
	const oldKey = "sk-issue39-old-relay-key"
	save := func(t *testing.T, body map[string]any) int64 {
		t.Helper()
		code, raw := postJSON(t, f.s.pgSaveProfile, body)
		if code != http.StatusOK {
			t.Fatalf("save status %d: %s", code, raw)
		}
		var saved struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(raw), &saved); err != nil {
			t.Fatal(err)
		}
		return saved.ID
	}
	id := save(t, map[string]any{"name": "t-issue39-switch", "format": "openai", "model": "m",
		"base_url": "http://relay.example.test/v1", "api_key": oldKey, "auth_type": "api_key"})
	t.Cleanup(func() { _, _ = f.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	steps := []struct {
		name string
		body map[string]any
	}{
		{"switch api key to oauth", map[string]any{"id": id, "name": "t-issue39-switch", "format": "openai", "model": "m",
			"base_url": "http://relay.example.test/v1", "auth_type": "chatgpt_oauth"}},
		{"edit oauth without auth_type", map[string]any{"id": id, "name": "t-issue39-switch", "format": "anthropic", "model": "m",
			"streaming": false, "base_url": "http://other.example.test"}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			save(t, step.body)
			var authType, format, baseURL, apiKey, hint string
			var isStreaming bool
			err := f.pg.QueryRow(`SELECT auth_type, format, COALESCE(base_url,''), COALESCE(api_key,''), COALESCE(api_key_hint,''), streaming
FROM llm_profiles WHERE id=$1`, id).Scan(&authType, &format, &baseURL, &apiKey, &hint, &isStreaming)
			if err != nil {
				t.Fatal(err)
			}
			if authType != string(db.AuthChatGPTOAuth) || format != "openai-responses" || !isStreaming ||
				baseURL != "" || apiKey != "" || hint != "" {
				t.Fatalf("row auth=%q format=%q streaming=%v base_url=%q key left=%v hint=%q",
					authType, format, isStreaming, baseURL, apiKey != "", hint)
			}
		})
	}
}

func TestListProfilesWithoutRegistryShowsDisconnected(t *testing.T) {
	f := newOAuthFixture(t, nil)
	f.connect(t, time.Now().Add(time.Hour))
	f.s.oauth = nil // oauth.key를 쓰지 못해 레지스트리가 없는 서버
	var got struct {
		Profiles []LLMProfileDTO `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(listProfilesBody(t, f.s)), &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Profiles {
		if p.Name != f.profile.Name {
			continue
		}
		if p.OAuth == nil || p.OAuth.Connected || p.OAuth.NeedsLogin {
			t.Fatalf("oauth state = %+v, want disconnected without a credential key", p.OAuth)
		}
		return
	}
	t.Fatalf("profile %q not in list", f.profile.Name)
}

func TestDeleteProfileForgetsTokenSource(t *testing.T) {
	f := newOAuthFixture(t, nil)
	f.s.llmHealth = newLLMHealthRegistry(f.pg) // 삭제 핸들러가 장애 전환 상태도 비운다
	before := f.reg.source(f.profile.ID)
	req := httptest.NewRequest(http.MethodDelete, "/api/llm/profiles/"+strconv.FormatInt(f.profile.ID, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(f.profile.ID, 10))
	rec := httptest.NewRecorder()
	f.s.pgDeleteProfile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}
	if f.reg.source(f.profile.ID) == before {
		t.Fatal("deleted profile kept its TokenSource")
	}
}

// TestOAuthRegistryConcurrentAccess 는 여러 goroutine이 레지스트리를 함께 써도 같은 프로필에
// TokenSource가 하나만 남는지 본다. 경쟁 상태는 go test -race가 잡는다.
func TestOAuthRegistryConcurrentAccess(t *testing.T) {
	reg := newOAuthTokenRegistry(nil, nil, &llmauth.Client{})
	loginErr := &llmauth.TokenError{Op: "refresh", StatusCode: http.StatusBadRequest, Code: llmauth.ErrorCodeInvalidGrant}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := int64(i % 4)
			switch i % 4 {
			case 0:
				reg.source(id)
			case 1:
				reg.recordRefresh(id, loginErr)
			case 2:
				reg.loginRequired(id)
			default:
				reg.forget(id)
				reg.recordRefresh(id, errors.New("network"))
			}
		}(i)
	}
	wg.Wait()

	sources := make(chan *llmauth.TokenSource, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sources <- reg.source(99)
		}()
	}
	wg.Wait()
	close(sources)
	first := reg.source(99)
	for src := range sources {
		if src != first {
			t.Fatal("concurrent callers got different TokenSources for one profile")
		}
	}
}
