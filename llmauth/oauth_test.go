package llmauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPKCE(t *testing.T) {
	// RFC 7636 부록 B 의 예시 난수와 결과.
	octets := []byte{116, 24, 223, 180, 151, 153, 224, 37, 79, 250, 96, 125, 216, 173, 187, 186,
		22, 212, 37, 77, 105, 214, 191, 240, 91, 88, 5, 88, 83, 132, 141, 121}
	tests := []struct {
		name          string
		random        []byte
		wantVerifier  string
		wantChallenge string
		wantErr       bool
	}{
		{
			name:          "RFC 7636 부록 B",
			random:        octets,
			wantVerifier:  "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			wantChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
		},
		{name: "난수가 모자라면 오류", random: octets[:10], wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewPKCE(bytes.NewReader(tc.random))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NewPKCE() err = nil, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPKCE() err = %v", err)
			}
			if got.Verifier != tc.wantVerifier || got.Challenge != tc.wantChallenge {
				t.Fatalf("NewPKCE() = %+v, want verifier %q challenge %q", got, tc.wantVerifier, tc.wantChallenge)
			}
		})
	}
}

func TestPKCEDefaultRandomIsUnique(t *testing.T) {
	a, err := NewPKCE(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPKCE(nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verifier == b.Verifier || len(a.Verifier) != 43 {
		t.Fatalf("verifiers %q %q: want distinct 43-char values", a.Verifier, b.Verifier)
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := &Client{}
	raw := c.AuthorizeURL(RedirectURI, "challenge-x", "state-y")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://auth.openai.com/oauth/authorize" {
		t.Fatalf("endpoint = %q", got)
	}
	want := map[string]string{
		"response_type":              "code",
		"client_id":                  "app_EMoamEEZ73f0CkXaXp7hrann",
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"scope":                      "openid profile email offline_access",
		"code_challenge":             "challenge-x",
		"code_challenge_method":      "S256",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"state":                      "state-y",
		"originator":                 "codex_cli_rs",
	}
	q := u.Query()
	for key, value := range want {
		if got := q.Get(key); got != value {
			t.Errorf("query %s = %q, want %q", key, got, value)
		}
	}
	if len(q) != len(want) {
		t.Errorf("query has %d params, want %d: %v", len(q), len(want), q)
	}
}

func TestExchangeCode(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	got, err := f.client().ExchangeCode(context.Background(), "auth-code", "verifier-1", RedirectURI)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if got.AccountID != "acct-123" || got.RefreshToken != "refresh-1" || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("ExchangeCode = %+v / refresh %q", got, got.RefreshToken)
	}
	if got.AccessToken == "" || got.IDToken == "" {
		t.Fatal("ExchangeCode returned empty access or id token")
	}
	form := f.exchangeForms[0]
	if form.Get("code") != "auth-code" || form.Get("code_verifier") != "verifier-1" || form.Get("redirect_uri") != RedirectURI {
		t.Fatalf("exchange form = %v", form)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	f := newFakeAuthServer(t, time.Unix(1_800_000_000, 0))
	c := f.client()
	first, err := c.Refresh(context.Background(), "refresh-0")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if first.RefreshToken != "refresh-1" || first.AccountID != "acct-123" {
		t.Fatalf("Refresh = %+v / refresh %q, want rotated refresh-1", first, first.RefreshToken)
	}
	// 회전된 뒤 옛 refresh 토큰을 다시 쓰면 서버가 거부한다.
	_, err = c.Refresh(context.Background(), "refresh-0")
	var tokenErr *TokenError
	if !errors.As(err, &tokenErr) || tokenErr.Code != ErrorCodeRefreshTokenReused || !tokenErr.NeedsLogin() {
		t.Fatalf("reused refresh err = %v, want refresh_token_reused", err)
	}
}

func TestRefreshKeepsRefreshTokenWhenOmitted(t *testing.T) {
	access := accessJWT(t, "acct-9", time.Unix(1_800_003_600, 0))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"access_token": access})
	}))
	t.Cleanup(srv.Close)
	c := &Client{HTTPClient: srv.Client(), Issuer: srv.URL}
	got, err := c.Refresh(context.Background(), "keep-me")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.RefreshToken != "keep-me" || got.AccountID != "acct-9" {
		t.Fatalf("Refresh = %+v / refresh %q", got, got.RefreshToken)
	}
}

