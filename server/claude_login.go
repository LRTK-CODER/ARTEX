package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
)

const claudeLoginRequiredMessage = "Claude 구독 로그인이 필요하다. LLM 프로필 화면에서 로그인한다"

func writeClaudeLoginErr(w http.ResponseWriter, status int, code loginErrorCode) {
	// 제공자 이름·콜백 주소만 바꾸고 고정 오류 코드를 기존 화면 패턴과 함께 쓴다.
	message := strings.ReplaceAll(loginErrorMessages[code], "ChatGPT", "Claude")
	message = strings.ReplaceAll(message, llmauth.RedirectURI, llmauth.ClaudeRedirectURI)
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}

func (s *Server) claudeLoginProfile(w http.ResponseWriter, id int64) bool {
	pg := s.pg(w)
	if pg == nil {
		return false
	}
	if s.oauth == nil {
		writeClaudeLoginErr(w, 503, loginErrKeyUnavailable)
		return false
	}
	p, err := pg.ProfileByID(id)
	if err != nil {
		writeClaudeLoginErr(w, 500, loginErrProfileLookupFailure)
		return false
	}
	if p == nil {
		writeClaudeLoginErr(w, 404, loginErrProfileNotFound)
		return false
	}
	if p.AuthType != db.AuthClaudeOAuth {
		writeClaudeLoginErr(w, 400, loginErrNotOAuthProfile)
		return false
	}
	return true
}

func (s *Server) startClaudeLogin(w http.ResponseWriter, r *http.Request) {
	var req loginProfileRequest
	if !decodeLoginRequest(w, r, &req) || !s.claudeLoginProfile(w, req.ProfileID) {
		return
	}
	pkce, err := llmauth.NewPKCE(nil)
	if err != nil {
		writeClaudeLoginErr(w, 500, loginErrInternal)
		return
	}
	id, errID := randomToken()
	state, errState := randomToken()
	if errID != nil || errState != nil {
		writeClaudeLoginErr(w, 500, loginErrInternal)
		return
	}
	lock := s.loginFlows.profileLock(req.ProfileID)
	lock.Lock()
	defer lock.Unlock()
	expires := s.claudeLoginFlows.clock().Add(5 * time.Minute)
	s.claudeLoginFlows.add(id, &loginFlow{profileID: req.ProfileID, expiresAt: expires, pkce: pkce, state: state, owner: sha256.Sum256([]byte(extractToken(r)))})
	writeJSON(w, 200, map[string]any{"flow_id": id, "authorize_url": s.oauth.claudeClient.AuthorizeURL(pkce.Challenge, state), "expires_at": expires})
}

func parseClaudeCallback(input string) (string, string, loginErrorCode) {
	input = strings.TrimSpace(input)
	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err != nil || u.Scheme != "http" || u.Host != "localhost:53692" || u.Path != "/callback" || u.User != nil || u.Fragment != "" {
			return "", "", loginErrCallbackURLMismatch
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "", "", loginErrInvalidRequest
		}
		if q.Get("error") != "" {
			return "", "", loginErrAuthorizationDenied
		}
		if len(q["code"]) != 1 || len(q["state"]) != 1 {
			return "", "", loginErrInvalidRequest
		}
		return q.Get("code"), q.Get("state"), ""
	}
	parts := strings.Split(input, "#")
	if len(parts) != 2 {
		return "", "", loginErrInvalidRequest
	}
	return parts[0], parts[1], ""
}

// claimClaudeBrowser 는 교환 중에도 흐름을 남겨 연결 해제가 뒤늦은 저장을 취소할 수 있게 한다.
func (l *loginFlows) claimClaudeBrowser(id string, profileID int64, state string, owner [32]byte) (*loginFlow, loginErrorCode) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.flows[id]
	if f == nil || f.profileID != profileID || f.isCompleting || subtle.ConstantTimeCompare(f.owner[:], owner[:]) != 1 {
		return nil, loginErrFlowNotFound
	}
	if !l.clock().Before(f.expiresAt) {
		delete(l.flows, id)
		return nil, loginErrFlowExpired
	}
	if subtle.ConstantTimeCompare([]byte(f.state), []byte(state)) != 1 {
		return nil, loginErrStateMismatch
	}
	f.isCompleting = true
	return f, ""
}

func (l *loginFlows) finishBrowser(f *loginFlow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.flows[f.id] == f {
		delete(l.flows, f.id)
	}
}

