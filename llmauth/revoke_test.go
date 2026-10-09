package llmauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRevokedSourceCannotReuseCachedToken(t *testing.T) {
	s := NewTokenSource(nil, nil, time.Now)
	s.hasTokens = true
	s.tokens = Tokens{AccessToken: "fake-cached", ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Revoke()
	if token, _, err := s.Token(context.Background()); !errors.Is(err, ErrNoTokens) || token != "" {
		t.Fatal("해제된 소스가 캐시 토큰을 반환했다")
	}
}
