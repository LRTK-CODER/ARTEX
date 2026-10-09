package db

import (
	"errors"
	"testing"
	"time"
)

// Claude 구독이 API 키 없이 목록·실패 전환에 들어가고, 방식 변경 때 키를 버리는지 본다.
func TestClaudeProfileCredentials(t *testing.T) {
	d := openTestDB(t)
	p := &LLMProfile{Name: "t-claude-credentials", Format: "anthropic", Model: "claude-sonnet-test", AuthType: AuthClaudeOAuth, APIKey: "fake-key-drop"}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	p.ID = id
	stored, err := d.ProfileByID(id)
	if err != nil || stored.APIKey != "" || stored.APIKeyHint != "" {
		t.Fatal("구독 프로필에 API 키가 남았다")
	}
	if err := d.SaveOAuthCredentials(t.Context(), testCipher(t), OAuthCredentials{ProfileID: id, AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	profiles, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	connected := false
	for _, p := range profiles {
		if p.ID == id {
			connected = p.OAuth != nil && p.OAuth.Connected
		}
	}
	if !connected {
		t.Fatal("목록에 연결 상태가 없다")
	}
	chain, err := d.PoolProfiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range chain {
		if p.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("연결된 Claude 프로필이 실패 전환 체인에 없다")
	}
}

func TestClaudeAuthSwitchDropsCredentials(t *testing.T) {
	d := openTestDB(t)
	p := &LLMProfile{Name: "t-claude-auth-switch", Format: "anthropic", Model: "m", AuthType: AuthClaudeOAuth}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	p.ID = id
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	c := testCipher(t)
	if err := d.SaveOAuthCredentials(t.Context(), c, OAuthCredentials{ProfileID: id, AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	p.AuthType = AuthChatGPTOAuth
	p.Format = "openai-responses"
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OAuthCredentials(t.Context(), c, id); !errors.Is(err, ErrOAuthCredentialsNotFound) {
		t.Fatalf("다른 제공자 토큰이 남았다: %v", err)
	}
}
