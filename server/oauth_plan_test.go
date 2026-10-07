package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// fakePlanJWT 는 계정·만료·플랜 클레임만 담은 서명 없는 JWT 다.
func fakePlanJWT(t *testing.T, exp time.Time, plan string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"exp": exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": fakeOAuthAccountID, "chatgpt_plan_type": plan,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// listedOAuth 는 목록 응답에서 픽스처 프로필의 OAuth 상태를 꺼낸다.
func listedOAuth(t *testing.T, f *oauthFixture) *LLMProfileOAuthDTO {
	t.Helper()
	body := listProfilesBody(t, f.s)
	assertNoTokens(t, "profile list", body)
	var got struct {
		Profiles []LLMProfileDTO `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Profiles {
		if p.Name == f.profile.Name {
			return p.OAuth
		}
	}
	t.Fatalf("profile %q not in list", f.profile.Name)
	return nil
}

// TestOAuthRefreshStoresPlanAndListShowsIt 는 갱신으로 받은 플랜이 저장되고 목록 DTO 에 보이며,
// 레지스트리가 없으면 플랜도 빠지는지 본다.
func TestOAuthRefreshStoresPlanAndListShowsIt(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  fakePlanJWT(t, time.Now().Add(time.Hour), "pro"),
			"refresh_token": "rotated-refresh-token-issue48",
		})
	}))
	defer issuer.Close()
	f := newOAuthFixture(t, issuer)
	logs := captureLogs(t)
	err := f.pg.SaveOAuthCredentials(context.Background(), f.reg.cipher, db.OAuthCredentials{
		ProfileID: f.profile.ID, AccessToken: fakeOAuthAccessToken, RefreshToken: fakeOAuthRefreshToken,
		ExpiresAt: time.Now().Add(time.Minute), AccountID: fakeOAuthAccountID, PlanType: "plus",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state := listedOAuth(t, f); state == nil || state.Plan != "plus" {
		t.Fatalf("oauth state before refresh = %+v, want plan plus", state)
	}

	// 만료가 5분 안이라 Token 이 곧바로 갱신한다.
	if _, _, err := f.reg.source(f.profile.ID).Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	stored, err := f.pg.OAuthCredentials(context.Background(), f.reg.cipher, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PlanType != "pro" {
		t.Fatalf("stored plan after refresh = %q, want pro", stored.PlanType)
	}
	if state := listedOAuth(t, f); state == nil || state.Plan != "pro" {
		t.Fatalf("oauth state after refresh = %+v, want plan pro", state)
	}

	f.s.oauth = nil // oauth.key 를 쓰지 못해 레지스트리가 없는 서버
	if state := listedOAuth(t, f); state == nil || state.Connected || state.Plan != "" {
		t.Fatalf("oauth state without registry = %+v, want disconnected without plan", state)
	}
	assertNoTokens(t, "logs", logs.String(), "rotated-refresh-token-issue48")
}

// TestSaveProfileOAuthSwitchSurvivesCanceledRequest 는 요청 ctx 가 끊겨도 API 키 프로필을 OAuth 로
// 바꾸는 저장이 키를 남기지 않고 끝나는지 본다. 예전에는 키 지우기만 요청 ctx 를 써서 500 과 함께
// 키가 남았다.
func TestSaveProfileOAuthSwitchSurvivesCanceledRequest(t *testing.T) {
	f := newOAuthFixture(t, nil)
	id, err := f.pg.SaveProfile(&db.LLMProfile{Name: "t-issue48-cancel", Format: "openai", Model: "m",
		BaseURL: "http://relay.example.test/v1", APIKey: "sk-issue48-cancel-key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	raw, err := json.Marshal(map[string]any{"id": id, "name": "t-issue48-cancel", "format": "openai", "model": "m",
		"auth_type": "chatgpt_oauth"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", strings.NewReader(string(raw))).WithContext(ctx)
	rec := httptest.NewRecorder()
	f.s.pgSaveProfile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status %d: %s", rec.Code, rec.Body.String())
	}
	var apiKey, hint string
	if err := f.pg.QueryRow(`SELECT COALESCE(api_key,''),COALESCE(api_key_hint,'') FROM llm_profiles WHERE id=$1`, id).Scan(&apiKey, &hint); err != nil {
		t.Fatal(err)
	}
	if apiKey != "" || hint != "" {
		t.Fatalf("key left=%v hint=%q after switching to oauth", apiKey != "", hint)
	}
}

// TestLegacySetLLMRejectsOAuthDefaultProfile 는 레거시 POST /api/llm 이 chatgpt_oauth 인 "default"
// 프로필을 API 키 설정으로 덮지 않고 400 을 돌려주는지 본다.
func TestLegacySetLLMRejectsOAuthDefaultProfile(t *testing.T) {
	f := newOAuthFixture(t, nil)
	var others int
	if err := f.pg.QueryRow(`SELECT count(*) FROM llm_profiles WHERE name='default'`).Scan(&others); err != nil {
		t.Fatal(err)
	}
	if others > 0 {
		t.Skip("database already has a profile named default; this test needs to own it")
	}
	if _, err := f.pg.Exec(`UPDATE llm_profiles SET name='default' WHERE id=$1`, f.profile.ID); err != nil {
		t.Fatal(err)
	}

	code, body := postJSON(t, f.s.setLLM, map[string]any{"provider": "openai", "model": "m",
		"base_url": "http://legacy.example.test/v1", "api_key": "sk-issue48-legacy-key"})
	if code != http.StatusBadRequest || !strings.Contains(body, legacyOAuthProfileMessage) {
		t.Fatalf("setLLM status %d body %s, want 400 with the fixed message", code, body)
	}
	var authType, baseURL, apiKey string
	if err := f.pg.QueryRow(`SELECT auth_type,COALESCE(base_url,''),COALESCE(api_key,'') FROM llm_profiles WHERE id=$1`,
		f.profile.ID).Scan(&authType, &baseURL, &apiKey); err != nil {
		t.Fatal(err)
	}
	if authType != string(db.AuthChatGPTOAuth) || baseURL != "" || apiKey != "" {
		t.Fatalf("oauth row changed: auth=%q base_url=%q key left=%v", authType, baseURL, apiKey != "")
	}
}

// TestPoolMemberShowsOAuthFormat 는 저장된 형식이 예전 값인 chatgpt_oauth 행도 풀 멤버에 실제 형식으로
// 보이는지 본다.
func TestPoolMemberShowsOAuthFormat(t *testing.T) {
	f := newOAuthFixture(t, nil)
	f.connect(t, time.Now().Add(time.Hour))
	// 픽스처는 형식을 일부러 anthropic 으로 저장한다.
	m := f.s.poolMember(f.profile, 0)
	if m == nil {
		t.Fatal("connected OAuth profile did not become a pool member")
	}
	if m.Format != "openai-responses" {
		t.Fatalf("pool member format = %q, want openai-responses", m.Format)
	}
}
