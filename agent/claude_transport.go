package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// ClaudeOAuthHTTPClient 는 모델 목록도 모델 호출과 같은 인증·401 재시도 정책을 쓰게 한다.
func ClaudeOAuthHTTPClient(proxy string, tokens OAuthTokenSource) (*http.Client, error) {
	c, err := oauthHTTPClient(proxy, "", tokens, db.AuthClaudeOAuth)
	if err != nil {
		return nil, err
	}
	c.Timeout = 30 * time.Second
	return c, nil
}

const (
	// ClaudeBaseURL 은 구독 토큰을 보낼 고정 Anthropic API 주소다.
	ClaudeBaseURL = "https://api.anthropic.com"
	// ClaudeCodeIdentity 는 OML에서 확인한 구독 요청의 시스템 첫 줄이다.
	ClaudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeOAuthBeta    = "oauth-2025-04-20"
)

// ClaudeOAuthHeaders 는 SDK의 OAuth 인증과 같은 헤더를 넣으며 API 키를 제거한다.
func ClaudeOAuthHeaders(h http.Header, token string) {
	h.Del("x-api-key")
	h.Set("Authorization", "Bearer "+token)
	h.Set("anthropic-version", "2023-06-01")
	for _, beta := range strings.Split(h.Get("anthropic-beta"), ",") {
		if strings.TrimSpace(beta) == claudeOAuthBeta {
			return
		}
	}
	if h.Get("anthropic-beta") == "" {
		h.Set("anthropic-beta", claudeOAuthBeta)
	} else {
		h.Set("anthropic-beta", h.Get("anthropic-beta")+","+claudeOAuthBeta)
	}
}

type claudeTransport struct {
	base   http.RoundTripper
	tokens OAuthTokenSource
}

func (t claudeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := claudeRequestBody(req)
	if err != nil {
		return nil, err
	}
	resp, used, err := t.send(req, body)
	if err == nil && used != "" && resp.StatusCode == http.StatusUnauthorized {
		// 첫 401만 갱신한다. 응답 본문은 버리고 닫아 비밀값이 오류·로그로 퍼지지 않게 한다.
		_ = resp.Body.Close()
		t.tokens.Invalidate(used)
		resp, used, err = t.send(req, body)
	}
	if err == nil && resp.StatusCode >= 300 {
		sanitizeClaudeError(resp)
	} else if err == nil && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// HTTP 200의 SSE·JSON 오류도 캡처보다 먼저 정제하되 정상 이벤트는 즉시 전달한다.
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), maxClaudeResponseLineBytes)
		resp.Body = &claudeResponseBody{body: resp.Body, scanner: scanner, token: used}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
	} else if err == nil {
		sanitizeClaudeJSON(resp, used)
	}
	return resp, err
}

// 일반 JSON은 한 객체로 검사한다. 줄별 검사로는 여러 줄·이스케이프된 오류를 놓칠 수 있다.
func sanitizeClaudeJSON(resp *http.Response, token string) {
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxClaudeResponseLineBytes+1))
	_ = resp.Body.Close()
	var data any
	if readErr != nil || len(raw) > maxClaudeResponseLineBytes || decodeClaudeJSON(raw, &data) != nil {
		raw = safeClaudeError(nil, http.StatusInternalServerError)
	} else {
		if object, ok := data.(map[string]any); ok && (object["type"] == "error" || object["error"] != nil) {
			raw = safeClaudeError(raw, claudeErrorStatus(raw))
		} else {
			// 정규화한 문자열에는 Unicode 이스케이프 대신 실제 값이 들어간다.
			raw, _ = json.Marshal(data)
			if token != "" {
				raw = bytes.ReplaceAll(raw, []byte(token), []byte("[redacted]"))
			}
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	resp.Header.Del("Content-Length")
}

// 도구 입력의 큰 정수·소수를 float64로 바꾸지 않고 원래 JSON 숫자대로 보존한다.
func decodeClaudeJSON(raw []byte, data any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(data); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("claude oauth: invalid JSON response")
	}
	return nil
}

func claudeErrorStatus(raw []byte) int {
	var event struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &event)
	switch event.Error.Type {
	case "rate_limit_error", "usage_limit_reached", "usage_not_included", "insufficient_quota":
		return http.StatusTooManyRequests
	case "authentication_error", "claude_login_required":
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// 오류 응답이 토큰을 되비춰도 로그·LLM 기록에는 고정 문구만 남긴다.
// 알 수 없는 429(식별 조건 거절 포함)는 한도 소진으로 분류하지 않는다.
func sanitizeClaudeError(resp *http.Response) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		raw = nil
	}
	body := safeClaudeError(raw, resp.StatusCode)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Type", "application/json")
}

