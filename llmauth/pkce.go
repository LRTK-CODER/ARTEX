package llmauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// pkceVerifierBytes 는 verifier 를 만들 난수 바이트 수다.
// RFC 7636 4.1 이 권하는 32바이트(base64url 43자)를 쓴다.
const pkceVerifierBytes = 32

// PKCE 는 RFC 7636 의 code_verifier 와 S256 code_challenge 쌍이다.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE 는 random 에서 32바이트를 읽어 PKCE 쌍을 만든다.
// random 이 nil 이면 crypto/rand 를 쓴다. 난수를 다 읽지 못하면 오류를 올린다.
func NewPKCE(random io.Reader) (PKCE, error) {
	if random == nil {
		random = rand.Reader
	}
	buf := make([]byte, pkceVerifierBytes)
	if _, err := io.ReadFull(random, buf); err != nil {
		return PKCE{}, fmt.Errorf("llmauth: read pkce random: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	return PKCE{Verifier: verifier, Challenge: PKCEChallenge(verifier)}, nil
}

// PKCEChallenge 는 verifier 의 S256 challenge(BASE64URL(SHA256(verifier)), 패딩 없음)를 돌려준다.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
