package llmauth

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// refreshBefore 는 만료 몇 분 전부터 미리 갱신할지다. Codex CLI 와 같다.
const refreshBefore = 5 * time.Minute

// ErrNoTokens 는 저장소에 토큰이 없다는 뜻이다. 로그인이 필요하다. Store 구현은 이 오류를 감싸 올린다.
var ErrNoTokens = errors.New("llmauth: no stored tokens")

// Store 는 토큰을 영속하는 곳이다. 갱신마다 refresh 토큰이 회전하므로 Save 가 실패하면
// 다음 프로세스는 다시 로그인해야 한다.
type Store interface {
	// Load 는 저장된 토큰을 읽는다. 없으면 ErrNoTokens 를 감싸 올린다.
	Load(ctx context.Context) (Tokens, error)
	Save(ctx context.Context, tokens Tokens) error
}

// TokenSource 는 Codex 백엔드 호출에 쓸 access token 을 건넨다. 만료 5분 전이면 갱신하고,
// 같은 TokenSource 를 여러 goroutine 이 불러도 갱신은 한 번에 하나만 한다.
type TokenSource struct {
	client *Client
	store  Store
	now    func() time.Time

	// lock 은 크기 1 채널이다. sync.Mutex 와 달리 기다리는 쪽이 ctx 취소로 빠질 수 있다.
	lock      chan struct{}
	tokens    Tokens
	hasTokens bool
	isStale   atomic.Bool
}

// NewTokenSource 는 store 의 토큰을 client 로 갱신하는 TokenSource 를 만든다.
// now 가 nil 이면 time.Now 를 쓴다.
func NewTokenSource(client *Client, store Store, now func() time.Time) *TokenSource {
	if now == nil {
		now = time.Now
	}
	return &TokenSource{client: client, store: store, now: now, lock: make(chan struct{}, 1)}
}

// Token 은 유효한 access token 과 계정 ID 를 돌려준다.
// 저장소에 토큰이 없으면 ErrNoTokens, 갱신이 거절되면 *TokenError 를 올린다.
// 갱신은 성공했지만 저장에 실패하면 오류를 올린다. 새 토큰은 메모리에 남아 이 프로세스는 계속 쓴다.
func (s *TokenSource) Token(ctx context.Context) (accessToken, accountID string, err error) {
	select {
	case s.lock <- struct{}{}:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	defer func() { <-s.lock }()

	if !s.hasTokens {
		tokens, err := s.store.Load(ctx)
		if err != nil {
			return "", "", fmt.Errorf("llmauth: load tokens: %w", err)
		}
		s.tokens, s.hasTokens = tokens, true
	}
	isExpiring := !s.now().Add(refreshBefore).Before(s.tokens.ExpiresAt)
	if s.isStale.Swap(false) || isExpiring {
		if err := s.refresh(ctx); err != nil {
			return "", "", err
		}
	}
	return s.tokens.AccessToken, s.tokens.AccountID, nil
}

// Invalidate 는 지금 토큰을 버리게 해 다음 Token 호출이 갱신하게 한다.
// Codex 백엔드가 401 을 돌려줄 때 부른다.
func (s *TokenSource) Invalidate() {
	s.isStale.Store(true)
}

func (s *TokenSource) refresh(ctx context.Context) error {
	tokens, err := s.client.Refresh(ctx, s.tokens.RefreshToken)
	if err != nil {
		// 갱신이 실패했으니 다음 호출도 다시 갱신을 시도하게 한다.
		s.isStale.Store(true)
		return err
	}
	if tokens.IDToken == "" {
		tokens.IDToken = s.tokens.IDToken
	}
	// 옛 refresh 토큰은 서버에서 이미 무효가 됐으므로 저장 결과와 관계없이 새 토큰을 쓴다.
	s.tokens = tokens
	if err := s.store.Save(ctx, tokens); err != nil {
		return fmt.Errorf("llmauth: save refreshed tokens: %w", err)
	}
	return nil
}