func TestTokenErrorHidesSecrets(t *testing.T) {
	const secret = "sk-SECRET-token-value-123"
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode ErrorCode
	}{
		{"error 문자열", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"token ` + secret + ` bad","refresh_token":"` + secret + `"}`, ErrorCodeInvalidGrant},
		{"error 객체", http.StatusUnauthorized, `{"error":{"code":"refresh_token_reused","message":"` + secret + `"}}`, ErrorCodeRefreshTokenReused},
		{"error 가 null 이고 최상위 code", http.StatusUnauthorized, `{"error":null,"code":"refresh_token_reused","detail":"` + secret + `"}`, ErrorCodeRefreshTokenReused},
		{"최상위 code", http.StatusUnauthorized, `{"code":"refresh_token_expired","detail":"` + secret + `"}`, ErrorCodeRefreshTokenExpired},
		{"코드 자리에 비밀값", http.StatusBadRequest, `{"error":"` + secret + ` leaked here"}`, ""},
		{"JSON 이 아닌 본문", http.StatusBadGateway, `<html>` + secret + `</html>`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			c := &Client{HTTPClient: srv.Client(), Issuer: srv.URL}
			calls := map[string]func() error{
				"exchange": func() error { _, err := c.ExchangeCode(context.Background(), secret, secret, RedirectURI); return err },
				"refresh":  func() error { _, err := c.Refresh(context.Background(), secret); return err },
			}
			for op, call := range calls {
				err := call()
				var tokenErr *TokenError
				if !errors.As(err, &tokenErr) {
					t.Fatalf("%s err = %v, want *TokenError", op, err)
				}
				if tokenErr.StatusCode != tc.status || tokenErr.Code != tc.wantCode {
					t.Fatalf("%s TokenError = %+v, want status %d code %q", op, tokenErr, tc.status, tc.wantCode)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("%s err.Error() = %q contains secret", op, err.Error())
				}
				if !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
					t.Fatalf("%s err.Error() = %q lacks status code", op, err.Error())
				}
			}
		})
	}
}

func TestTokenResponseErrorsHideSecrets(t *testing.T) {
	const secret = "sk-SECRET-token-value-123"
	tests := []struct {
		name string
		body string
	}{
		{"깨진 JSON", `{"access_token":"` + secret},
		{"JWT 가 아닌 access token", `{"access_token":"` + secret + `","refresh_token":"r"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			c := &Client{HTTPClient: srv.Client(), Issuer: srv.URL}
			_, err := c.Refresh(context.Background(), "r")
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("Refresh err = %v, want error without secret", err)
			}
		})
	}
}

func TestStringHidesSecrets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tokens := Tokens{IDToken: "id-SECRET", AccessToken: "access-SECRET", RefreshToken: "refresh-SECRET", AccountID: "acct-1", ExpiresAt: now.Add(time.Hour)}
	store := &memoryStore{tokens: tokens, hasTokens: true}
	// 만료 전 토큰이라 갱신하지 않는다. Issuer 는 닿지 않는 주소로 두어 실제 서버를 부를 일이 없게 한다.
	src := NewTokenSource(&Client{Issuer: "http://127.0.0.1:1"}, store, func() time.Time { return now })
	if _, _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	values := map[string]any{
		"Tokens":      tokens,
		"TokenSource": src,
		"PKCE":        PKCE{Verifier: "verifier-SECRET", Challenge: "challenge"},
	}
	for name, value := range values {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			if got := fmt.Sprintf(format, value); strings.Contains(got, "SECRET") {
				t.Errorf("%s Sprintf(%q) = %q contains secret", name, format, got)
			}
		}
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var otherHostGotBody bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHostGotBody = true
	}))
	t.Cleanup(other.Close)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(issuer.Close)

	// 기본 HTTP 클라이언트를 쓰는 경로다(HTTPClient 를 넘기지 않는다).
	c := &Client{Issuer: issuer.URL}
	_, err := c.Refresh(context.Background(), "refresh-SECRET")
	var tokenErr *TokenError
	if !errors.As(err, &tokenErr) || tokenErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want TokenError status 307", err)
	}
	if otherHostGotBody {
		t.Fatal("redirect target received the refresh request")
	}
}

func TestResponseSizeLimit(t *testing.T) {
	access := accessJWT(t, "acct-1", time.Unix(1_800_003_600, 0))
	tests := []struct {
		name    string
		padding int
		wantErr bool
	}{
		{"상한 안", 1000, false},
		{"상한 넘음", maxResponseBytes, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 공백은 JSON 으로 유효해서, 상한이 없으면 큰 응답도 정상으로 읽힌다.
				fmt.Fprintf(w, `{"access_token":%q}%s`, access, strings.Repeat(" ", tc.padding))
			}))
			t.Cleanup(srv.Close)
			c := &Client{HTTPClient: srv.Client(), Issuer: srv.URL}
			_, err := c.Refresh(context.Background(), "r")
			if tc.wantErr != errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("err = %v, want too-large error %v", err, tc.wantErr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
