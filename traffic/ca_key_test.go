package traffic

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestOpenRestrictsCAKey 는 기록 프록시의 CA 개인 키 파일이 서버만 읽는 0600이고, 에이전트가 믿을 CA 인증서
// (CACertPath)는 그대로 읽을 수 있는지 본다. 에이전트 명령이 다른 uid로 돌아도(#165) 키로 프록시를 흉내 내지 못하게.
// 이미 0644로 남은 키(예전 버전)도 다시 열 때 줄어야 한다.
func TestOpenRestrictsCAKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are required")
	}
	dir := t.TempDir()
	for round := 0; round < 2; round++ {
		tr, err := Open(dir, "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		key := filepath.Join(dir, "_ca", caKeyFile)
		info, err := os.Stat(key)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("round %d: CA key perm = %o, want 600", round, perm)
		}
		cert, err := os.Stat(tr.CACertPath())
		if err != nil {
			t.Fatal(err)
		}
		if cert.Mode().Perm()&0o044 != 0o044 {
			t.Fatalf("CA cert perm = %o, want readable by group and others", cert.Mode().Perm())
		}
		tr.Close()
		if err := os.Chmod(key, 0o644); err != nil { // 예전 버전이 남긴 권한
			t.Fatal(err)
		}
	}
}
