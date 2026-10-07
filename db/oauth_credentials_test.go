package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	fakeAccessToken  = "fake-access-token-1f2e3d"
	fakeRefreshToken = "fake-refresh-token-9a8b7c"
)

func testCipher(t *testing.T) *TokenCipher {
	t.Helper()
	key, err := LoadOrCreateCredentialKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewTokenCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadOrCreateCredentialKey(t *testing.T) {
	t.Run("새 키 파일은 0600이고 다시 읽으면 같은 키", func(t *testing.T) {
		dir := t.TempDir()
		key, err := LoadOrCreateCredentialKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, CredentialKeyFilename))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("key file perm = %o, want 600", perm)
		}
		again, err := LoadOrCreateCredentialKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(key, again) {
			t.Fatal("second load returned a different key")
		}
		leftovers, err := filepath.Glob(filepath.Join(dir, ".*tmp*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(leftovers) != 0 {
			t.Fatalf("temp files left behind: %v", leftovers)
		}
	})

	t.Run("형식이 틀린 키 파일은 덮어쓰지 않고 오류", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, CredentialKeyFilename)
		if err := os.WriteFile(path, []byte("not-hex"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateCredentialKey(dir); err == nil {
			t.Fatal("malformed key file accepted")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "not-hex" {
			t.Fatalf("existing key file was overwritten: %q", data)
		}
	})

	t.Run("디렉터리에 쓸 수 없으면 오류", func(t *testing.T) {
		if _, err := LoadOrCreateCredentialKey(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("expected error for missing directory")
		}
	})
}

func TestNewTokenCipherRejectsWrongKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := NewTokenCipher(make([]byte, size)); err == nil {
			t.Fatalf("key of %d bytes accepted", size)
		}
	}
}

func TestOAuthCredentialsJSONHasNoTokens(t *testing.T) {
	out, err := json.Marshal(OAuthCredentials{ProfileID: 1, AccessToken: fakeAccessToken, RefreshToken: fakeRefreshToken, AccountID: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fakeAccessToken, fakeRefreshToken} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("token leaked into JSON: %s", out)
		}
	}
}

// oauthTestProfile 은 chatgpt_oauth 프로필을 만들고 테스트가 끝나면 지운다.
func oauthTestProfile(t *testing.T, d *DB, name string) int64 {
	t.Helper()
	id, err := d.SaveProfile(&LLMProfile{Name: name, Format: "openai-responses", Model: "m", AuthType: AuthChatGPTOAuth})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	return id
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestSchemaAppliesTwice(t *testing.T) {
	first := openTestDB(t)
	second, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("second schema apply: %v", err)
	}
	defer second.Close()
	if _, err := first.Exec(`INSERT INTO llm_profiles(name,format,model,auth_type) VALUES ('t-oauth-badauth','openai','m','bogus')`); err == nil {
		first.Exec(`DELETE FROM llm_profiles WHERE name='t-oauth-badauth'`)
		t.Fatal("auth_type CHECK accepted an unknown value")
	}
	var authType string
	if err := first.QueryRow(`SELECT column_default FROM information_schema.columns WHERE table_name='llm_profiles' AND column_name='auth_type'`).Scan(&authType); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authType, "api_key") {
		t.Fatalf("auth_type default = %q, want api_key", authType)
	}
}

func TestOAuthCredentialsStoredEncrypted(t *testing.T) {
	d := openTestDB(t)
	c := testCipher(t)
	id := oauthTestProfile(t, d, "t-oauth-encrypted")
	expiresAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: id, AccessToken: fakeAccessToken, RefreshToken: fakeRefreshToken, ExpiresAt: expiresAt, AccountID: "acct-1"}); err != nil {
		t.Fatal(err)
	}

	var rawAccess, rawRefresh string
	if err := d.QueryRow(`SELECT access_token,refresh_token FROM llm_oauth_credentials WHERE profile_id=$1`, id).Scan(&rawAccess, &rawRefresh); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{rawAccess, rawRefresh} {
		if strings.Contains(raw, fakeAccessToken) || strings.Contains(raw, fakeRefreshToken) {
			t.Fatalf("plaintext token stored in DB: %q", raw)
		}
	}

	got, err := d.OAuthCredentials(t.Context(), c, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != fakeAccessToken || got.RefreshToken != fakeRefreshToken || got.AccountID != "acct-1" || !got.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("round trip = %+v", got)
	}

	if _, err := d.OAuthCredentials(t.Context(), testCipher(t), id); !errors.Is(err, ErrCredentialDecrypt) {
		t.Fatalf("other key: err = %v, want ErrCredentialDecrypt", err)
	}

	// 다른 프로필 행에 암호문을 옮겨 넣어도 풀리지 않아야 한다(AAD 로 행에 묶임).
	other := oauthTestProfile(t, d, "t-oauth-encrypted-other")
	if _, err := d.Exec(`INSERT INTO llm_oauth_credentials(profile_id,access_token,refresh_token,expires_at) VALUES ($1,$2,$3,now())`, other, rawAccess, rawRefresh); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OAuthCredentials(t.Context(), c, other); !errors.Is(err, ErrCredentialDecrypt) {
		t.Fatalf("copied ciphertext: err = %v, want ErrCredentialDecrypt", err)
	}

	missing := oauthTestProfile(t, d, "t-oauth-encrypted-missing")
	if _, err := d.OAuthCredentials(t.Context(), c, missing); !errors.Is(err, ErrOAuthCredentialsNotFound) {
		t.Fatalf("missing: err = %v, want ErrOAuthCredentialsNotFound", err)
	}
}