func safeClaudeError(raw []byte, status int) []byte {
	var data struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	kind, message := "claude_request_failed", "Claude 요청이 거절됐다. 로그인·구독 상태를 확인한다"
	if status == http.StatusTooManyRequests {
		kind, message = "rate_limit_error", "Claude 요청이 제한됐다. 구독 한도 또는 요청 조건을 확인한다"
		if json.Unmarshal(raw, &data) == nil {
			switch data.Error.Type {
			case "usage_limit_reached", "usage_not_included", "insufficient_quota":
				kind, message = data.Error.Type, data.Error.Type+": Claude 구독 사용 한도에 도달했다"
			}
		}
	} else if status == http.StatusUnauthorized {
		kind, message = "claude_login_required", "Claude 구독 로그인이 필요하다"
	} else if status >= 500 {
		kind, message = "claude_server_error", "Claude 서버가 요청을 처리하지 못했다"
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
	return body
}

const maxClaudeResponseLineBytes = 16 << 20

type claudeResponseBody struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	token   string
	pending []byte
}

func (b *claudeResponseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 {
		if !b.scanner.Scan() {
			if err := b.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		var lines, dataLines []string
		frameBytes := 0
		isErrorEvent := false
		for {
			line := b.scanner.Text()
			frameBytes += len(line)
			if frameBytes > maxClaudeResponseLineBytes {
				return 0, errors.New("claude oauth: response frame too large")
			}
			if line == "" {
				break
			}
			lines = append(lines, line)
			if raw, ok := strings.CutPrefix(line, "data:"); ok {
				dataLines = append(dataLines, strings.TrimPrefix(raw, " "))
			}
			if strings.TrimSpace(line) == "event: error" {
				isErrorEvent = true
			}
			if !b.scanner.Scan() {
				if err := b.scanner.Err(); err != nil {
					return 0, err
				}
				break
			}
		}
		raw := []byte(strings.Join(dataLines, "\n"))
		var event map[string]any
		parsed := decodeClaudeJSON(raw, &event) == nil
		if isErrorEvent || (parsed && (event["type"] == "error" || event["error"] != nil)) {
			lines = []string{"event: error", "data: " + string(safeClaudeError(raw, claudeErrorStatus(raw)))}
		} else if parsed && len(dataLines) > 0 {
			encoded, _ := json.Marshal(event)
			for i, line := range lines {
				if strings.HasPrefix(line, "data:") {
					lines[i] = ""
				}
			}
			// 여러 data 줄을 한 줄로 합친다. JSON 값·도구 입력·서명은 그대로다.
			var retained []string
			for _, line := range lines {
				if line != "" {
					retained = append(retained, line)
				}
			}
			lines = append(retained, "data: "+string(encoded))
		}
		frame := strings.Join(lines, "\n") + "\n\n"
		if b.token != "" {
			frame = strings.ReplaceAll(frame, b.token, "[redacted]")
		}
		b.pending = []byte(frame)
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *claudeResponseBody) Close() error { return b.body.Close() }

func (t claudeTransport) send(req *http.Request, body []byte) (*http.Response, string, error) {
	token, _, err := t.tokens.Token(req.Context())
	if err != nil {
		if needsLogin(err) {
			resp := loginRequiredResponse(req)
			resp.Body = io.NopCloser(strings.NewReader(`{"error":{"type":"claude_login_required","message":"Claude 구독 로그인이 필요하다"}}`))
			resp.ContentLength = -1
			return resp, "", nil
		}
		return nil, "", err
	}
	out := req.Clone(req.Context())
	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		out.ContentLength = int64(len(body))
	}
	ClaudeOAuthHeaders(out.Header, token)
	resp, err := t.base.RoundTrip(out)
	return resp, token, err
}

func claudeRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(req.Body).Decode(&fields); err != nil || fields == nil {
		return nil, errors.New("claude oauth: invalid request body")
	}
	var system []map[string]json.RawMessage
	if raw := fields["system"]; len(raw) > 0 && string(raw) != "null" {
		// Norma는 배열을 쓰지만 문자열 system도 기존 지시문을 보존한 채 받아들인다.
		var text string
		if json.Unmarshal(raw, &text) == nil {
			encoded, _ := json.Marshal(text)
			system = []map[string]json.RawMessage{{"type": json.RawMessage(`"text"`), "text": encoded}}
		} else if json.Unmarshal(raw, &system) != nil {
			return nil, errors.New("claude oauth: invalid system prompt")
		}
	}
	var first string
	if len(system) > 0 {
		_ = json.Unmarshal(system[0]["text"], &first)
	}
	if first != ClaudeCodeIdentity && !strings.HasPrefix(first, ClaudeCodeIdentity+"\n") {
		identity, _ := json.Marshal(ClaudeCodeIdentity)
		system = append([]map[string]json.RawMessage{{"type": json.RawMessage(`"text"`), "text": identity}}, system...)
	}
	raw, err := json.Marshal(system)
	if err != nil {
		return nil, err
	}
	fields["system"] = raw
	return json.Marshal(fields)
}
