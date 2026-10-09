package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
)

// ChatGPT 구독 로그인 API. redirect가 localhost:1455로 고정이라 원격 서버는 콜백을 받을 수 없다.
// 그래서 사용자가 실패한 콜백 URL을 붙여넣는 방식과 디바이스 코드 방식만 둔다.
// 흐름 상태(PKCE verifier, state, 디바이스 진행 상태)는 메모리에만 두고 DB에 쓰지 않는다.

const (
	// loginFlowTTL 은 흐름 하나가 유효한 시간이다. 디바이스 폴링 goroutine 도 이 시간이 지나면 끝난다.
	loginFlowTTL = 10 * time.Minute
	// loginFlowRetention 은 끝난 디바이스 흐름의 결과를 폴링으로 읽을 수 있게 남겨 두는 시간이다.
	loginFlowRetention = 5 * time.Minute
	// loginSaveTimeout 은 교환한 토큰을 저장하는 상한이다.
	loginSaveTimeout = 10 * time.Second
	loginRandomBytes = 32
	// maxLoginRequestBytes 는 로그인 요청 본문 상한이다. code·state·콜백 URL은 수 KB를 넘지 않는다.
	maxLoginRequestBytes = 32 << 10
	// callbackPort·callbackPath는 붙여넣은 콜백 URL이 가리켜야 할 포트다(llmauth.RedirectURI).
	callbackPort = "1455"
	callbackPath = "/auth/callback"
)

// loginErrorCode 는 화면이 문구를 고르는 짧은 오류 코드다.
type loginErrorCode string

const (
	loginErrInvalidRequest       loginErrorCode = "invalid_request"
	loginErrProfileNotFound      loginErrorCode = "profile_not_found"
	loginErrNotOAuthProfile      loginErrorCode = "not_oauth_profile"
	loginErrKeyUnavailable       loginErrorCode = "credential_key_unavailable"
	loginErrFlowNotFound         loginErrorCode = "flow_not_found"
	loginErrFlowExpired          loginErrorCode = "flow_expired"
	loginErrStateMismatch        loginErrorCode = "state_mismatch"
	loginErrAuthorizationDenied  loginErrorCode = "authorization_denied"
	loginErrExchangeFailed       loginErrorCode = "exchange_failed"
	loginErrDeviceLoginDisabled  loginErrorCode = "device_login_disabled"
	loginErrDeviceStartFailed    loginErrorCode = "device_start_failed"
	loginErrLoginCanceled        loginErrorCode = "login_canceled"
	loginErrSaveFailed           loginErrorCode = "save_failed"
	loginErrNotConnected         loginErrorCode = "not_connected"
	loginErrDisconnectFailed     loginErrorCode = "disconnect_failed"
	loginErrProfileLookupFailure loginErrorCode = "profile_lookup_failed"
	loginErrCallbackURLMismatch  loginErrorCode = "callback_url_mismatch"
	loginErrRequestTooLarge      loginErrorCode = "request_too_large"
	loginErrNetwork              loginErrorCode = "network_error"
	loginErrInternal             loginErrorCode = "internal_error"
)

