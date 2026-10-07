package llmauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// memoryStore 는 Store 의 메모리 구현이다. 저장소 인터페이스를 시험하는 데만 쓴다.
type memoryStore struct {
	mu        sync.Mutex
	tokens    Tokens
	hasTokens bool
	saves     int
	saveErr   error
}

func (m *memoryStore) Load(context.Context) (Tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasTokens {
		return Tokens{}, fmt.Errorf("memory store: %w", ErrNoTokens)
	}
	return m.tokens, nil
}

func (m *memoryStore) Save(_ context.Context, tokens Tokens) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.tokens, m.hasTokens = tokens, true
	m.saves++
	return nil
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
			if tc.wantRefresh && (store.saves != 1 || store.tokens.RefreshToken != "refresh-1" || store.tokens.AccessToken != access) {
				t.Fatalf("store after refresh = saves %d, refresh %q", store.saves, store.tokens.RefreshToken)
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
	if f.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", f.refreshCalls)
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
	src.Invalidate()
	second, _, err := src.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	third, _, err := src.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second != third || f.refreshCalls != 1 {
		t.Fatalf("Invalidate: changed=%v stable=%v refreshCalls=%d, want one refresh", first != second, second == third, f.refreshCalls)
	}
}

func TestTokenSourceErrors(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	saveFailed := errors.New("disk full")
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
			name: "갱신 뒤 저장 실패",
			store: func(t *testing.T) *memoryStore {
				s := storedTokens(t, "refresh-0", now)
				s.saveErr = saveFailed
				return s
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, saveFailed) {
					t.Fatalf("err = %v, want save error", err)
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

func TestTokenSourceKeepsRefreshedTokensAfterSaveFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := newFakeAuthServer(t, now)
	store := storedTokens(t, "refresh-0", now)
	store.saveErr = errors.New("disk full")
	src := NewTokenSource(f.client(), store, func() time.Time { return now })
	ctx := context.Background()
	if _, _, err := src.Token(ctx); err == nil {
		t.Fatal("first Token err = nil, want save error")
	}
	// 옛 refresh 토큰은 이미 무효다. 다시 갱신하지 않고 메모리의 새 토큰을 써야 한다.
	if _, _, err := src.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if f.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", f.refreshCalls)
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