// waitForLockWaiter 는 다른 세션이 행 잠금을 기다리기 시작할 때까지 기다린다.
// 첫 갱신을 그 뒤에 풀어야 두 번째 갱신이 FOR UPDATE 에서 실제로 막힌 경우를 시험한다.
func waitForLockWaiter(t *testing.T, d *DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int
		if err := d.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE wait_event_type='Lock' AND query LIKE '%FROM llm_oauth_credentials%FOR UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("second refresh never blocked on the row lock")
		case <-tick.C:
		}
	}
}

func TestRefreshOAuthCredentialsConcurrentCallsRefreshOnce(t *testing.T) {
	d := openTestDB(t)
	c := testCipher(t)
	id := oauthTestProfile(t, d, "t-oauth-refresh")
	if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: id, AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	refresh := func(ctx context.Context, current OAuthCredentials) (OAuthCredentials, error) {
		n := calls.Add(1)
		if current.RefreshToken != "old-refresh" {
			return OAuthCredentials{}, errors.New("refresh token already rotated")
		}
		if n == 1 {
			close(entered)
			<-release
		}
		return OAuthCredentials{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	results := make([]OAuthCredentials, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	run := func(i int) {
		defer wg.Done()
		results[i], errs[i] = d.RefreshOAuthCredentials(t.Context(), c, id, "old-access", refresh)
	}
	wg.Add(1)
	go run(0)
	<-entered
	wg.Add(1)
	go run(1)
	waitForLockWaiter(t, d)
	close(release)
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("refresh %d: %v", i, errs[i])
		}
		if results[i].AccessToken != "new-access" {
			t.Fatalf("refresh %d got access %q, want new-access", i, results[i].AccessToken)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("refresh called %d times, want 1", n)
	}
	stored, err := d.OAuthCredentials(t.Context(), c, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "new-refresh" {
		t.Fatalf("stored refresh = %q, want new-refresh", stored.RefreshToken)
	}
}

func TestRefreshOAuthCredentialsFailureKeepsStored(t *testing.T) {
	d := openTestDB(t)
	c := testCipher(t)
	id := oauthTestProfile(t, d, "t-oauth-refresh-fail")
	if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: id, AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	refreshErr := errors.New("token endpoint down")
	_, err := d.RefreshOAuthCredentials(t.Context(), c, id, "a", func(context.Context, OAuthCredentials) (OAuthCredentials, error) {
		return OAuthCredentials{}, refreshErr
	})
	if !errors.Is(err, refreshErr) {
		t.Fatalf("err = %v, want %v", err, refreshErr)
	}
	stored, err := d.OAuthCredentials(t.Context(), c, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "a" || stored.RefreshToken != "r" {
		t.Fatalf("stored changed after failed refresh: %+v", stored)
	}
}

func TestPoolProfilesIncludesConnectedOAuthProfile(t *testing.T) {
	d := openTestDB(t)
	c := testCipher(t)
	connected := oauthTestProfile(t, d, "t-oauth-pool-connected")
	oauthTestProfile(t, d, "t-oauth-pool-unconnected")
	if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: connected, AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	chain, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var hasConnected bool
	for _, p := range chain {
		switch p.Name {
		case "t-oauth-pool-connected":
			hasConnected = true
			if p.AuthType != AuthChatGPTOAuth {
				t.Fatalf("auth_type = %q, want %q", p.AuthType, AuthChatGPTOAuth)
			}
		case "t-oauth-pool-unconnected":
			t.Fatal("OAuth profile without credentials entered the failover chain")
		}
	}
	if !hasConnected {
		t.Fatal("keyless OAuth profile with credentials missing from the failover chain")
	}
}

func TestListProfilesShowsOAuthStatusWithoutTokens(t *testing.T) {
	d := openTestDB(t)
	c := testCipher(t)
	connected := oauthTestProfile(t, d, "t-oauth-list-connected")
	oauthTestProfile(t, d, "t-oauth-list-unconnected")
	expiresAt := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: connected, AccessToken: fakeAccessToken, RefreshToken: fakeRefreshToken, ExpiresAt: expiresAt, AccountID: "acct"}); err != nil {
		t.Fatal(err)
	}
	profiles, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]*OAuthStatus{}
	for _, p := range profiles {
		statuses[p.Name] = p.OAuth
	}
	if s := statuses["t-oauth-list-connected"]; s == nil || !s.Connected || !s.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("connected status = %+v", s)
	}
	if s := statuses["t-oauth-list-unconnected"]; s == nil || s.Connected {
		t.Fatalf("unconnected status = %+v", s)
	}
	out, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fakeAccessToken, fakeRefreshToken} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("token leaked into profile list JSON")
		}
	}
	if !strings.Contains(string(out), `"connected":true`) {
		t.Fatalf("profile list JSON lacks connected status: %s", out)
	}
}

