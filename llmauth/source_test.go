package llmauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var errCommit = errors.New("memory store: commit failed")

// memoryStore 는 Store 의 메모리 구현이다. DB 구현과 같은 계약을 mutex 로 지킨다:
// 잠근 뒤 다시 읽어 stale 이 아니면 그대로 돌려주고, refresh 나 커밋이 실패하면 저장하지 않는다.
// DB 드라이버처럼 취소된 ctx 로는 커밋하지 않는다.
type memoryStore struct {
	mu          sync.Mutex
	tokens      Tokens
	hasTokens   bool
	saves       int
	failCommits int
}

func (m *memoryStore) Load(context.Context) (Tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasTokens {
		return Tokens{}, fmt.Errorf("memory store: %w", ErrNoTokens)
	}
	return m.tokens, nil
}

func (m *memoryStore) Refresh(ctx context.Context, staleAccessToken string, refresh func(context.Context, Tokens) (Tokens, error)) (Tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasTokens {
		return Tokens{}, fmt.Errorf("memory store: %w", ErrNoTokens)
	}
	if m.tokens.AccessToken != staleAccessToken {
		return m.tokens, nil
	}
	next, err := refresh(ctx, m.tokens)
	if err != nil {
		return Tokens{}, fmt.Errorf("memory store: refresh: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Tokens{}, fmt.Errorf("memory store: commit: %w", err)
	}
	if m.failCommits > 0 {
		m.failCommits--
		return Tokens{}, errCommit
	}
	m.tokens = next
	m.saves++
	return next, nil
}

// put 은 로그인 쪽이 새 토큰을 저장하는 것을 흉내 낸다.
func (m *memoryStore) put(tokens Tokens) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens, m.hasTokens = tokens, true
}

func (m *memoryStore) snapshot() (Tokens, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens, m.saves
}

func storedTokens(t *testing.T, refresh string, exp time.Time) *memoryStore {
	t.Helper()
	return &memoryStore{hasTokens: true, tokens: Tokens{
		AccessToken:  accessJWT(t, "acct-123", exp),
		RefreshToken: refresh,
		AccountID:    "acct-123",
		ExpiresAt:    exp,
	}}
}

func (f *fakeAuthServer) counts() (refreshCalls int, validRefresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls, f.validRefresh
}

func TestTokenSourceRefreshWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name        string
		expiresIn   time.Duration
		wantRefresh bool
	}{
		{"만료까지 넉넉함", time.Hour, false},
		{"만료 5분 1초 전", 5*time.Minute + time.Second, false},
		{"만료 정확히 5분 전", 5 * time.Minute, true},
		{"만료 1분 전", time.Minute, true},
		{"이미 만료", -time.Minute, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAuthServer(t, now)
			store := storedTokens(t, "refresh-0", now.Add(tc.expiresIn))
			original := store.tokens.AccessToken
			src := NewTokenSource(f.client(), store, func() time.Time { return now })

			access, account, err := src.Token(context.Background())
			if err != nil {
				t.Fatalf("Token: %v", err)
			}
			if account != "acct-123" {
				t.Fatalf("account = %q", account)
			}
			isRefreshed := access != original
			if isRefreshed != tc.wantRefresh {
				t.Fatalf("refreshed = %v, want %v", isRefreshed, tc.wantRefresh)
			}
			stored, saves := store.snapshot()
			if tc.wantRefresh && (saves != 1 || stored.RefreshToken != "refresh-1" || stored.AccessToken != access) {
				t.Fatalf("store after refresh = saves %d, refresh %q", saves, stored.RefreshToken)
			}
		})
	}
}

func TestTokenSourceConcurrentRefreshOnce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	store := storedTokens(t, "refresh-0", now.Add(time.Minute))
	src := NewTokenSource(f.client(), store, func() time.Time { return now })

	const callers = 20
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _, errs[i] = src.Token(context.Background())
		}()
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("caller %d got a different access token", i)
		}
	}
	if calls, _ := f.counts(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}

