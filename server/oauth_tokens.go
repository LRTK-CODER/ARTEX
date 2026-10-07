package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmauth"
)

// oauthStore 는 한 프로필의 암호화된 자격 증명(db)을 llmauth.Store 로 보인다.
type oauthStore struct {
	pg        *db.DB
	cipher    *db.TokenCipher
	profileID int64
	// onRefresh 는 갱신 결과를 레지스트리에 알린다. 다시 로그인해야 하는지 기록하려고 쓴다.
	onRefresh func(err error)
}

func (s oauthStore) Load(ctx context.Context) (llmauth.Tokens, error) {
	cred, err := s.pg.OAuthCredentials(ctx, s.cipher, s.profileID)
	if err != nil {
		return llmauth.Tokens{}, wrapNoTokens(err)
	}
	return tokensFromCredentials(cred), nil
}

// Refresh 는 db 의 행 잠금 안에서 갱신한다. 잠금을 쥔 채 인증 서버를 부르므로 ctx 에 시간 상한이
// 있어야 한다. llmauth.TokenSource 가 30초 상한을 건 ctx 를 넘기고, 여기서는 그대로 전달한다.
func (s oauthStore) Refresh(ctx context.Context, staleAccessToken string, refresh func(context.Context, llmauth.Tokens) (llmauth.Tokens, error)) (llmauth.Tokens, error) {
	cred, err := s.pg.RefreshOAuthCredentials(ctx, s.cipher, s.profileID, staleAccessToken,
		func(ctx context.Context, current db.OAuthCredentials) (db.OAuthCredentials, error) {
			next, err := refresh(ctx, tokensFromCredentials(current))
			if err != nil {
				return db.OAuthCredentials{}, err
			}
			return db.OAuthCredentials{
				AccessToken:  next.AccessToken,
				RefreshToken: next.RefreshToken,
				ExpiresAt:    next.ExpiresAt,
				AccountID:    next.AccountID,
				PlanType:     next.PlanType,
			}, nil
		})
	if s.onRefresh != nil {
		s.onRefresh(err)
	}
	if err != nil {
		return llmauth.Tokens{}, wrapNoTokens(err)
	}
	return tokensFromCredentials(cred), nil
}

// wrapNoTokens 는 자격 증명이 없다는 db 오류를 llmauth.ErrNoTokens 로도 알아보게 감싼다.
// llmauth 와 agent 는 ErrNoTokens 로 "로그인이 필요하다"를 가른다.
func wrapNoTokens(err error) error {
	if errors.Is(err, db.ErrOAuthCredentialsNotFound) {
		return fmt.Errorf("%w: %w", llmauth.ErrNoTokens, err)
	}
	return err
}

// tokensFromCredentials 는 db 의 자격 증명을 llmauth 토큰으로 옮긴다. db 는 IDToken 을 저장하지 않는다.
func tokensFromCredentials(c db.OAuthCredentials) llmauth.Tokens {
	return llmauth.Tokens{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		AccountID:    c.AccountID,
		ExpiresAt:    c.ExpiresAt,
		PlanType:     c.PlanType,
	}
}

// oauthTokenRegistry 는 chatgpt_oauth 프로필마다 TokenSource 를 하나만 만들어 나눠 준다.
// 같은 프로필을 쓰는 프로바이더들이 TokenSource 를 공유해야 갱신이 한 번에 하나만 돌고,
// agent.Config 비교(applyLLM)에서도 같은 설정으로 보인다.
// nil 레지스트리(키를 불러오지 못했거나 테스트의 제로값 Server)는 OAuth 프로필을 쓸 수 없다고 답한다.
type oauthTokenRegistry struct {
	pg     *db.DB
	cipher *db.TokenCipher
	client *llmauth.Client

	mu      sync.Mutex
	sources map[int64]*llmauth.TokenSource
	// needsLogin 은 갱신이 "다시 로그인해야 한다"로 거절된 프로필이다. 메모리에만 둔다.
	// 갱신이 성공하거나 forget 으로 TokenSource 를 버리면 지운다.
	needsLogin map[int64]bool
}