// loginErrorMessages 는 오류 코드별 고정 문구다. 토큰·code·인증 서버 응답 원문을 싣지 않으려고
// 응답 문구는 이 표에서만 고른다.
var loginErrorMessages = map[loginErrorCode]string{
	loginErrInvalidRequest:       "요청 형식이 맞지 않다",
	loginErrProfileNotFound:      "LLM 프로필을 찾지 못했다",
	loginErrNotOAuthProfile:      "ChatGPT 구독 프로필이 아니다",
	loginErrKeyUnavailable:       "자격 증명 암호화 키를 쓸 수 없어 ChatGPT 구독 로그인을 할 수 없다",
	loginErrFlowNotFound:         "로그인 흐름을 찾지 못했다. 로그인을 다시 시작한다",
	loginErrFlowExpired:          "로그인 흐름이 만료됐다. 로그인을 다시 시작한다",
	loginErrStateMismatch:        "붙여넣은 주소가 이 로그인 흐름의 것이 아니다",
	loginErrAuthorizationDenied:  "ChatGPT 로그인이 승인되지 않았다",
	loginErrExchangeFailed:       "ChatGPT 로그인 코드를 토큰으로 바꾸지 못했다. 로그인을 다시 시작한다",
	loginErrDeviceLoginDisabled:  "이 계정은 디바이스 코드 로그인이 꺼져 있다. ChatGPT 보안 설정에서 켜거나 주소 붙여넣기 로그인을 쓴다",
	loginErrDeviceStartFailed:    "디바이스 코드 로그인을 시작하지 못했다",
	loginErrLoginCanceled:        "로그인이 취소됐다",
	loginErrSaveFailed:           "ChatGPT 구독 자격 증명을 저장하지 못했다",
	loginErrNotConnected:         "연결된 ChatGPT 구독 계정이 없다",
	loginErrDisconnectFailed:     "ChatGPT 구독 연결을 해제하지 못했다",
	loginErrProfileLookupFailure: "LLM 프로필을 읽지 못했다",
	loginErrCallbackURLMismatch:  "붙여넣은 주소가 로그인 콜백 주소(http://localhost:1455/auth/callback)가 아니다",
	loginErrRequestTooLarge:      "요청 본문이 너무 크다",
	loginErrNetwork:              "ChatGPT 인증 서버와 통신하지 못했다. 잠시 뒤 다시 시도한다",
	loginErrInternal:             "서버 내부 오류로 로그인을 진행하지 못했다",
}

func writeLoginErr(w http.ResponseWriter, status int, code loginErrorCode) {
	writeJSON(w, status, map[string]any{"error": loginErrorMessages[code], "code": code})
}

// deviceLoginStatus 는 디바이스 코드 흐름의 진행 상태다.
type deviceLoginStatus string

const (
	deviceLoginPending   deviceLoginStatus = "pending"
	deviceLoginSucceeded deviceLoginStatus = "succeeded"
	deviceLoginFailed    deviceLoginStatus = "failed"
	deviceLoginExpired   deviceLoginStatus = "expired"
)

// loginFlow 는 진행 중인 로그인 하나다. 붙여넣기 흐름은 pkce·state를, 디바이스 흐름은 status를 쓴다.
type loginFlow struct {
	id        string
	profileID int64
	isDevice  bool
	expiresAt time.Time
	pkce      llmauth.PKCE
	state     string
	// owner 는 Claude 흐름을 시작한 웹 세션의 해시이며 토큰 원문은 보관하지 않는다.
	owner        [32]byte
	isCompleting bool

	status  deviceLoginStatus
	errCode loginErrorCode
	// cancel 은 디바이스 폴링 goroutine을 멈춘다. done은 그 goroutine이 끝나면 닫힌다.
	cancel context.CancelFunc
	done   chan struct{}
}

// loginFlows 는 흐름을 ID로 들고 있다. 제로값을 그대로 쓴다.
type loginFlows struct {
	mu    sync.Mutex
	flows map[string]*loginFlow
	// now 가 nil 이면 실제 시계를 쓴다. 테스트가 만료를 확인할 때 바꾼다.
	now func() time.Time
	// profileLocks 는 프로필별로 자격 증명 저장과 연결 해제를 직렬화한다. 해제 뒤에 진행 중이던
	// 디바이스 저장이 자격 증명을 되살리지 않게 한다. 프로필 수만큼만 생긴다.
	profileLocks map[int64]*sync.Mutex
	// beforeDeviceSave 는 디바이스 흐름이 토큰을 받고 저장 잠금을 잡기 직전에 불린다.
	// nil 이면 아무것도 하지 않는다. 테스트가 해제와의 순서를 고정할 때 쓴다.
	beforeDeviceSave func()
}

// profileLock 은 프로필의 저장·해제 잠금을 돌려준다.
func (l *loginFlows) profileLock(profileID int64) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.profileLocks == nil {
		l.profileLocks = map[int64]*sync.Mutex{}
	}
	lock := l.profileLocks[profileID]
	if lock == nil {
		lock = &sync.Mutex{}
		l.profileLocks[profileID] = lock
	}
	return lock
}

// isRegistered 는 흐름이 아직 버려지지 않았는지 알려 준다. 새 흐름이나 연결 해제가 지운 흐름은 저장하지 않는다.
func (l *loginFlows) isRegistered(f *loginFlow) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flows[f.id] == f
}

