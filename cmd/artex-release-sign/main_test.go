package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// TestSignMatchesOpenSSL 은 이 도구의 서명과 `openssl pkeyutl -sign -rawin`의 서명이 같은 바이트인지 본다.
// ed25519 서명은 결정적이라 같은 키·같은 입력이면 같아야 한다. 키는 테스트 안에서만 만든다.
func TestSignMatchesOpenSSL(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl이 없어 건너뛴다")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "release.key")
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte("0123  artex-1.0.0-linux-amd64.zip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"keygen", "-out", key}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if err := run([]string{"keygen", "-out", key}); err == nil {
		t.Fatal("이미 있는 개인 키를 덮어쓰면 안 된다")
	}
	assertMode(t, key, 0o600)

	// 테스트 키는 운영 공개 키 목록에 없으므로 서명은 쓰되 검증 실패를 알려야 한다.
	if err := run([]string{"sign", "-key", key, sums}); !errors.Is(err, selfupdate.ErrUnsignedSums) {
		t.Fatalf("sign = %v, want 서명 파일을 쓰고 ErrUnsignedSums", err)
	}
	ours, err := os.ReadFile(sums + ".sig")
	if err != nil {
		t.Fatal(err)
	}

	theirs := filepath.Join(dir, "openssl.sig")
	openssl(t, "pkeyutl", "-sign", "-inkey", key, "-rawin", "-in", sums, "-out", theirs)
	if got, _ := os.ReadFile(theirs); !bytes.Equal(got, ours) {
		t.Fatalf("openssl 서명이 다름\nopenssl: %x\n도구:    %x", got, ours)
	}

	// 반대로 openssl로 만든 키도 이 도구가 읽는다.
	osslKey := filepath.Join(dir, "openssl.key")
	openssl(t, "genpkey", "-algorithm", "ed25519", "-out", osslKey)
	if err := run([]string{"sign", "-key", osslKey, sums}); !errors.Is(err, selfupdate.ErrUnsignedSums) {
		t.Fatalf("openssl 키로 sign = %v", err)
	}
	// README의 공개 키 꺼내기(`openssl pkey -pubout -outform DER`의 마지막 32바이트)가 맞는 키를 주는지 본다.
	der := filepath.Join(dir, "pub.der")
	openssl(t, "pkey", "-in", osslKey, "-pubout", "-outform", "DER", "-out", der)
	pubDER, err := os.ReadFile(der)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(sums + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	sumsBytes, err := os.ReadFile(sums)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubDER[len(pubDER)-ed25519.PublicKeySize:]), sumsBytes, sig) {
		t.Fatal("DER 공개 키의 마지막 32바이트로 서명이 검증되지 않는다")
	}
}

func openssl(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "openssl", args...).CombinedOutput(); err != nil {
		t.Fatalf("openssl %v: %v\n%s", args, err, out)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s 권한 = %o, want %o", path, got, want)
	}
}
