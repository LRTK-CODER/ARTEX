// Package llmauth 는 ChatGPT 구독 계정의 OAuth 토큰을 받고 갱신한다.
// Codex CLI 와 같은 공개 client_id 로 브라우저 로그인(PKCE)과 디바이스 코드 로그인을 하고,
// 만료가 다가온 토큰을 갱신한다. 저장은 Store 인터페이스로만 연결한다.
package llmauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Codex 로그인 흐름의 고정값. 서버가 client_id 마다 redirect 허용 목록을 고정하므로 바꿀 수 없다.
const (
	// DefaultIssuer 는 OpenAI 인증 서버 주소다.
	DefaultIssuer = "https://auth.openai.com"
	// ClientID 는 Codex CLI 의 공개 OAuth client_id 다.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// RedirectURI 는 브라우저 로그인 콜백 주소다. 서버 허용 목록에 고정돼 있다.
	RedirectURI = "http://localhost:1455/auth/callback"
	// Originator 는 authorize 요청과 Codex 백엔드 호출에 싣는 호출자 식별값이다.
	// Codex CLI 의 기본값과 같게 둔다.
	Originator = "codex_cli_rs"

	authorizeScope = "openid profile email offline_access"
	// refreshScope 는 Codex CLI 가 refresh 요청에 싣는 scope 다.
	refreshScope = "openid profile email"

	defaultHTTPTimeout = 30 * time.Second
	// maxResponseBytes 는 인증 서버 응답을 읽을 상한이다. 토큰 응답은 수 KB 다.
	maxResponseBytes = 1 << 20
)

// Client 는 OpenAI 인증 서버와 이야기한다. 제로값은 기본 issuer 와 30초 타임아웃 HTTP 클라이언트를 쓴다.
type Client struct {
	// HTTPClient 가 nil 이면 30초 타임아웃 클라이언트를 쓴다.
	HTTPClient *http.Client
	// Issuer 가 비면 DefaultIssuer 를 쓴다. 끝의 "/"는 붙이지 않는다.
	Issuer string

	// now 와 sleep 은 디바이스 코드 폴링의 시계다. nil 이면 실제 시계를 쓴다.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// Tokens 는 인증 서버가 준 토큰과 거기서 꺼낸 값이다.
type Tokens struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	// AccountID 는 JWT 의 chatgpt_account_id 클레임이다. Codex 백엔드 호출 헤더에 쓴다.
	AccountID string
	// ExpiresAt 은 access token 의 exp 클레임이다.
	ExpiresAt time.Time
}

// String 은 토큰 원문 없이 계정과 만료 시각만 보여 준다. 로그·오류에 토큰이 새지 않게 한다.
func (t Tokens) String() string {
	return fmt.Sprintf("llmauth.Tokens{AccountID: %q, ExpiresAt: %s}", t.AccountID, t.ExpiresAt.Format(time.RFC3339))
}

// GoString 은 %#v 에서도 토큰 원문을 숨긴다.
func (t Tokens) GoString() string { return t.String() }

// ErrorCode 는 인증 서버 오류 응답의 error 코드다.
type ErrorCode string

// 분기에 쓰는 인증 서버 오류 코드.
const (
	ErrorCodeInvalidGrant            ErrorCode = "invalid_grant"
	ErrorCodeRefreshTokenReused      ErrorCode = "refresh_token_reused"
	ErrorCodeRefreshTokenExpired     ErrorCode = "refresh_token_expired"
	ErrorCodeRefreshTokenInvalidated ErrorCode = "refresh_token_invalidated"
)

// TokenError 는 인증 서버가 성공이 아닌 상태를 돌려준 경우다.
// 응답 본문 원문은 토큰이나 개인 정보를 담을 수 있어 싣지 않는다. 상태 코드와 error 코드만 담는다.
type TokenError struct {
	// Op 는 실패한 단계다(예: "exchange code", "refresh").
	Op         string
	StatusCode int
	// Code 는 응답의 error 코드다. 없거나 코드 모양이 아니면 비어 있다.
	Code ErrorCode
}

