package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// sigAsset 은 SHA256SUMS 파일 바이트에 대한 ed25519 서명(64바이트 원본)이다. 소유자가 오프라인 개인 키로
// 만들어 초안 릴리스에 붙인다. 순수 ed25519라 `openssl pkeyutl -sign -rawin`으로도 같은 서명이 나온다.
const sigAsset = sumsAsset + ".sig"

// maxSumsSize 는 SHA256SUMS를 읽는 상한이다. 플랫폼 다섯 개면 수백 바이트라 1 MiB는 넉넉하다.
const maxSumsSize = 1 << 20

// releaseKeys 는 릴리스 서명을 믿는 공개 키 목록이다. 패키지 초기화 때 한 번 해석한다.
var releaseKeys = mustParseKeys(releaseKeysBase64)

// ErrUnsignedSums 는 SHA256SUMS에 맞는 서명이 없거나 믿는 키의 서명이 아니라는 뜻이다.
var ErrUnsignedSums = errors.New("SHA256SUMS 서명을 검증하지 못했습니다")

// VerifySums 는 sig가 sums에 대한, 바이너리에 고정한 공개 키 중 하나의 서명인지 확인한다.
// 릴리스 서명 도구가 서명한 뒤 스스로 확인하는 데 쓴다.
func VerifySums(sums, sig []byte) error { return verifySums(sums, sig, releaseKeys) }

// verifySums 는 keys 중 하나라도 서명을 받아들이면 통과한다. 키를 둘 이상 두는 것은 키를 바꿀 때
// 옛 키로 서명한 릴리스와 새 키로 서명한 릴리스가 함께 있는 기간을 넘기기 위해서다.
func verifySums(sums, sig []byte, keys []ed25519.PublicKey) error {
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: 서명 길이 %d바이트(기대값 %d)", ErrUnsignedSums, len(sig), ed25519.SignatureSize)
	}
	for _, k := range keys {
		if ed25519.Verify(k, sums, sig) {
			return nil
		}
	}
	return fmt.Errorf("%w: 믿는 키 %d개 중 어느 것의 서명도 아닙니다", ErrUnsignedSums, len(keys))
}

// mustParseKeys 는 base64로 적은 32바이트 ed25519 공개 키를 해석한다. 목록은 소스에 고정한 값이라
// 잘못됐으면 빌드한 사람의 실수이므로 패닉으로 바로 드러낸다.
func mustParseKeys(encoded []string) []ed25519.PublicKey {
	keys := make([]ed25519.PublicKey, 0, len(encoded))
	for _, s := range encoded {
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic(fmt.Sprintf("selfupdate: 릴리스 공개 키 %q가 32바이트 ed25519 공개 키의 base64가 아님", s))
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys
}