func (l *loginFlows) clock() time.Time {
	if l.now == nil {
		return time.Now()
	}
	return l.now()
}

// add 는 흐름을 넣고 ID를 돌려준다. 같은 프로필의 이전 흐름은 버린다. 한 프로필에 두 디바이스
// 흐름이 함께 돌아 서로의 저장을 덮지 않게 한다.
func (l *loginFlows) add(id string, flow *loginFlow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropLocked(func(f *loginFlow) bool { return f.profileID == flow.profileID })
	if l.flows == nil {
		l.flows = map[string]*loginFlow{}
	}
	flow.id = id
	l.flows[id] = flow
}

// dropProfile 은 프로필의 흐름을 모두 버린다. 연결 해제 뒤 디바이스 흐름이 다시 연결하지 못하게 한다.
func (l *loginFlows) dropProfile(profileID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropLocked(func(f *loginFlow) bool { return f.profileID == profileID })
}

// dropLocked 는 match에 맞거나 보관 시간이 지난 흐름을 지우고, 도는 goroutine을 멈춘다.
func (l *loginFlows) dropLocked(match func(*loginFlow) bool) {
	now := l.clock()
	for id, f := range l.flows {
		if match(f) || now.After(f.expiresAt.Add(loginFlowRetention)) {
			if f.cancel != nil {
				f.cancel()
			}
			delete(l.flows, id)
		}
	}
}

// takeBrowser 는 붙여넣기 흐름을 state와 대조해 꺼낸다. 맞으면 흐름을 지워 한 번만 쓰게 한다.
// state가 다르면 흐름을 남겨 맞는 주소로 다시 시도할 수 있게 한다.
func (l *loginFlows) takeBrowser(id string, profileID int64, state string) (*loginFlow, loginErrorCode) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.flows[id]
	if f == nil || f.isDevice || f.profileID != profileID {
		return nil, loginErrFlowNotFound
	}
	if l.clock().After(f.expiresAt) {
		delete(l.flows, id)
		return nil, loginErrFlowExpired
	}
	if subtle.ConstantTimeCompare([]byte(f.state), []byte(state)) != 1 {
		return nil, loginErrStateMismatch
	}
	delete(l.flows, id)
	return f, ""
}

// deviceStatus 는 디바이스 흐름의 상태를 돌려준다. 만료 시각이 지났는데 아직 대기 중이면 만료로 본다.
func (l *loginFlows) deviceStatus(id string) (deviceLoginStatus, loginErrorCode, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.flows[id]
	if f == nil || !f.isDevice {
		return "", "", false
	}
	if f.status == deviceLoginPending && l.clock().After(f.expiresAt) {
		return deviceLoginExpired, loginErrFlowExpired, true
	}
	return f.status, f.errCode, true
}

// finishDevice 는 goroutine의 결과를 기록한다. 흐름이 이미 버려졌으면 기록해도 읽을 곳이 없다.
func (l *loginFlows) finishDevice(f *loginFlow, status deviceLoginStatus, code loginErrorCode) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f.status, f.errCode = status, code
}

// randomToken 은 흐름 ID와 state에 쓰는 추측할 수 없는 값이다.
func randomToken() (string, error) {
	buf := make([]byte, loginRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// loginProfile 은 요청의 profile_id가 로그인할 수 있는 chatgpt_oauth 프로필인지 본다.
// 쓸 수 없으면 응답을 쓰고 false를 돌려준다.
func (s *Server) loginProfile(w http.ResponseWriter, profileID int64) bool {
	pg := s.pg(w)
	if pg == nil {
		return false
	}
	if s.oauth == nil {
		// 키 없이 저장하면 나중에 풀 수 없으므로 교환 전에 거절한다.
		writeLoginErr(w, http.StatusServiceUnavailable, loginErrKeyUnavailable)
		return false
	}
	p, err := pg.ProfileByID(profileID)
	if err != nil {
		log.Printf("[auth] ChatGPT login: load LLM profile %d: %v", profileID, err)
		writeLoginErr(w, http.StatusInternalServerError, loginErrProfileLookupFailure)
		return false
	}
	if p == nil {
		writeLoginErr(w, http.StatusNotFound, loginErrProfileNotFound)
		return false
	}
	if p.AuthType != db.AuthChatGPTOAuth {
		writeLoginErr(w, http.StatusBadRequest, loginErrNotOAuthProfile)
		return false
	}
	return true
}

// decodeLoginRequest 는 상한을 둔 본문을 읽는다. 실패하면 응답을 쓰고 false를 돌려준다.
func decodeLoginRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginRequestBytes)
	err := decode(r, v)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeLoginErr(w, http.StatusRequestEntityTooLarge, loginErrRequestTooLarge)
	case err != nil:
		writeLoginErr(w, http.StatusBadRequest, loginErrInvalidRequest)
	default:
		return true
	}
	return false
}