func (s *Server) completeClaudeLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProfileID   int64  `json:"profile_id"`
		FlowID      string `json:"flow_id"`
		CallbackURL string `json:"callback_url"`
	}
	if !decodeLoginRequest(w, r, &req) {
		return
	}
	code, state, errCode := parseClaudeCallback(req.CallbackURL)
	if errCode != "" {
		writeClaudeLoginErr(w, 400, errCode)
		return
	}
	if req.FlowID == "" || code == "" || state == "" {
		writeClaudeLoginErr(w, 400, loginErrInvalidRequest)
		return
	}
	if !s.claudeLoginProfile(w, req.ProfileID) {
		return
	}
	f, errCode := s.claudeLoginFlows.claimClaudeBrowser(req.FlowID, req.ProfileID, state, sha256.Sum256([]byte(extractToken(r))))
	if errCode != "" {
		writeClaudeLoginErr(w, 400, errCode)
		return
	}
	defer s.claudeLoginFlows.finishBrowser(f)
	tokens, err := s.oauth.claudeClient.ExchangeCode(r.Context(), code, state, f.pkce.Verifier)
	if err != nil {
		writeClaudeLoginErr(w, 502, loginErrExchangeFailed)
		return
	}
	lock := s.loginFlows.profileLock(req.ProfileID)
	lock.Lock()
	defer lock.Unlock()
	if !s.claudeLoginFlows.isRegistered(f) {
		writeClaudeLoginErr(w, 400, loginErrLoginCanceled)
		return
	}
	if !s.claudeLoginFlows.clock().Before(f.expiresAt) {
		writeClaudeLoginErr(w, 400, loginErrFlowExpired)
		return
	}
	if !s.claudeLoginProfile(w, req.ProfileID) {
		return
	}
	if err := s.saveSubscriptionLogin(req.ProfileID, db.AuthClaudeOAuth, tokens); err != nil {
		writeClaudeLoginErr(w, 500, loginErrSaveFailed)
		return
	}
	writeJSON(w, 200, map[string]any{"connected": true})
}

func (s *Server) disconnectClaude(w http.ResponseWriter, r *http.Request) {
	var req loginProfileRequest
	if !decodeLoginRequest(w, r, &req) || !s.claudeLoginProfile(w, req.ProfileID) {
		return
	}
	lock := s.loginFlows.profileLock(req.ProfileID)
	lock.Lock()
	defer lock.Unlock()
	s.claudeLoginFlows.dropProfile(req.ProfileID)
	err := s.m.pg.DeleteOAuthCredentials(r.Context(), req.ProfileID)
	if err != nil && !errors.Is(err, db.ErrOAuthCredentialsNotFound) {
		writeClaudeLoginErr(w, 500, loginErrDisconnectFailed)
		return
	}
	s.afterOAuthCredentialsChanged(req.ProfileID)
	writeJSON(w, 200, map[string]any{"connected": false})
}

func (s *Server) claudeLoginStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeClaudeLoginErr(w, 400, loginErrInvalidRequest)
		return
	}
	if !s.claudeLoginProfile(w, id) {
		return
	}
	profiles, err := s.m.pg.ListProfiles()
	if err != nil {
		writeClaudeLoginErr(w, 500, loginErrProfileLookupFailure)
		return
	}
	for _, p := range profiles {
		if p.ID == id {
			writeJSON(w, 200, llmProfileDTO(p, s.oauth).OAuth)
			return
		}
	}
	writeClaudeLoginErr(w, 404, loginErrProfileNotFound)
}

func applyClaudeOAuthRules(cfg *agent.Config) {
	cfg.AuthType = db.AuthClaudeOAuth
	cfg.Format = "anthropic"
	cfg.APIKey = ""
	cfg.BaseURL = agent.ClaudeBaseURL
	cfg.Retry.ConnectAttempts, cfg.Retry.EmptyAttempts, cfg.Retry.StreamAttempts = -1, -1, -1
}

func (s *Server) prepareSubscriptionTest(ctx context.Context, cfg *agent.Config, stored *db.LLMProfile, authType db.AuthType) string {
	if authType == db.AuthChatGPTOAuth {
		return s.prepareOAuthTest(ctx, cfg, stored)
	}
	if stored == nil || stored.AuthType != authType {
		return "선택한 구독 인증 방식으로 프로필을 먼저 저장한다"
	}
	applyClaudeOAuthRules(cfg)
	cfg.BaseURL = s.claudeURL()
	src, err := s.oauth.connectedSourceForAuth(ctx, stored.ID, db.AuthClaudeOAuth)
	if err != nil {
		return claudeLoginRequiredMessage
	}
	cfg.OAuthTokens = src
	return ""
}

func (s *Server) claudeURL() string {
	if s.claudeBaseURL != "" {
		return s.claudeBaseURL
	}
	return agent.ClaudeBaseURL
}
