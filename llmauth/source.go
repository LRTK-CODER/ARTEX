package llmauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// refreshBefore 는 만료 몇 분 전부터 미리 갱신할지다. Codex CLI 와 같다.
	refreshBefore = 5 * time.Minute
	// refreshTimeout 은 갱신과 저장 한 번에 주는 상한이다. 호출자 ctx 와 떼어 놓으므로 따로 둔다.
	refreshTimeout = 30 * time.Second
)

// ErrNoTokens 는 저장소에 토큰이 없다는 뜻이다. 로그인이 필요하다. Store 구현은 이 오류를 감싸 올린다.
var ErrNoTokens = errors.New("llmauth: no stored tokens")

// Store 는 토큰을 영속하는 곳이다. refresh 토큰은 갱신마다 회전하므로, 갱신과 저장을
// 저장소의 잠금 안에서 함께 해야 여러 TokenSource·프로세스가 서로의 회전을 깨지 않는다.
type Store interface {
	// Load 는 저장된 토큰을 읽는다. 없으면 ErrNoTokens 를 감싸 올린다.
	Load(ctx context.Context) (Tokens, error)
	// Refresh 는 저장된 토큰을 잠그고 다시 읽는다. 저장된 access token 이 staleAccessToken 과
	// 다르면 다른 쪽이 이미 갱신한 것이므로 refresh 를 부르지 않고 그 토큰을 돌려준다.
	// 같으면 refresh 를 불러 받은 토큰을 같은 잠금 안에서 저장하고 돌려준다.
	// refresh 나 저장이 실패하면 아무것도 저장하지 않고 오류를 올린다(refresh 의 오류는 감싸 올린다).
	Refresh(ctx context.Context, staleAccessToken string, refresh func(ctx context.Context, current Tokens) (Tokens, error)) (Tokens, error)
}

// TokenSource 는 Codex 백엔드 호출에 쓸 access token 을 건넨다. 만료 5분 전이면 갱신하고,
// 같은 TokenSource 를 여러 goroutine 이 불러도 갱신은 한 번에 하나만 한다.
// 토큰을 담고 있으므로 fmt 로 출력해도 내용은 보이지 않는다(String).
type TokenSource struct {
	client *Client
	store  Store
	now    func() time.Time

	// lock 은 크기 1 채널이다. sync.Mutex 와 달리 기다리는 쪽이 ctx 취소로 빠질 수 있다.
	// 아래 필드는 lock 을 잡고서만 읽고 쓴다.
	lock      chan struct{}
	tokens    Tokens
	hasTokens bool
	// pending 은 인증 서버가 회전해 준 뒤 저장하지 못한 토큰이다. 옛 refresh 토큰은 서버에서
	// 이미 무효라서, 다음 갱신은 서버를 다시 부르지 않고 이 토큰의 저장을 다시 시도한다.
	pending    Tokens
	hasPending bool
	// loginErr 는 다시 로그인해야 하는 갱신 오류다. 저장된 토큰이 바뀔 때까지 서버를 다시 부르지 않는다.
	loginErr error
	// mustRefresh 는 Invalidate 로 요청된 갱신을 끝내지 못했다는 표시다. 다음 호출이 다시 갱신한다.
	mustRefresh bool

	// rejectedMu 는 rejected 를 지킨다. Invalidate 는 lock 을 기다리지 않아야 하므로 따로 둔다.
	rejectedMu sync.Mutex
	// rejected 는 백엔드가 거부해 Invalidate 로 넘어온 access token 이다. 다음 Token 이 지금 토큰과
	// 견주고 비운다. 이미 갱신돼 지난 토큰이면 다시 갱신하지 않는다.
	rejected map[string]struct{}
}

// NewTokenSource 는 store 의 토큰을 client 로 갱신하는 TokenSource 를 만든다.
// now 가 nil 이면 time.Now 를 쓴다.
func NewTokenSource(client *Client, store Store, now func() time.Time) *TokenSource {
	if now == nil {
		now = time.Now
	}
	return &TokenSource{client: client, store: store, now: now, lock: make(chan struct{}, 1)}
}

// String 은 토큰 원문을 숨긴다.
func (s *TokenSource) String() string { return "llmauth.TokenSource{...}" }

// GoString 은 %#v 에서도 토큰 원문을 숨긴다.
func (s *TokenSource) GoString() string { return s.String() }

