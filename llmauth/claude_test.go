package llmauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type claudeRoundTrip func(*http.Request) (*http.Response, error)

func (f claudeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 잘못된 PKCE·form 전송·토큰 응답 수용·비밀값 출력은 이 테스트가 실패해야 한다.
func TestClaudeOAuthExchangeAndRefresh(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	var bodies []map[string]any
	client := &ClaudeClient{Now: func() time.Time { return now }, HTTPClient: &http.Client{Transport: claudeRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://platform.claude.com/v1/oauth/token" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("잘못된 토큰 요청 주소·형식")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fake-claude-access","refresh_token":"fake-claude-refresh","expires_in":3600,"account":{"uuid":"account-1"}}`))}, nil
	})}}
	u, err := url.Parse(client.AuthorizeURL("challenge-1", "state-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "claude.ai" || u.Path != "/oauth/authorize" || q.Get("code") != "true" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "http://localhost:53692/callback" || q.Get("state") != "state-1" || q.Get("code_challenge") != "challenge-1" {
		t.Fatalf("잘못된 인가 주소: %v", u)
	}
	tokens, err := client.ExchangeCode(context.Background(), "code-1", "state-1", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "fake-claude-access" || tokens.RefreshToken != "fake-claude-refresh" || tokens.AccountID != "account-1" || !tokens.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("교환 결과가 일치하지 않는다")
	}
	_, err = client.Refresh(context.Background(), tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0]["grant_type"] != "authorization_code" || bodies[0]["client_id"] != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" || bodies[0]["code_verifier"] != "verifier-1" || bodies[0]["state"] != "state-1" || bodies[0]["redirect_uri"] != "http://localhost:53692/callback" || bodies[1]["grant_type"] != "refresh_token" || bodies[1]["refresh_token"] != "fake-claude-refresh" {
		t.Fatal("JSON 교환·갱신 필드가 일치하지 않는다")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", tokens, tokens), "fake-claude-") {
		t.Fatal("토큰이 출력된다")
	}
}

func TestClaudeOAuthRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{`{"access_token":"secret","refresh_token":"secret","expires_in":0}`, `{"access_token":"secret","refresh_token":"secret","expires_in":-1}`, `{"access_token":"secret","refresh_token":"secret","expires_in":true}`, `{"access_token":"secret","refresh_token":"secret","expires_in":1.5}`, `{"access_token":"","refresh_token":"secret","expires_in":3600}`, `{"access_token":"secret","expires_in":3600}`, `secret-not-json`} {
		t.Run(body, func(t *testing.T) {
			c := &ClaudeClient{HTTPClient: &http.Client{Transport: claudeRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			_, err := c.ExchangeCode(context.Background(), "code", "state", "verifier")
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("형식 검증·비밀값 보호 실패: %v", err)
			}
		})
	}
}

func TestClaudeOAuthRefreshErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		login  bool
	}{{400, true}, {401, true}, {403, true}, {429, false}, {500, false}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := 0
			c := &ClaudeClient{HTTPClient: &http.Client{Transport: claudeRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(`{"error":"fake-secret-token"}`))}, nil
			})}}
			_, err := c.Refresh(context.Background(), "fake-secret-refresh")
			if err == nil || calls != 1 || strings.Contains(err.Error(), "fake-secret") {
				t.Fatalf("오류·재시도·비밀값 보호 실패: %v", err)
			}
			e, ok := err.(*TokenError)
			if !ok || e.NeedsLogin() != tc.login {
				t.Fatalf("재로그인 분류: %v", err)
			}
		})
	}
}
