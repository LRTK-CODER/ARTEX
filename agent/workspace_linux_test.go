package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureRunDirLayout 는 에이전트 uid 분리(#165)를 켰을 때와 껐을 때의 작업 디렉터리·큰 출력 위치와 권한을 본다.
// 켜면 tasks/<tid>는 서버만 쓰고(에이전트 그룹은 읽고 지나가기만, sticky 없음) planner·mainagent는 그 아래 work,
// worker는 i<iid>에서 그룹 쓰기로 일하며, 큰 출력은 서버만 쓰는 tasks/<tid>/cmd-output/<이름>이다. 끄면 예전
// 배치 그대로 특수 비트 없이 만든다. 테스트 사용자의 주 그룹은 그대로라 그룹 변경은 컨테이너 확인에서 본다.
func TestEnsureRunDirLayout(t *testing.T) {
	const (
		serverOnly = fs.ModeSetgid | 0o050
		shared     = fs.ModeSetgid | 0o070
	)
	cases := []struct {
		name              string
		intentID          int64
		share             bool
		wantWork, wantOut string // tasks/7 기준
		wantModes         map[string]fs.FileMode
	}{
		{name: "planner off", intentID: 0, wantWork: ".", wantOut: "cmd-output", wantModes: map[string]fs.FileMode{".": 0}},
		{name: "worker off", intentID: 3, wantWork: "i3", wantOut: "i3/cmd-output", wantModes: map[string]fs.FileMode{".": 0, "i3": 0}},
		{
			name: "planner on", intentID: 0, share: true, wantWork: "work", wantOut: "cmd-output/work",
			wantModes: map[string]fs.FileMode{".": serverOnly, "work": shared, "cmd-output": serverOnly, "cmd-output/work": serverOnly},
		},
		{
			name: "worker on", intentID: 3, share: true, wantWork: "i3", wantOut: "cmd-output/i3",
			wantModes: map[string]fs.FileMode{".": serverOnly, "i3": shared, "cmd-output": serverOnly, "cmd-output/i3": serverOnly},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			taskDir := filepath.Join(base, "tasks", "7")
			dirs, err := ensureRunDir(base, 7, tc.intentID, tc.share)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(taskDir, filepath.FromSlash(tc.wantWork)); dirs.work != want {
				t.Errorf("work = %q, want %q", dirs.work, want)
			}
			if want := filepath.Join(taskDir, filepath.FromSlash(tc.wantOut)); dirs.toolOutput != want {
				t.Errorf("toolOutput = %q, want %q", dirs.toolOutput, want)
			}
			for rel, want := range tc.wantModes {
				assertSpecialAndGroup(t, filepath.Join(taskDir, filepath.FromSlash(rel)), want)
			}
		})
	}
}

// TestEnsureRunDirRevertsStickyTaskDir 는 켰을 때 예전 배치(그룹 쓰기·sticky)로 만들어진 tasks/<tid>를 서버만 쓰는
// 디렉터리로 되돌리는지 본다. sticky가 남으면 fs.protected_regular=2인 호스트에서 서버가 에이전트 파일을 다시 쓰지
// 못하고, 그룹 쓰기가 남으면 에이전트가 의도 디렉터리를 바꿔치기할 수 있다.
func TestEnsureRunDirRevertsStickyTaskDir(t *testing.T) {
	base := t.TempDir()
	taskDir := filepath.Join(base, "tasks", "7")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(taskDir, fs.ModeSetgid|fs.ModeSticky|0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureRunDir(base, 7, 1, true); err != nil {
		t.Fatal(err)
	}
	assertSpecialAndGroup(t, taskDir, fs.ModeSetgid|0o050)
}

// TestEnsureRunDirRefusesPlantedEntry 는 켰을 때 작업 디렉터리나 큰 출력 위치 자리에 링크나 파일이 미리 있으면,
// 지우고 다시 만들거나 링크를 따라가지 않고 오류를 올리는지 본다. 밖의 디렉터리 권한도 바뀌지 않아야 한다.
func TestEnsureRunDirRefusesPlantedEntry(t *testing.T) {
	plants := []struct {
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
	spots := []string{"i4", "cmd-output", "cmd-output/i4"}
	for _, p := range plants {
		for _, spot := range spots {
			t.Run(p.name+" at "+spot, func(t *testing.T) {
				base := t.TempDir()
				outside := filepath.Join(base, "keys")
				if err := os.Mkdir(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				taskDir := filepath.Join(base, "tasks", "7")
				if err := os.MkdirAll(filepath.Join(taskDir, "cmd-output"), 0o755); err != nil {
					t.Fatal(err)
				}
				planted := filepath.Join(taskDir, filepath.FromSlash(spot))
				if spot == "cmd-output" {
					if err := os.Remove(planted); err != nil {
						t.Fatal(err)
					}
				}
				p.plant(t, planted, outside)
				if _, err := ensureRunDir(base, 7, 4, true); err == nil || !strings.Contains(err.Error(), "refusing") {
					t.Fatalf("ensureRunDir over a planted %s = %v, want a refusal", p.name, err)
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
}

// TestEnsureSessionDirShared 는 켰을 때 대화 작업 디렉터리를 에이전트 그룹이 쓸 수 있게 맞추고(첨부 업로드가 그룹
// 쓰기 없이 먼저 만들었어도), 큰 출력은 서버만 쓰는 sessions/cmd-output/<sid>에 두는지 본다. 끄면 예전처럼 작업
// 디렉터리 안의 cmd-output이다.
func TestEnsureSessionDirShared(t *testing.T) {
	cases := []struct {
		name    string
		share   bool
		wantOut string // sessions 기준
	}{
		{name: "on", share: true, wantOut: "cmd-output/conv-1"},
		{name: "off", share: false, wantOut: "conv-1/cmd-output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := filepath.Join(t.TempDir(), "sessions")
			dir := filepath.Join(sessions, "conv-1")
			if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o755); err != nil {
				t.Fatal(err)
			}
			dirs, err := ensureSessionDir(dir, tc.share)
			if err != nil {
				t.Fatal(err)
			}
			if dirs.work != dir {
				t.Errorf("work = %q, want %q", dirs.work, dir)
			}
			if want := filepath.Join(sessions, filepath.FromSlash(tc.wantOut)); dirs.toolOutput != want {
				t.Errorf("toolOutput = %q, want %q", dirs.toolOutput, want)
			}
			if !tc.share {
				return
			}
			assertSpecialAndGroup(t, dir, fs.ModeSetgid|0o070)
			assertSpecialAndGroup(t, filepath.Join(sessions, "cmd-output"), fs.ModeSetgid|0o050)
			assertSpecialAndGroup(t, dirs.toolOutput, fs.ModeSetgid|0o050)
		})
	}
}

// TestAddGroupWriteSkipsHardLinks 는 파일 도구가 쓴 뒤 그룹 쓰기를 더할 때, 밖의 파일을 하드 링크로 들여온 것이면
// 밖의 파일 권한을 바꾸지 않고, 보통 파일에는 그룹 읽기·쓰기를 더하는지 본다.
func TestAddGroupWriteSkipsHardLinks(t *testing.T) {
	base := t.TempDir()
	key := filepath.Join(base, "oauth.key")
	mustWrite(t, key, fakeKeyContent)
	hard := filepath.Join(base, "hard")
	if err := os.Link(key, hard); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(base, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{hard, plain} {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		err = addGroupWrite(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perm = %o, want 600", perm)
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