func TestTokenSourcesShareStore(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	store := storedTokens(t, "refresh-0", now.Add(time.Hour))
	clock := func() time.Time { return now }
	first := NewTokenSource(f.client(), store, clock)
	second := NewTokenSource(f.client(), store, clock)
	ctx := context.Background()

	// 둘 다 회전 전 토큰을 읽어 둔 뒤, 백엔드가 401 을 준 것처럼 둘 다 Invalidate 한다.
	for _, src := range []*TokenSource{first, second} {
		access, _, err := src.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		src.Invalidate(access)
	}
	a, _, err := first.Token(ctx)
	if err != nil {
		t.Fatalf("first Token: %v", err)
	}
	b, _, err := second.Token(ctx)
	if err != nil {
		t.Fatalf("second Token: %v (다른 쪽 회전 뒤 재로그인이 필요해지면 안 된다)", err)
	}
	if a != b {
		t.Fatal("second TokenSource did not pick up the rotated token")
	}
	if calls, _ := f.counts(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}

func TestTokenSourceInvalidate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	store := storedTokens(t, "refresh-0", now.Add(time.Hour))
	src := NewTokenSource(f.client(), store, func() time.Time { return now })
	ctx := context.Background()

	first, _, err := src.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	src.Invalidate(first)
	second, _, err := src.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	third, _, err := src.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := f.counts()
	if first == second || second != third || calls != 1 {
		t.Fatalf("Invalidate: changed=%v stable=%v refreshCalls=%d, want one refresh", first != second, second == third, calls)
	}
}

// TestTokenSourceRefreshesOnceForConcurrentRejections 는 여러 요청이 같은 옛 토큰으로 401 을 받아
// 저마다 Invalidate 해도 갱신은 한 번만 하는지 확인한다.
func TestTokenSourceRefreshesOnceForConcurrentRejections(t *testing.T) {
	const callers = 8
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	store := storedTokens(t, "refresh-0", now.Add(time.Hour))
	src := NewTokenSource(f.client(), store, func() time.Time { return now })
	ctx := context.Background()

	var allHaveOldToken, done sync.WaitGroup
	allHaveOldToken.Add(callers)
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			old, _, err := src.Token(ctx)
			allHaveOldToken.Done()
			if err != nil {
				errs[i] = err
				return
			}
			// 모두 옛 토큰을 들고 백엔드에서 401 을 받은 뒤에야 거부를 알린다.
			allHaveOldToken.Wait()
			src.Invalidate(old)
			results[i], _, errs[i] = src.Token(ctx)
		}()
	}
	done.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("caller %d got a different access token", i)
		}
	}
	if calls, _ := f.counts(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}

func TestTokenSourceRetriesAfterFailedRefresh(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	f.failRefreshes = 1
	store := storedTokens(t, "refresh-0", now.Add(time.Hour))
	src := NewTokenSource(f.client(), store, func() time.Time { return now })
	ctx := context.Background()

	before, _, err := src.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	src.Invalidate(before)
	if _, _, err := src.Token(ctx); err == nil {
		t.Fatal("Token after failed refresh: err = nil")
	}
	// 만료 전이어도 Invalidate 로 요청한 갱신을 끝내지 못했으니 다시 갱신해야 한다.
	after, _, err := src.Token(ctx)
	if err != nil {
		t.Fatalf("retry Token: %v", err)
	}
	if calls, _ := f.counts(); calls != 2 || after == before {
		t.Fatalf("refresh calls = %d, token changed = %v, want 2 calls and a new token", calls, after != before)
	}
}

func TestTokenSourceErrors(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name  string
		store func(t *testing.T) *memoryStore
		check func(t *testing.T, err error)
	}{
		{
			name:  "저장된 토큰 없음",
			store: func(*testing.T) *memoryStore { return &memoryStore{} },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoTokens) {
					t.Fatalf("err = %v, want ErrNoTokens", err)
				}
			},
		},
		{
			name:  "이미 회전된 refresh 토큰",
			store: func(t *testing.T) *memoryStore { return storedTokens(t, "refresh-used", now) },
			check: func(t *testing.T, err error) {
				var tokenErr *TokenError
				if !errors.As(err, &tokenErr) || !tokenErr.NeedsLogin() {
					t.Fatalf("err = %v, want TokenError needing login", err)
				}
			},
		},
		{
			name: "갱신 뒤 커밋 실패",
			store: func(t *testing.T) *memoryStore {
				s := storedTokens(t, "refresh-0", now)
				s.failCommits = 1
				return s
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, errCommit) {
					t.Fatalf("err = %v, want commit error", err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAuthServer(t, now)
			f.usedRefreshes["refresh-used"] = true
			src := NewTokenSource(f.client(), tc.store(t), func() time.Time { return now })
			_, _, err := src.Token(context.Background())
			tc.check(t, err)
		})
	}
}