// isCallbackURL 은 붙여넣은 주소가 로그인 콜백 주소인지 본다. 다른 주소를 붙여넣은 실수를 알려 주려는 것이다.
// 교환은 늘 고정 redirect_uri로 하므로 보안 경계는 아니다.
func isCallbackURL(u *url.URL) bool {
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1") &&
		u.Port() == callbackPort && u.Path == callbackPath
}

type loginProfileRequest struct {
	ProfileID int64 `json:"profile_id"`
}

// startChatGPTLogin 은 붙여넣기 로그인을 시작한다. authorize URL과 흐름 ID를 돌려준다.
func (s *Server) startChatGPTLogin(w http.ResponseWriter, r *http.Request) {
	var req loginProfileRequest
	if !decodeLoginRequest(w, r, &req) {
		return
	}
	if !s.loginProfile(w, req.ProfileID) {
		return
	}
	pkce, err := llmauth.NewPKCE(nil)
	if err != nil {
		writeLoginErr(w, http.StatusInternalServerError, loginErrInternal)
		return
	}
	flowID, errID := randomToken()
	state, errState := randomToken()
	if errID != nil || errState != nil {
		writeLoginErr(w, http.StatusInternalServerError, loginErrInternal)
		return
	}
	expiresAt := s.loginFlows.clock().Add(loginFlowTTL)
	s.loginFlows.add(flowID, &loginFlow{profileID: req.ProfileID, expiresAt: expiresAt, pkce: pkce, state: state})
	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id":       flowID,
		"authorize_url": s.oauth.client.AuthorizeURL(llmauth.RedirectURI, pkce.Challenge, state),
		"expires_at":    expiresAt,
	})
}