func TestSaveProfileKeepsAuthTypeWhenEmptyOnUpdate(t *testing.T) {
	d := openTestDB(t)
	id := oauthTestProfile(t, d, "t-oauth-update")
	if _, err := d.SaveProfile(&LLMProfile{ID: id, Name: "t-oauth-update", Format: "openai-responses", Model: "m2"}); err != nil {
		t.Fatal(err)
	}
	p, err := d.ProfileByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthType != AuthChatGPTOAuth || p.Model != "m2" {
		t.Fatalf("after update: auth_type=%q model=%q", p.AuthType, p.Model)
	}

	keyed, err := d.SaveProfile(&LLMProfile{Name: "t-oauth-default-auth", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, keyed) })
	kp, err := d.ProfileByID(keyed)
	if err != nil {
		t.Fatal(err)
	}
	if kp.AuthType != AuthAPIKey {
		t.Fatalf("new profile auth_type = %q, want %q", kp.AuthType, AuthAPIKey)
	}
}

// TestOAuthCredentialsPlanType 은 plan_type 열이 저장·갱신·목록에 쓰이고, 열이 생기기 전 방식으로
// 넣은 행은 빈 플랜으로 읽히는지 본다.
func TestOAuthCredentialsPlanType(t *testing.T) {
	d := openTestDB(t)
	c := testCipher(t)
	var columnDefault string
	if err := d.QueryRow(`SELECT column_default FROM information_schema.columns
WHERE table_name='llm_oauth_credentials' AND column_name='plan_type'`).Scan(&columnDefault); err != nil {
		t.Fatalf("plan_type column: %v", err)
	}
	if !strings.HasPrefix(columnDefault, "''") {
		t.Fatalf("plan_type default = %q, want empty string", columnDefault)
	}
	id := oauthTestProfile(t, d, "t-oauth-plan")
	if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: id, AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now(), PlanType: "plus"}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.OAuthCredentials(t.Context(), c, id); err != nil || got.PlanType != "plus" {
		t.Fatalf("saved plan = %q, err %v; want plus", got.PlanType, err)
	}
	_, err := d.RefreshOAuthCredentials(t.Context(), c, id, "a", func(context.Context, OAuthCredentials) (OAuthCredentials, error) {
		return OAuthCredentials{AccessToken: "a2", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour), PlanType: "pro"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.OAuthCredentials(t.Context(), c, id); err != nil || got.PlanType != "pro" {
		t.Fatalf("refreshed plan = %q, err %v; want pro", got.PlanType, err)
	}

	// plan_type 열을 모르는 예전 코드처럼 넣은 행
	legacy := oauthTestProfile(t, d, "t-oauth-plan-legacy")
	access, err := c.seal(legacy, "access_token", "a")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := c.seal(legacy, "refresh_token", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO llm_oauth_credentials(profile_id,access_token,refresh_token,expires_at) VALUES ($1,$2,$3,now())`, legacy, access, refresh); err != nil {
		t.Fatal(err)
	}
	if got, err := d.OAuthCredentials(t.Context(), c, legacy); err != nil || got.PlanType != "" {
		t.Fatalf("legacy plan = %q, err %v; want empty", got.PlanType, err)
	}

	profiles, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	plans := map[string]string{}
	for _, p := range profiles {
		if p.OAuth != nil {
			plans[p.Name] = p.OAuth.PlanType
		}
	}
	if plans["t-oauth-plan"] != "pro" || plans["t-oauth-plan-legacy"] != "" {
		t.Fatalf("listed plans = %v", plans)
	}
}

func TestDeleteOAuthCredentials(t *testing.T) {
	d := openTestDB(t)
	c := testCipher(t)
	id := oauthTestProfile(t, d, "t-oauth-delete")
	kept := oauthTestProfile(t, d, "t-oauth-delete-kept")
	for _, pid := range []int64{id, kept} {
		if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: pid, AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.DeleteOAuthCredentials(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OAuthCredentials(t.Context(), c, id); !errors.Is(err, ErrOAuthCredentialsNotFound) {
		t.Fatalf("after delete: err = %v, want ErrOAuthCredentialsNotFound", err)
	}
	if err := d.DeleteOAuthCredentials(t.Context(), id); !errors.Is(err, ErrOAuthCredentialsNotFound) {
		t.Fatalf("second delete: err = %v, want ErrOAuthCredentialsNotFound", err)
	}
	if _, err := d.OAuthCredentials(t.Context(), c, kept); err != nil {
		t.Fatalf("other profile lost its credentials: %v", err)
	}
	if p, err := d.ProfileByID(id); err != nil || p == nil {
		t.Fatalf("profile row gone after credential delete: %v", err)
	}
}

// TestSaveProfileOAuthDropsAPIKey 는 저장 뒤 인증 방식이 chatgpt_oauth 면 어느 저장 경로든 API 키와
// 힌트가 남지 않고, API 키 프로필의 키는 그대로인지 본다.
func TestSaveProfileOAuthDropsAPIKey(t *testing.T) {
	d := openTestDB(t)
	const key = "sk-issue48-relay-key"
	newKeyed := func(t *testing.T, name string) int64 {
		t.Helper()
		id, err := d.SaveProfile(&LLMProfile{Name: name, Format: "openai", Model: "m", APIKey: key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
		return id
	}
	tests := []struct {
		name    string
		save    func(t *testing.T) int64
		wantKey bool
	}{
		{"새 OAuth 프로필에 키", func(t *testing.T) int64 {
			id, err := d.SaveProfile(&LLMProfile{Name: "t-oauth-key-insert", Format: "openai-responses", Model: "m", APIKey: key, AuthType: AuthChatGPTOAuth})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
			return id
		}, false},
		{"키 없이 OAuth 로 전환", func(t *testing.T) int64 {
			id := newKeyed(t, "t-oauth-key-switch")
			if _, err := d.SaveProfile(&LLMProfile{ID: id, Name: "t-oauth-key-switch", Format: "openai-responses", Model: "m", AuthType: AuthChatGPTOAuth}); err != nil {
				t.Fatal(err)
			}
			return id
		}, false},
		{"키를 주며 OAuth 로 전환", func(t *testing.T) int64 {
			id := newKeyed(t, "t-oauth-key-switch-with-key")
			if _, err := d.SaveProfile(&LLMProfile{ID: id, Name: "t-oauth-key-switch-with-key", Format: "openai-responses", Model: "m", APIKey: key, AuthType: AuthChatGPTOAuth}); err != nil {
				t.Fatal(err)
			}
			return id
		}, false},
		{"API 키 프로필 수정은 키 유지", func(t *testing.T) int64 {
			id := newKeyed(t, "t-oauth-key-apikey")
			if _, err := d.SaveProfile(&LLMProfile{ID: id, Name: "t-oauth-key-apikey", Format: "openai", Model: "m2"}); err != nil {
				t.Fatal(err)
			}
			return id
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.save(t)
			var apiKey, hint string
			if err := d.QueryRow(`SELECT COALESCE(api_key,''),COALESCE(api_key_hint,'') FROM llm_profiles WHERE id=$1`, id).Scan(&apiKey, &hint); err != nil {
				t.Fatal(err)
			}
			isKept := apiKey == key && hint != ""
			isCleared := apiKey == "" && hint == ""
			if (tc.wantKey && !isKept) || (!tc.wantKey && !isCleared) {
				t.Fatalf("key left=%v hint=%q, want key %v", apiKey != "", hint, tc.wantKey)
			}
		})
	}
}