func TestTokenSourceRetriesSaveAfterCommitFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	store := storedTokens(t, "refresh-0", now)
	store.failCommits = 1
	clock := func() time.Time { return now }
	src := NewTokenSource(f.client(), store, clock)
	ctx := context.Background()

	if _, _, err := src.Token(ctx); !errors.Is(err, errCommit) {
		t.Fatalf("first Token err = %v, want commit error", err)
	}
	// 서버는 이미 회전했다. 다음 호출은 서버를 다시 부르지 않고 회전된 토큰의 저장을 다시 해야 한다.
	if _, _, err := src.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	calls, valid := f.counts()
	stored, _ := store.snapshot()
	if calls != 1 || stored.RefreshToken != valid {
		t.Fatalf("refresh calls = %d, stored refresh %q, server valid %q", calls, stored.RefreshToken, valid)
	}
	// 재시작 흉내: 새 TokenSource 가 저장소만 보고 재로그인 없이 쓸 수 있어야 한다.
	restarted := NewTokenSource(f.client(), store, clock)
	stored, _ = store.snapshot()
	restarted.Invalidate(stored.AccessToken)
	if _, _, err := restarted.Token(ctx); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestTokenSourceSavesWhenCallerCancelsAfterRefresh(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	store := storedTokens(t, "refresh-0", now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 서버가 회전된 토큰을 돌려주는 순간 호출자가 요청을 멈춘다.
	f.onRefresh = cancel
	src := NewTokenSource(f.client(), store, func() time.Time { return now })

	_, _, _ = src.Token(ctx) // 호출자 쪽 결과는 취소 여부에 따라 달라도 된다. 저장 결과만 본다
	_, valid := f.counts()
	stored, _ := store.snapshot()
	if stored.RefreshToken != valid {
		t.Fatalf("stored refresh %q, server valid %q: rotated token was not saved", stored.RefreshToken, valid)
	}
}

func TestTokenSourceStopsAfterLoginError(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	f.usedRefreshes["refresh-used"] = true
	store := storedTokens(t, "refresh-used", now)
	src := NewTokenSource(f.client(), store, func() time.Time { return now })
	ctx := context.Background()

	for range 3 {
		var tokenErr *TokenError
		if _, _, err := src.Token(ctx); !errors.As(err, &tokenErr) || !tokenErr.NeedsLogin() {
			t.Fatalf("err = %v, want TokenError needing login", err)
		}
	}
	if calls, _ := f.counts(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1 (재로그인 전에는 서버를 다시 부르지 않는다)", calls)
	}
	// 다시 로그인해 저장소에 새 토큰이 들어오면 그 토큰을 쓴다.
	fresh := Tokens{AccessToken: accessJWT(t, "acct-123", now.Add(time.Hour)), RefreshToken: "refresh-0", AccountID: "acct-123", ExpiresAt: now.Add(time.Hour)}
	store.put(fresh)
	access, _, err := src.Token(ctx)
	if err != nil || access != fresh.AccessToken {
		t.Fatalf("after relogin: err = %v, got new token = %v", err, access == fresh.AccessToken)
	}
}

func TestTokenSourceWaitHonorsContext(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	src := NewTokenSource(f.client(), storedTokens(t, "refresh-0", now.Add(time.Hour)), func() time.Time { return now })
	src.lock <- struct{}{} // 다른 호출이 갱신 중인 상태를 만든다
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := src.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestTokenSourceRefreshPlanType 은 갱신 응답에 플랜이 없으면 저장된 플랜을 지키고,
// 있으면 새 플랜으로 바꿔 저장하는지 본다.
func TestTokenSourceRefreshPlanType(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name         string
		responsePlan string
		wantPlan     string
	}{
		{name: "응답에 플랜 없음", responsePlan: "", wantPlan: "plus"},
		{name: "응답의 새 플랜", responsePlan: "pro", wantPlan: "pro"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAuthServer(t, now)
			f.idTokenPlan = tc.responsePlan
			store := storedTokens(t, "refresh-0", now.Add(time.Minute))
			store.tokens.PlanType = "plus"
			src := NewTokenSource(f.client(), store, func() time.Time { return now })
			if _, _, err := src.Token(context.Background()); err != nil {
				t.Fatalf("Token: %v", err)
			}
			saved, saves := store.snapshot()
			if saves != 1 || saved.PlanType != tc.wantPlan {
				t.Fatalf("saves=%d plan=%q, want 1 save with plan %q", saves, saved.PlanType, tc.wantPlan)
			}
		})
	}
}
