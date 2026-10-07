package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/llmauth"
)

// OAuthTokenSource 는 Codex 백엔드 호출에 쓸 토큰을 건넨다. *llmauth.TokenSource 가 만족한다.
// 쓰는 쪽(agent)에 좁게 두어, 테스트에서 손으로 만든 가짜로 바꾸고 토큰 공급자가 db 에 기대지 않게 한다.
type OAuthTokenSource interface {
	// Token 은 유효한 access token 과 ChatGPT 계정 ID 를 돌려준다.
	Token(ctx context.Context) (accessToken, accountID string, err error)
	// Invalidate 는 staleAccessToken 이 지금 토큰이면 다음 Token 이 갱신하게 한다.
	// 이미 갱신돼 지난 토큰이면 아무것도 하지 않아, 동시에 401 을 받은 요청들이 갱신을 거듭하지 않는다.
	Invalidate(staleAccessToken string)
}

// CodexBaseURL 은 BaseURL 이 빈 OAuth 프로필의 기본 주소다. 비워 두면 Norma 가 OPENAI_BASE_URL
// 환경 변수(API 키 프로필용 중계일 수 있다)로 채워 구독 토큰이 그쪽으로 나간다.
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// oauthAPIKeyPlaceholder 는 OAuth 프로필의 llm.Config.APIKey 자리에 넣는 값이다.
// Norma 는 APIKey 가 비면 OPENAI_API_KEY 환경 변수로 채우므로 비워 둘 수 없다.
// oauthTransport 가 Authorization 헤더를 통째로 바꾸므로 이 값은 밖으로 나가지 않는다.
const oauthAPIKeyPlaceholder = "chatgpt-oauth"

// loginRequiredBody 는 다시 로그인해야 할 때 돌려주는 합성 401 응답 본문이다.
// 오류 원문을 싣지 않는 고정 문자열이라 토큰이 섞일 수 없다.
const loginRequiredBody = `{"error":{"code":"chatgpt_login_required","message":"ChatGPT 구독 로그인이 필요하다"}}`

// codexRejectedFields 는 Codex 백엔드가 400 으로 거부하는 Responses 본문 필드다(#33 조사).
var codexRejectedFields = []string{"max_output_tokens", "max_tokens", "metadata"}

// oauthTransport 는 Norma 가 만든 Responses 요청을 Codex 백엔드가 받는 모양으로 바꾸고
// OAuth 토큰을 싣는다. 401 이면 토큰을 버리고 한 번만 갱신해 다시 보낸다.
// Norma 의 재시도(408·429·5xx·네트워크 오류)는 이 transport 바깥에서 돌므로 401 재시도와 겹치지 않는다.
type oauthTransport struct {
	base   http.RoundTripper
	tokens OAuthTokenSource
}

func (t oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := codexRequestBody(req)
	if err != nil {
		return nil, err
	}
	resp, usedToken, err := t.send(req, body)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// 401 본문으로는 만료와 형식 오류를 가를 수 없다. 한 번만 갱신해 보고, 다시 401 이면 그대로 돌려준다.
		// 버리는 응답은 본문을 읽지 않고 닫는다. 연결 하나를 재사용하지 못할 뿐이다.
		_ = resp.Body.Close()
		t.tokens.Invalidate(usedToken)
		resp, _, err = t.send(req, body)
	}
	if errors.Is(err, errLoginRequired) {
		// 오류로 올리면 Norma 는 네트워크 오류로 보고 재시도하고, llmpool 은 TokenError 문자열의
		// "status 400" 을 읽어 장애 전환을 하지 않는다. 401 응답이면 Norma 는 재시도 없이
		// "status 401" 오류로 올리고 llmpool 은 hard failure 로 본다.
		return loginRequiredResponse(req), nil
	}
	return resp, err
}

// errLoginRequired 는 갱신으로는 풀리지 않아 다시 로그인해야 한다는 transport 안의 표시다.
var errLoginRequired = errors.New("chatgpt oauth: login required")

// send 는 토큰을 받아 req 의 사본에 싣고 body 를 본문으로 보낸다. 원본 req 는 바꾸지 않는다.
// 401 이면 그 토큰을 Invalidate 하도록 실은 access token 도 돌려준다.
func (t oauthTransport) send(req *http.Request, body []byte) (*http.Response, string, error) {
	accessToken, accountID, err := t.tokens.Token(req.Context())
	if err != nil {
		if needsLogin(err) {
			return nil, "", errLoginRequired
		}
		return nil, "", fmt.Errorf("chatgpt oauth: %w", err)
	}
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	out.ContentLength = int64(len(body))
	out.Header.Set("Authorization", "Bearer "+accessToken)
	out.Header.Set("ChatGPT-Account-ID", accountID)
	out.Header.Set("originator", llmauth.Originator)
	resp, err := t.base.RoundTrip(out)
	return resp, accessToken, err
}

// needsLogin 은 갱신으로는 풀리지 않아 사람이 다시 로그인해야 하는 토큰 오류인지 알려 준다.
func needsLogin(err error) bool {
	if errors.Is(err, llmauth.ErrNoTokens) {
		return true
	}
	var tokenErr *llmauth.TokenError
	return errors.As(err, &tokenErr) && tokenErr.NeedsLogin()
}

func loginRequiredResponse(req *http.Request) *http.Response {
	return &http.Response{
		Status:        "401 Unauthorized",
		StatusCode:    http.StatusUnauthorized,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(loginRequiredBody)),
		ContentLength: int64(len(loginRequiredBody)),
		Request:       req,
	}
}

// codexRequestBody 는 req 본문을 읽어 Codex 백엔드가 받는 Responses 본문으로 바꾼다.
// 거부되는 필드를 빼고 stream:true, store:false 를 넣는다.
func codexRequestBody(req *http.Request) ([]byte, error) {
	raw, err := readRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("chatgpt oauth: read request body: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("chatgpt oauth: request body is not a JSON object: %w", err)
	}
	for _, name := range codexRejectedFields {
		delete(fields, name)
	}
	fields["stream"] = json.RawMessage("true")
	fields["store"] = json.RawMessage("false")
	return json.Marshal(fields)
}

// readRequestBody 는 req 본문을 읽고 닫는다. RoundTripper 는 원본 본문을 늘 닫아야 한다.
func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, errors.New("empty body")
	}
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}
