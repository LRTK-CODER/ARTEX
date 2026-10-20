//go:build !windows

package server

import (
	"os"
	"syscall"
	"testing"
)

// TestPrepareKeyDirOwnedByServer 는 키 디렉터리를 서버 uid 소유로 만드는지 본다. 에이전트 명령을 다른 uid로
// 돌릴 때(#165) 키를 지키는 것은 0700 권한과 이 소유권이다.
func TestPrepareKeyDirOwnedByServer(t *testing.T) {
	base, data, outside := keyDirLayout(t)
	t.Chdir(outside)
	got, err := PrepareKeyDir("keys", base, data)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat of %s has no owner information", got)
	}
	if int(st.Uid) != os.Getuid() {
		t.Fatalf("key dir owner = %d, want server uid %d", st.Uid, os.Getuid())
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("key dir perm = %o, want 700", perm)
	}
}