// completeChatGPTLogin 은 붙여넣은 콜백 URL(또는 code·state)로 붙여넣기 로그인을 끝낸다.
func (s *Server) completeChatGPTLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProfileID   int64  `json:"profile_id"`
		FlowID      string `json:"flow_id"`
		CallbackURL string `json:"callback_url"`
		Code        string `json:"code"`
		State       string `json:"state"`
	}
	if !decodeLoginRequest(w, r, &req) {
		return
	}
	code, state := req.Code, req.State
	if req.CallbackURL != "" {
		u, err := url.Parse(req.CallbackURL)
		if err != nil || !isCallbackURL(u) {
			writeLoginErr(w, http.StatusBadRequest, loginErrCallbackURLMismatch)
			return
		}
		q := u.Query()
		if q.Get("error") != "" {
			writeLoginErr(w, http.StatusBadRequest, loginErrAuthorizationDenied)
			return
		}
		code, state = q.Get("code"), q.Get("state")
	}
	if req.FlowID == "" || code == "" || state == "" {
		writeLoginErr(w, http.StatusBadRequest, loginErrInvalidRequest)
		return
	}
	if !s.loginProfile(w, req.ProfileID) {
		return
	}
	flow, errCode := s.loginFlows.takeBrowser(req.FlowID, req.ProfileID, state)
	if errCode != "" {
		writeLoginErr(w, http.StatusBadRequest, errCode)
		return
	}
	tokens, err := s.oauth.client.ExchangeCode(r.Context(), code, flow.pkce.Verifier, llmauth.RedirectURI)
	if err != nil {
		log.Printf("[auth] ChatGPT login for LLM profile %d: %v", req.ProfileID, err)
		writeLoginErr(w, http.StatusBadGateway, loginErrExchangeFailed)
		return
	}
	lock := s.loginFlows.profileLock(req.ProfileID)
	lock.Lock()
	err = s.saveChatGPTLogin(req.ProfileID, tokens)
	lock.Unlock()
	if err != nil {
		log.Printf("[auth] ChatGPT login for LLM profile %d: %v", req.ProfileID, err)
		writeLoginErr(w, http.StatusInternalServerError, loginErrSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connected": true, "plan_type": tokens.PlanType})
}

// startChatGPTDeviceLogin 은 디바이스 코드 로그인을 시작하고 폴링 goroutine을 띄운다.
// goroutine은 흐름 TTL, 서버 종료, 같은 프로필의 새 흐름·연결 해제 중 먼저 오는 것에 끝난다.
func (s *Server) startChatGPTDeviceLogin(w http.ResponseWriter, r *http.Request) {
	var req loginProfileRequest
	if !decodeLoginRequest(w, r, &req) {
		return
	}
	if !s.loginProfile(w, req.ProfileID) {
		return
	}
	client := s.oauth.client
	dc, err := client.StartDeviceCode(r.Context())
	if errors.Is(err, llmauth.ErrDeviceCodeDisabled) {
		writeLoginErr(w, http.StatusBadRequest, loginErrDeviceLoginDisabled)
		return
	}
	if err != nil {
		log.Printf("[auth] ChatGPT device login for LLM profile %d: %v", req.ProfileID, err)
		writeLoginErr(w, http.StatusBadGateway, loginErrDeviceStartFailed)
		return
	}
	flowID, err := randomToken()
	if err != nil {
		writeLoginErr(w, http.StatusInternalServerError, loginErrInternal)
		return
	}
	expiresAt := s.loginFlows.clock().Add(loginFlowTTL)
	ctx, cancel := context.WithTimeout(s.ctxOrBackground(), loginFlowTTL)
	flow := &loginFlow{profileID: req.ProfileID, isDevice: true, expiresAt: expiresAt,
		status: deviceLoginPending, cancel: cancel, done: make(chan struct{})}
	s.loginFlows.add(flowID, flow)
	go s.waitChatGPTDeviceLogin(ctx, cancel, flow, client, dc)
	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id":          flowID,
		"user_code":        dc.UserCode,
		"verification_url": dc.VerificationURL,
		"expires_at":       expiresAt,
	})
}

func (s *Server) waitChatGPTDeviceLogin(ctx context.Context, cancel context.CancelFunc, flow *loginFlow, client *llmauth.Client, dc llmauth.DeviceCode) {
	defer close(flow.done)
	defer cancel()
	tokens, err := client.WaitDeviceCode(ctx, dc)
	var netErr net.Error
	switch {
	// 만료는 흐름 TTL 이나 llmauth의 대기 상한만 뜻한다. HTTP 요청 하나의 타임아웃도
	// context.DeadlineExceeded로 보이므로 err가 아니라 흐름 ctx를 본다.
	case errors.Is(err, llmauth.ErrDeviceCodeTimeout), errors.Is(ctx.Err(), context.DeadlineExceeded):
		s.loginFlows.finishDevice(flow, deviceLoginExpired, loginErrFlowExpired)
		return
	case ctx.Err() != nil:
		// 서버 종료나 새 흐름·연결 해제로 멈췄다. 결과를 저장하지 않는다.
		s.loginFlows.finishDevice(flow, deviceLoginFailed, loginErrLoginCanceled)
		return
	case errors.As(err, &netErr):
		log.Printf("[auth] ChatGPT device login for LLM profile %d: %v", flow.profileID, err)
		s.loginFlows.finishDevice(flow, deviceLoginFailed, loginErrNetwork)
		return
	case err != nil:
		log.Printf("[auth] ChatGPT device login for LLM profile %d: %v", flow.profileID, err)
		s.loginFlows.finishDevice(flow, deviceLoginFailed, loginErrExchangeFailed)
		return
	}
	if s.loginFlows.beforeDeviceSave != nil {
		s.loginFlows.beforeDeviceSave()
	}
	// 연결 해제와 같은 잠금 안에서, 해제가 흐름을 지우지 않았을 때만 저장한다.
	lock := s.loginFlows.profileLock(flow.profileID)
	lock.Lock()
	defer lock.Unlock()
	if !s.loginFlows.isRegistered(flow) {
		s.loginFlows.finishDevice(flow, deviceLoginFailed, loginErrLoginCanceled)
		return
	}
	if err := s.saveChatGPTLogin(flow.profileID, tokens); err != nil {
		log.Printf("[auth] ChatGPT device login for LLM profile %d: %v", flow.profileID, err)
		s.loginFlows.finishDevice(flow, deviceLoginFailed, loginErrSaveFailed)
		return
	}
	s.loginFlows.finishDevice(flow, deviceLoginSucceeded, "")
}

