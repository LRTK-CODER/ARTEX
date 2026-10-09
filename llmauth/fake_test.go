package llmauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeJWT 는 서명 없는 JWT 모양 문자열을 만든다. ParseClaims는 서명을 보지 않는다.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
}

func accessJWT(t *testing.T, accountID string, exp time.Time) string {
	t.Helper()
	return fakeJWT(t, map[string]any{
		"exp":                         exp.Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
}

// fakeAuthServer 는 OpenAI 인증 서버의 토큰·디바이스 코드 엔드포인트를 흉내 낸다.
// 실제 서버처럼 client_id·grant_type·형식이 틀린 요청은 400으로 거부하고,
// 이미 쓴 refresh 토큰은 refresh_token_reused로 거부한다.
type fakeAuthServer struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	issued        int
	validRefresh  string
	usedRefreshes map[string]bool
	refreshCalls  int
	// failRefreshes 만큼 다음 refresh를 500으로 실패시킨다.
	failRefreshes int
	// onRefresh 는 refresh가 성공해 응답하기 직전에 불린다.
	onRefresh     func()
	exchangeForms []url.Values
	// pollStatuses 는 폴링 응답 상태를 차례로 정한다. 다 쓰면 성공을 돌려준다.
	pollStatuses    []int
	isAlwaysPending bool
	pollCalls       int
	expiresIn       time.Duration
	now             time.Time
	accountID       string
	usercodeCode    int
	// idTokenPlan·accessTokenPlan이 비어 있지 않으면 각 토큰에 chatgpt_plan_type 클레임으로 싣는다.
	idTokenPlan     string
	accessTokenPlan string
}

func newFakeAuthServer(t *testing.T, now time.Time) *fakeAuthServer {
	f := &fakeAuthServer{
		t:             t,
		usedRefreshes: map[string]bool{},
		expiresIn:     time.Hour,
		now:           now,
		accountID:     "acct-123",
		validRefresh:  "refresh-0",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", f.token)
	mux.HandleFunc("POST /api/accounts/deviceauth/usercode", f.usercode)
	mux.HandleFunc("POST /api/accounts/deviceauth/token", f.deviceToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAuthServer) client() *Client {
	return &Client{HTTPClient: f.srv.Client(), Issuer: f.srv.URL}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // 테스트 응답 쓰기 실패는 클라이언트 쪽 단언에서 드러난다
}

// issueLocked 는 새 토큰 묶음을 만들고 refresh 토큰을 회전시킨다. f.mu를 잡고 부른다.
func (f *fakeAuthServer) issueLocked(w http.ResponseWriter) {
	f.issued++
	if f.validRefresh != "" {
		f.usedRefreshes[f.validRefresh] = true
	}
	f.validRefresh = fmt.Sprintf("refresh-%d", f.issued)
	writeJSON(w, http.StatusOK, map[string]string{
		"id_token": fakeJWT(f.t, map[string]any{"https://api.openai.com/auth": f.authClaim(f.idTokenPlan)}),
		// 실제 서버처럼 발급마다 다른 access token을 준다.
		"access_token": fakeJWT(f.t, map[string]any{
			"jti":                         f.issued,
			"exp":                         f.now.Add(f.expiresIn).Unix(),
			"https://api.openai.com/auth": f.authClaim(f.accessTokenPlan),
		}),
		"refresh_token": f.validRefresh,
	})
}

// authClaim 은 토큰의 계정 클레임 객체다. plan이 비면 플랜 클레임을 뺀다.
func (f *fakeAuthServer) authClaim(plan string) map[string]any {
	claim := map[string]any{"chatgpt_account_id": f.accountID}
	if plan != "" {
		claim["chatgpt_plan_type"] = plan
	}
	return claim
}

func (f *fakeAuthServer) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Header.Get("Content-Type") {
	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		form := r.PostForm
		f.exchangeForms = append(f.exchangeForms, form)
		if form.Get("grant_type") != "authorization_code" || form.Get("client_id") != ClientID ||
			form.Get("code") == "" || form.Get("code_verifier") == "" || form.Get("redirect_uri") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		if form.Get("code") == "bad-code" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		f.issueLocked(w)
	case "application/json":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			body["grant_type"] != "refresh_token" || body["client_id"] != ClientID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		f.refreshCalls++
		if f.usedRefreshes[body["refresh_token"]] {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "refresh_token_reused", "message": "already used"}})
			return
		}
		if body["refresh_token"] != f.validRefresh {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		if f.failRefreshes > 0 {
			f.failRefreshes--
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		if f.onRefresh != nil {
			f.onRefresh()
		}
		f.issueLocked(w)
	default:
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_media_type"})
	}
}

func (f *fakeAuthServer) usercode(w http.ResponseWriter, r *http.Request) {
	if f.usercodeCode != 0 {
		w.WriteHeader(f.usercodeCode)
		return
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["client_id"] != ClientID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"device_auth_id": "dev-1", "user_code": "ABCD-1234", "interval": "5"})
}

func (f *fakeAuthServer) deviceToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body["device_auth_id"] != "dev-1" || body["user_code"] != "ABCD-1234" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	call := f.pollCalls
	f.pollCalls++
	if call < len(f.pollStatuses) {
		w.WriteHeader(f.pollStatuses[call])
		return
	}
	if f.isAlwaysPending {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"authorization_code": "device-auth-code",
		"code_verifier":      "device-verifier",
		"code_challenge":     PKCEChallenge("device-verifier"),
	})
}

// fakeClock 은 sleep이 부를 때만 시간이 흐르는 시계다.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}
