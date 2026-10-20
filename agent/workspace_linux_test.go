package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureRunDirShared 는 에이전트 uid 분리(#165)를 켰을 때 작업 디렉터리가 에이전트 그룹이 쓸 수 있게 만들어지고,
// 의도 디렉터리를 품은 작업 디렉터리에만 sticky가 붙는지 본다. 끄면 예전처럼 특수 비트 없이 만든다. 테스트 사용자의
// 주 그룹은 그대로라 그룹 변경은 컨테이너 확인에서 본다.
func TestEnsureRunDirShared(t *testing.T) {
	cases := []struct {
		name               string
		share              bool
		wantTask, wantWork fs.FileMode
	}{
		{name: "on", share: true, wantTask: fs.ModeSetgid | fs.ModeSticky | 0o070, wantWork: fs.ModeSetgid | 0o070},
		{name: "off", share: false, wantTask: 0, wantWork: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dir, err := ensureRunDir(base, 7, 3, tc.share)
			if err != nil {
				t.Fatal(err)
			}
			if dir != filepath.Join(base, "tasks", "7", "i3") {
				t.Fatalf("ensureRunDir = %q", dir)
			}
			assertSpecialAndGroup(t, filepath.Join(base, "tasks", "7"), tc.wantTask)
			assertSpecialAndGroup(t, dir, tc.wantWork)
		})
	}
}

// TestEnsureRunDirRefusesPlantedEntry 는 켰을 때 에이전트가 의도 디렉터리 자리에 링크나 파일을 미리 두면, 지우고
// 다시 만들거나 링크를 따라가지 않고 오류를 올리는지 본다. 밖의 디렉터리 권한도 바뀌지 않아야 한다.
func TestEnsureRunDirRefusesPlantedEntry(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, path, outside string)
	}{
		{name: "symbolic link", plant: func(t *testing.T, path, outside string) {
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "regular file", plant: func(t *testing.T, path, _ string) { mustWrite(t, path, "x") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			outside := filepath.Join(base, "keys")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := ensureRunDir(base, 7, 0, true); err != nil {
				t.Fatal(err)
			}
			planted := filepath.Join(base, "tasks", "7", "i4")
			tc.plant(t, planted, outside)
			if _, err := ensureRunDir(base, 7, 4, true); err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("ensureRunDir over a planted %s = %v, want a refusal", tc.name, err)
			}
			if info, err := os.Lstat(planted); err != nil || info.IsDir() {
				t.Fatalf("planted entry was replaced: %v, %v", info, err)
			}
			if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o700 || info.Mode()&fs.ModeSetgid != 0 {
				t.Fatalf("outside dir changed: %v, %v", info.Mode(), err)
			}
		})
	}
}

// TestEnsureSessionDirShared 는 첨부 업로드가 대화 작업 디렉터리를 그룹 쓰기 없이 먼저 만들었어도, 켰을 때는
// 대화를 시작하며 에이전트 그룹이 쓸 수 있게 맞추는지 본다.
func TestEnsureSessionDirShared(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions", "conv-1")
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureSessionDir(dir, true); err != nil {
		t.Fatal(err)
	}
	assertSpecialAndGroup(t, dir, fs.ModeSetgid|0o070)
}

// TestAddGroupWriteDoesNotFollowLinks 는 파일 도구가 쓴 뒤 그룹 쓰기를 더할 때, 그 자리가 링크로 바뀌었거나 밖의
// 파일을 하드 링크로 들여온 것이면 밖의 파일 권한을 바꾸지 않는지 본다.
func TestAddGroupWriteDoesNotFollowLinks(t *testing.T) {
	base := t.TempDir()
	key := filepath.Join(base, "oauth.key")
	mustWrite(t, key, fakeKeyContent)
	symlink := filepath.Join(base, "sym")
	if err := os.Symlink(key, symlink); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(base, "hard")
	if err := os.Link(key, hard); err != nil {
		t.Fatal(err)
	}
	if err := addGroupWrite(symlink); err == nil {
		t.Fatal("addGroupWrite followed a symbolic link")
	}
	if err := addGroupWrite(hard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perm = %o, want 600", perm)
	}
	plain := filepath.Join(base, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := addGroupWrite(plain); err != nil {
		t.Fatal(err)
	}
	assertGroupBits(t, plain, 0o060)
}

// assertSpecialAndGroup 은 디렉터리의 setgid·sticky와 그룹 rwx 비트가 want와 같은지 본다.
func assertSpecialAndGroup(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mask := fs.ModeSetgid | fs.ModeSticky | 0o070
	got := info.Mode() & mask
	if want == 0 {
		// 끈 상태: 특수 비트가 없고 그룹 권한은 umask대로다.
		if got&(fs.ModeSetgid|fs.ModeSticky) != 0 {
			t.Fatalf("%s mode = %v, want no setgid/sticky", path, info.Mode())
		}
		return
	}
	if got != want {
		t.Fatalf("%s mode = %v, want %v of %v", path, info.Mode(), want, mask)
	}
}