func newOAuthTokenRegistry(pg *db.DB, cipher *db.TokenCipher, client *llmauth.Client) *oauthTokenRegistry {
	return &oauthTokenRegistry{pg: pg, cipher: cipher, client: client,
		sources: map[int64]*llmauth.TokenSource{}, needsLogin: map[int64]bool{}}
}

// loadOAuthTokenRegistry 는 keyDir 의 oauth.key 로 레지스트리를 만든다. 키를 쓰지 못하면 기록하고
// nil 을 돌려준다. 키 파일 문제로 API 키 프로필까지 막지 않으려고 서버 시작을 멈추지 않는다.
func loadOAuthTokenRegistry(pg *db.DB, keyDir string) *oauthTokenRegistry {
	if pg == nil {
		return nil
	}
	key, err := db.LoadOrCreateCredentialKey(keyDir)
	if err != nil {
		log.Printf("[auth] OAuth credential key unavailable, chatgpt_oauth profiles disabled: %v", err)
		return nil
	}
	cipher, err := db.NewTokenCipher(key)
	if err != nil {
		log.Printf("[auth] OAuth credential key unusable, chatgpt_oauth profiles disabled: %v", err)
		return nil
	}
	return newOAuthTokenRegistry(pg, cipher, &llmauth.Client{})
}

// source 는 프로필의 TokenSource 를 돌려준다. 처음 부를 때 만들고 그 뒤로는 같은 것을 준다.
func (r *oauthTokenRegistry) source(profileID int64) *llmauth.TokenSource {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if src := r.sources[profileID]; src != nil {
		return src
	}
	store := oauthStore{pg: r.pg, cipher: r.cipher, profileID: profileID,
		onRefresh: func(err error) { r.recordRefresh(profileID, err) }}
	src := llmauth.NewTokenSource(r.client, store, nil)
	r.sources[profileID] = src
	return src
}

// forget 은 프로필의 TokenSource 와 재로그인 표시를 버린다. 로그인을 새로 했거나 연결을 끊거나
// 프로필을 지웠을 때 부른다. 다음 source 호출이 새 TokenSource 를 만든다.
func (r *oauthTokenRegistry) forget(profileID int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sources, profileID)
	delete(r.needsLogin, profileID)
}

// errOAuthNotConnected 는 프로필에 저장된 OAuth 자격 증명이 없다는 뜻이다. 로그인이 필요하다.
var errOAuthNotConnected = errors.New("chatgpt oauth profile is not connected")

// connectedSource 는 자격 증명이 저장된 프로필의 TokenSource 를 돌려준다. 자격 증명이 없으면
// errOAuthNotConnected, 조회·복호화가 실패하면 그 오류를 올린다. 토큰은 갱신하지 않는다.
func (r *oauthTokenRegistry) connectedSource(ctx context.Context, profileID int64) (*llmauth.TokenSource, error) {
	if r == nil {
		return nil, errors.New("oauth credential key unavailable")
	}
	_, err := r.pg.OAuthCredentials(ctx, r.cipher, profileID)
	if errors.Is(err, db.ErrOAuthCredentialsNotFound) {
		return nil, errOAuthNotConnected
	}
	if err != nil {
		return nil, err
	}
	return r.source(profileID), nil
}

// loginRequired 는 프로필의 마지막 갱신이 다시 로그인해야 한다는 이유로 거절됐는지 알려 준다.
func (r *oauthTokenRegistry) loginRequired(profileID int64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.needsLogin[profileID]
}

func (r *oauthTokenRegistry) recordRefresh(profileID int64, err error) {
	needsLogin := needsChatGPTLogin(err)
	if err != nil && !needsLogin {
		// 네트워크 오류 같은 일시 실패는 재로그인 여부를 바꾸지 않는다.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if needsLogin {
		r.needsLogin[profileID] = true
		return
	}
	delete(r.needsLogin, profileID)
}