// Token 은 유효한 access token 과 계정 ID 를 돌려준다.
// 저장소에 토큰이 없으면 ErrNoTokens, 갱신이 거절되면 *TokenError 를 올린다.
// 갱신과 저장은 호출자 ctx 의 취소와 떼어 최대 30초 동안 끝까지 한다. 회전된 토큰을 저장하기 전에
// 멈추면 저장소의 refresh 토큰이 무효가 되기 때문이다. 그동안 호출자는 기다린다.
// 갱신은 됐지만 저장이 실패하면 오류를 올리고, 다음 호출이 저장을 다시 시도한다.
func (s *TokenSource) Token(ctx context.Context) (accessToken, accountID string, err error) {
	select {
	case s.lock <- struct{}{}:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	defer func() { <-s.lock }()

	if !s.hasTokens || s.loginErr != nil {
		if err := s.reload(ctx); err != nil {
			return "", "", err
		}
	}
	isExpiring := !s.now().Add(refreshBefore).Before(s.tokens.ExpiresAt)
	if s.takeRejected(s.tokens.AccessToken) || s.mustRefresh || isExpiring || s.hasPending {
		if err := s.refresh(ctx); err != nil {
			// 갱신을 끝내지 못했으니 다음 호출도 다시 갱신하게 한다.
			s.mustRefresh = true
			return "", "", err
		}
		s.mustRefresh = false
	}
	return s.tokens.AccessToken, s.tokens.AccountID, nil
}

// Invalidate 는 staleAccessToken 이 지금 토큰이면 다음 Token 호출이 갱신하게 한다.
// Codex 백엔드가 그 토큰에 401 을 돌려줄 때 부른다. 여러 요청이 같은 옛 토큰으로 401 을 받아도
// 갱신은 한 번만 한다. 이미 갱신돼 지난 토큰이면 아무것도 하지 않는다.
func (s *TokenSource) Invalidate(staleAccessToken string) {
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	if s.rejected == nil {
		s.rejected = make(map[string]struct{})
	}
	s.rejected[staleAccessToken] = struct{}{}
}

// takeRejected 는 current 가 거부된 토큰인지 알려 주고 기록을 비운다. 남은 기록은 모두 current 보다
// 앞선 토큰이라 다시 볼 일이 없다. lock 을 잡고 부른다.
func (s *TokenSource) takeRejected(current string) bool {
	s.rejectedMu.Lock()
	defer s.rejectedMu.Unlock()
	_, isRejected := s.rejected[current]
	clear(s.rejected)
	return isRejected
}

// reload 는 저장소에서 토큰을 읽는다. 다시 로그인해야 하는 상태라면 저장된 토큰이 바뀌었을 때만
// (다시 로그인했을 때만) 그 상태를 푼다.
func (s *TokenSource) reload(ctx context.Context) error {
	tokens, err := s.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("llmauth: load tokens: %w", err)
	}
	if s.loginErr != nil {
		if tokens.AccessToken == s.tokens.AccessToken {
			return s.loginErr
		}
		// 갱신 실패로 남은 표시는 옛 토큰에 대한 것이라 새로 로그인한 토큰에는 맞지 않는다.
		s.loginErr = nil
		s.hasPending, s.pending = false, Tokens{}
		s.mustRefresh = false
	}
	s.tokens, s.hasTokens = tokens, true
	return nil
}

func (s *TokenSource) refresh(ctx context.Context) error {
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	tokens, err := s.store.Refresh(refreshCtx, s.tokens.AccessToken, s.rotate)
	if err != nil {
		var tokenErr *TokenError
		if errors.As(err, &tokenErr) && tokenErr.NeedsLogin() {
			s.loginErr = err
		}
		return fmt.Errorf("llmauth: refresh tokens: %w", err)
	}
	// 저장소가 다른 쪽이 갱신한 토큰을 돌려줬어도 저장되지 않은 토큰은 쓸모가 없다.
	s.tokens = tokens
	s.hasPending, s.pending = false, Tokens{}
	return nil
}

// rotate 는 Store.Refresh 가 잠금 안에서 부르는 갱신 함수다. 지난번에 회전만 하고 저장하지 못한
// 토큰이 있으면 서버를 다시 부르지 않고 그것을 돌려준다.
func (s *TokenSource) rotate(ctx context.Context, current Tokens) (Tokens, error) {
	if s.hasPending {
		return s.pending, nil
	}
	next, err := s.client.Refresh(ctx, current.RefreshToken)
	if err != nil {
		return Tokens{}, err
	}
	if next.IDToken == "" {
		next.IDToken = current.IDToken
	}
	// 서버는 이미 옛 refresh 토큰을 무효로 했다. 저장이 실패해도 잃지 않게 먼저 들고 있는다.
	s.pending, s.hasPending = next, true
	return next, nil
}