// chatGPTDeviceLoginStatus 는 디바이스 흐름의 상태만 돌려준다. 토큰은 싣지 않는다.
func (s *Server) chatGPTDeviceLoginStatus(w http.ResponseWriter, r *http.Request) {
	status, code, ok := s.loginFlows.deviceStatus(r.PathValue("id"))
	if !ok {
		writeLoginErr(w, http.StatusNotFound, loginErrFlowNotFound)
		return
	}
	body := map[string]any{"status": status}
	if code != "" {
		body["code"], body["error"] = code, loginErrorMessages[code]
	}
	writeJSON(w, http.StatusOK, body)
}

// disconnectChatGPT 는 프로필의 자격 증명을 지우고 메모리의 TokenSource와 진행 중인 흐름을 버린다.
// 지우기만 하므로 암호화 키가 없어도 된다.
func (s *Server) disconnectChatGPT(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req loginProfileRequest
	if !decodeLoginRequest(w, r, &req) {
		return
	}
	// 저장 중인 디바이스 흐름이 있으면 그 저장이 끝난 뒤에 지운다. 잠금을 잡은 뒤 흐름을 버리므로
	// 아직 저장하지 않은 흐름은 isRegistered 에서 걸러진다.
	lock := s.loginFlows.profileLock(req.ProfileID)
	lock.Lock()
	defer lock.Unlock()
	s.loginFlows.dropProfile(req.ProfileID)
	err := pg.DeleteOAuthCredentials(r.Context(), req.ProfileID)
	if errors.Is(err, db.ErrOAuthCredentialsNotFound) {
		writeLoginErr(w, http.StatusNotFound, loginErrNotConnected)
		return
	}
	if err != nil {
		log.Printf("[auth] ChatGPT disconnect for LLM profile %d: %v", req.ProfileID, err)
		writeLoginErr(w, http.StatusInternalServerError, loginErrDisconnectFailed)
		return
	}
	s.afterOAuthCredentialsChanged(req.ProfileID)
	writeJSON(w, http.StatusOK, map[string]any{"connected": false})
}

// saveChatGPTLogin 은 로그인으로 받은 토큰과 플랜을 저장한다. 요청이 끊겨도 저장이 끝나게
// 서버 수명 ctx로 저장한다.
func (s *Server) saveChatGPTLogin(profileID int64, tokens llmauth.Tokens) error {
	return s.saveSubscriptionLogin(profileID, db.AuthChatGPTOAuth, tokens)
}

func (s *Server) saveSubscriptionLogin(profileID int64, authType db.AuthType, tokens llmauth.Tokens) error {
	ctx, cancel := context.WithTimeout(s.ctxOrBackground(), loginSaveTimeout)
	defer cancel()
	err := s.m.pg.SaveOAuthCredentialsForAuth(ctx, s.oauth.cipher, db.OAuthCredentials{
		ProfileID:    profileID,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    tokens.ExpiresAt,
		AccountID:    tokens.AccountID,
		PlanType:     tokens.PlanType,
	}, authType)
	if err != nil {
		return err
	}
	s.afterOAuthCredentialsChanged(profileID)
	return nil
}

// afterOAuthCredentialsChanged 는 자격 증명이 바뀐 프로필의 TokenSource·재로그인 표시를 버리고
// 캐시한 프로바이더를 다시 만든다. 캐시한 프로바이더는 옛 TokenSource를 쥐고 있어 옛 계정의
// 토큰을 계속 쓸 수 있기 때문이다.
func (s *Server) afterOAuthCredentialsChanged(profileID int64) {
	s.oauth.forget(profileID)
	s.invalidateProfileAgents()
	s.reapplyActiveProfile()
}