func (e *TokenError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("llmauth: %s: status %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("llmauth: %s: status %d: %s", e.Op, e.StatusCode, e.Code)
}

// NeedsLogin 은 refresh 토큰을 더 쓸 수 없어 다시 로그인해야 하는 오류인지 알려 준다.
func (e *TokenError) NeedsLogin() bool {
	switch e.Code {
	case ErrorCodeInvalidGrant, ErrorCodeRefreshTokenReused, ErrorCodeRefreshTokenExpired, ErrorCodeRefreshTokenInvalidated:
		return true
	}
	return false
}

// AuthorizeURL 은 브라우저로 열 authorize 주소를 만든다.
// challenge 는 PKCE challenge, state 는 호출자가 콜백에서 대조할 무작위 값이다.
func (c *Client) AuthorizeURL(redirectURI, challenge, state string) string {
	query := url.Values{
		"response_type":              {"code"},
		"client_id":                  {ClientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {authorizeScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {Originator},
	}
	return c.issuer() + "/oauth/authorize?" + query.Encode()
}

// ExchangeCode 는 authorization code 를 토큰으로 바꾼다. 요청은 form 형식이다.
// redirectURI 는 authorize 때와 같아야 한다.
func (c *Client) ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {ClientID},
		"code_verifier": {verifier},
	}
	body := strings.NewReader(form.Encode())
	raw, err := c.post(ctx, "exchange code", "/oauth/token", "application/x-www-form-urlencoded", body)
	if err != nil {
		return Tokens{}, err
	}
	resp, err := decodeTokenResponse(raw)
	if err != nil {
		return Tokens{}, fmt.Errorf("llmauth: exchange code: %w", err)
	}
	if resp.RefreshToken == "" {
		return Tokens{}, errors.New("llmauth: exchange code: response has no refresh_token")
	}
	return resp.tokens()
}

// Refresh 는 refresh 토큰으로 새 토큰을 받는다. refresh 토큰은 갱신마다 회전하므로
// 돌려받은 Tokens 를 곧바로 저장해야 한다. 응답에 새 refresh 토큰이나 id token 이 없으면
// 각각 넘긴 refresh 토큰을 유지하고 access token 에서 계정을 꺼낸다.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	payload, err := json.Marshal(map[string]string{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"scope":         refreshScope,
	})
	if err != nil {
		return Tokens{}, fmt.Errorf("llmauth: refresh: encode request: %w", err)
	}
	// Codex CLI 와 같이 refresh 는 JSON 으로 보낸다.
	raw, err := c.post(ctx, "refresh", "/oauth/token", "application/json", bytes.NewReader(payload))
	if err != nil {
		return Tokens{}, err
	}
	resp, err := decodeTokenResponse(raw)
	if err != nil {
		return Tokens{}, fmt.Errorf("llmauth: refresh: %w", err)
	}
	if resp.RefreshToken == "" {
		resp.RefreshToken = refreshToken
	}
	return resp.tokens()
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func decodeTokenResponse(raw []byte) (tokenResponse, error) {
	var resp tokenResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		// 본문에 토큰이 있을 수 있어 json 오류 문구(본문 일부를 인용할 수 있다)를 잇지 않는다.
		return tokenResponse{}, errors.New("decode token response: invalid json")
	}
	if resp.AccessToken == "" {
		return tokenResponse{}, errors.New("decode token response: no access_token")
	}
	return resp, nil
}

func (r tokenResponse) tokens() (Tokens, error) {
	access, err := ParseClaims(r.AccessToken)
	if err != nil {
		return Tokens{}, fmt.Errorf("llmauth: access token: %w", err)
	}
	if access.ExpiresAt.IsZero() {
		return Tokens{}, errors.New("llmauth: access token: no exp claim")
	}
	accountID := access.AccountID
	if r.IDToken != "" {
		id, err := ParseClaims(r.IDToken)
		if err != nil {
			return Tokens{}, fmt.Errorf("llmauth: id token: %w", err)
		}
		// Codex CLI 는 id token 의 계정을 쓴다. access token 에 같은 클레임이 있어도 id token 을 앞세운다.
		if id.AccountID != "" {
			accountID = id.AccountID
		}
	}
	if accountID == "" {
		return Tokens{}, errors.New("llmauth: token has no chatgpt_account_id claim")
	}
	return Tokens{
		IDToken:      r.IDToken,
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		AccountID:    accountID,
		ExpiresAt:    access.ExpiresAt,
	}, nil
}

// post 는 issuer 의 path 로 body 를 보내고 2xx 응답 본문을 돌려준다.
// 2xx 가 아니면 *TokenError 를 돌려준다.
func (c *Client) post(ctx context.Context, op, path, contentType string, body io.Reader) ([]byte, error) {
	status, raw, err := c.do(ctx, op, path, contentType, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, &TokenError{Op: op, StatusCode: status, Code: errorCode(raw)}
	}
	return raw, nil
}

func (c *Client) do(ctx context.Context, op, path, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer()+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("llmauth: %s: build request: %w", op, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("llmauth: %s: %w", op, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("llmauth: %s: read response: %w", op, err)
	}
	return res.StatusCode, raw, nil
}

// errorCodePattern 은 OAuth error 코드 모양이다. 이 모양이 아닌 값은 서버가 넣은 임의 문장이거나
// 비밀값일 수 있어 오류 메시지에 싣지 않는다.
var errorCodePattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

// errorCode 는 오류 응답 본문에서 error 코드만 꺼낸다.
// 인증 서버는 {"error":"code"}, {"error":{"code":"code"}}, {"code":"code"} 를 모두 쓴다.
func errorCode(raw []byte) ErrorCode {
	var body struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	code := body.Code
	var text string
	var nested struct {
		Code string `json:"code"`
	}
	switch {
	case json.Unmarshal(body.Error, &text) == nil:
		code = text
	case json.Unmarshal(body.Error, &nested) == nil && nested.Code != "":
		code = nested.Code
	}
	if !errorCodePattern.MatchString(code) {
		return ""
	}
	return ErrorCode(code)
}

func (c *Client) issuer() string {
	if c.Issuer == "" {
		return DefaultIssuer
	}
	return c.Issuer
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient == nil {
		return &http.Client{Timeout: defaultHTTPTimeout}
	}
	return c.HTTPClient
}
