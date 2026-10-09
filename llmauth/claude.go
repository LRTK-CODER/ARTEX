package llmauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ClaudeClientID 는 Claude Code 구독 로그인의 공개 클라이언트 식별자다.
	ClaudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// ClaudeRedirectURI 는 브라우저가 로그인 뒤 이동하는 루프백 주소다.
	ClaudeRedirectURI  = "http://localhost:53692/callback"
	claudeAuthorizeURL = "https://claude.ai/oauth/authorize"
	claudeTokenURL     = "https://platform.claude.com/v1/oauth/token"
	claudeScope        = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// ClaudeClient 는 Claude 구독의 JSON 토큰 교환·갱신을 한다. 제로값은 고정된 제공자 주소를 쓴다.
// OML claude_oauth.py의 요청 계약을 따르며 자격 증명은 기존 Store에 맡긴다.
type ClaudeClient struct {
	HTTPClient *http.Client
	// Now 는 expires_in을 절대 시각으로 바꿀 때 쓰며 nil이면 time.Now다.
	Now func() time.Time
}

// AuthorizeURL 은 PKCE S256·state를 포함한 로그인 주소를 만든다.
func (c *ClaudeClient) AuthorizeURL(challenge, state string) string {
	q := url.Values{"code": {"true"}, "client_id": {ClaudeClientID}, "response_type": {"code"},
		"redirect_uri": {ClaudeRedirectURI}, "scope": {claudeScope}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "state": {state}}
	return claudeAuthorizeURL + "?" + q.Encode()
}

// ExchangeCode 는 일회용 코드를 토큰으로 바꾼다. state·redirect_uri도 인가 때와 같아야 한다.
func (c *ClaudeClient) ExchangeCode(ctx context.Context, code, state, verifier string) (Tokens, error) {
	return c.post(ctx, "exchange Claude code", map[string]string{"grant_type": "authorization_code",
		"client_id": ClaudeClientID, "code": code, "state": state,
		"redirect_uri": ClaudeRedirectURI, "code_verifier": verifier})
}

// Refresh 는 회전된 access·refresh 토큰을 돌려준다. 요청을 자동 재시도하지 않는다.
func (c *ClaudeClient) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	return c.post(ctx, "refresh Claude token", map[string]string{"grant_type": "refresh_token",
		"client_id": ClaudeClientID, "refresh_token": refreshToken})
}

func (c *ClaudeClient) post(ctx context.Context, op string, fields map[string]string) (Tokens, error) {
	body, err := json.Marshal(fields)
	if err != nil {
		return Tokens{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL, bytes.NewReader(body))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: defaultHTTPTimeout}
	if c.HTTPClient != nil {
		*client = *c.HTTPClient
	}
	// 307·308로 토큰·verifier 본문이 다른 호스트에 재전송되지 않게 한다.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return Tokens{}, &claudeNetworkError{cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := &TokenError{Op: op, StatusCode: resp.StatusCode}
		if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
			e.Code = ErrorCodeInvalidGrant
		}
		return Tokens{}, e
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Tokens{}, &claudeNetworkError{cause: err}
	}
	if len(raw) > maxResponseBytes {
		return Tokens{}, ErrResponseTooLarge
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Account      struct {
			ID string `json:"uuid"`
		} `json:"account"`
	}
	// JSON 파싱 오류에 응답 원문이 섞일 수 있으므로 고정 문구로 거절한다.
	if json.Unmarshal(raw, &data) != nil || strings.TrimSpace(data.AccessToken) == "" || strings.TrimSpace(data.RefreshToken) == "" || data.ExpiresIn <= 0 || data.ExpiresIn > int64((1<<63-1)/time.Second) {
		return Tokens{}, errors.New("llmauth: invalid Claude token response")
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	return Tokens{AccessToken: data.AccessToken, RefreshToken: data.RefreshToken,
		ExpiresAt: now.Add(time.Duration(data.ExpiresIn) * time.Second), AccountID: data.Account.ID}, nil
}

// 네트워크 원인은 errors.Is/As로 보존하되 오류 문자열에 URL·비밀값을 내보내지 않는다.
type claudeNetworkError struct{ cause error }

func (e *claudeNetworkError) Error() string { return "llmauth: Claude token endpoint unavailable" }
func (e *claudeNetworkError) Unwrap() error { return e.cause }
