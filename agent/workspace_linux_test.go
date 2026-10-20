package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// testAgentUID 는 작업 공간 테스트가 ACL에 넣는 에이전트 uid다. 테스트는 그 uid가 되지 않으므로 아무 값이나 된다.
const testAgentUID = 20001

// requireACLTempDir 는 t.TempDir()의 파일 시스템이 POSIX ACL을 지원하지 않으면 건너뛴다.
func requireACLTempDir(t *testing.T) {
	t.Helper()
	openACLTestDir(t)
}

// wantACL 은 path의 kind ACL이 want와 같은지 본다. want가 nil이면 그 종류의 ACL이 없어야 한다.
func wantACL(t *testing.T, path string, kind ACLKind, want ACL) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := GetACL(f, kind)
	if want == nil {
		if !errors.Is(err, ErrNoACL) {
			t.Fatalf("%s %s = %v, %v; want none", path, kind, got, err)
		}
		return
	}
	sorted, cerr := want.canonical()
	if cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || !slices.Equal(got, sorted) {
		t.Fatalf("%s %s = %v, %v; want %v", path, kind, got, err, sorted)
	}
}

// assertMode 는 path의 권한 비트와 특수 비트가 want인지 본다. ACL이 있으면 그룹 자리는 mask다.
func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (fs.ModePerm | specialModeBits); got != want {
		t.Fatalf("%s mode = %v, want %v", path, got, want)
	}
}

// TestEnsureRunDirLayout 은 에이전트 uid 분리(#165)를 켰을 때와 껐을 때의 작업 디렉터리·큰 출력 위치와 권한을 본다
// (#182). 켜면 모두 서버 소유이고 소유 그룹·other 권한이 없으며, 작업 uid만 ACL로 받는다. tasks/<tid>와 큰 출력은
// 읽기, planner·mainagent의 work와 worker의 i<iid>는 읽기·쓰기다. 쓰기 영역의 기본 ACL에는 서버 uid가 있어 에이전트가
// 만든 항목도 서버가 읽고 지운다. tasks/는 모든 uid가 지나가기만 한다. 끄면 예전 배치 그대로 ACL 없이 만든다.
func TestEnsureRunDirLayout(t *testing.T) {
	requireACLTempDir(t)
	server := uint32(os.Getuid())
	rx, rwx := ACLRead|ACLExecute, aclPermAll
	readAccess := ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rx}, {Tag: ACLOther}}
	writeAccess := ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rwx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rwx}, {Tag: ACLOther}}
	writeDefault := append(slices.Clone(writeAccess), ACLEntry{Tag: ACLNamedUser, ID: server, Perm: rwx})
	cases := []struct {
		name              string
		intentID          int64
		agentUID          int
		wantWork, wantOut string // tasks/7 기준
		readDirs          []string
	}{
		{name: "planner off", intentID: 0, wantWork: ".", wantOut: "cmd-output"},
		{name: "worker off", intentID: 3, wantWork: "i3", wantOut: "i3/cmd-output"},
		{name: "planner on", intentID: 0, agentUID: testAgentUID, wantWork: "work", wantOut: "cmd-output/work", readDirs: []string{".", "cmd-output", "cmd-output/work"}},
		{name: "worker on", intentID: 3, agentUID: testAgentUID, wantWork: "i3", wantOut: "cmd-output/i3", readDirs: []string{".", "cmd-output", "cmd-output/i3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			taskDir := filepath.Join(base, "tasks", "7")
			dirs, err := ensureRunDir(base, 7, tc.intentID, tc.agentUID)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(taskDir, filepath.FromSlash(tc.wantWork)); dirs.work != want {
				t.Errorf("work = %q, want %q", dirs.work, want)
			}
			if want := filepath.Join(taskDir, filepath.FromSlash(tc.wantOut)); dirs.toolOutput != want {
				t.Errorf("toolOutput = %q, want %q", dirs.toolOutput, want)
			}
			if tc.agentUID == 0 {
				wantACL(t, dirs.work, ACLAccess, nil)
				wantACL(t, dirs.work, ACLDefault, nil)
				return
			}
			assertMode(t, filepath.Join(base, "tasks"), 0o711)
			for _, rel := range tc.readDirs {
				p := filepath.Join(taskDir, filepath.FromSlash(rel))
				assertMode(t, p, 0o750)
				wantACL(t, p, ACLAccess, readAccess)
				wantACL(t, p, ACLDefault, readAccess)
			}
			assertMode(t, dirs.work, 0o770)
			wantACL(t, dirs.work, ACLAccess, writeAccess)
			wantACL(t, dirs.work, ACLDefault, writeDefault)
			if _, err := os.Stat(filepath.Join(taskDir, UploadsDirName)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("ensureRunDir created uploads: %v", err)
			}

			// 서버가 작업 공간에 만든 파일은 기본 ACL을 물려받아 작업 uid와 서버 uid 항목을 갖고, other는 없다.
			made := filepath.Join(dirs.work, "made.txt")
			if err := os.WriteFile(made, []byte("x"), 0o666); err != nil {
				t.Fatal(err)
			}
			assertMode(t, made, 0o660)
			wantACL(t, made, ACLAccess, ACL{{Tag: ACLUserOwner, Perm: ACLRead | ACLWrite}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rwx},
				{Tag: ACLNamedUser, ID: server, Perm: rwx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: ACLRead | ACLWrite}, {Tag: ACLOther}})
		})
	}
}

// TestEnsureRunDirSharesExistingUploads 는 작업 uid를 받기 전에 올라온 첨부(tasks/<tid>/uploads)에 실행할 때 읽기를
// 주는지, 그 뒤 올라온 첨부가 읽기를 물려받는지 본다.
func TestEnsureRunDirSharesExistingUploads(t *testing.T) {
	requireACLTempDir(t)
	base := t.TempDir()
	uploads := filepath.Join(base, "tasks", "7", UploadsDirName)
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureRunDir(base, 7, 1, testAgentUID); err != nil {
		t.Fatal(err)
	}
	rx := ACLRead | ACLExecute
	read := ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rx}, {Tag: ACLOther}}
	assertMode(t, uploads, 0o750)
	wantACL(t, uploads, ACLAccess, read)
	wantACL(t, uploads, ACLDefault, read)
	later := filepath.Join(uploads, "later.txt")
	if err := os.WriteFile(later, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertMode(t, later, 0o640)
}

// TestEnsureRunDirClearsLegacyModes 는 켰을 때 예전 공용 그룹 배치(#175: setgid, 그룹 쓰기, sticky)로 만들어진
// tasks/<tid>를 서버 소유 디렉터리로 되돌리는지 본다. 그룹·other 권한이 남으면 다른 작업 uid가 들어온다.
func TestEnsureRunDirClearsLegacyModes(t *testing.T) {
	requireACLTempDir(t)
	base := t.TempDir()
	taskDir := filepath.Join(base, "tasks", "7")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(taskDir, fs.ModeSetgid|fs.ModeSticky|0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(base, "tasks"), fs.ModeSetgid|0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureRunDir(base, 7, 1, testAgentUID); err != nil {
		t.Fatal(err)
	}
	assertMode(t, taskDir, 0o750)
	assertMode(t, filepath.Join(base, "tasks"), 0o711)
}

// TestEnsureRunDirRefusesPlantedEntry 는 켰을 때 작업 디렉터리나 큰 출력 위치 자리에 링크나 파일이 미리 있으면,
// 지우고 다시 만들거나 링크를 따라가지 않고 오류를 올리는지 본다. 밖의 디렉터리 권한·ACL도 바뀌지 않아야 한다.
func TestEnsureRunDirRefusesPlantedEntry(t *testing.T) {
	requireACLTempDir(t)
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
	spots := []string{"i4", "cmd-output", "cmd-output/i4", "uploads"}
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
				if _, err := ensureRunDir(base, 7, 4, testAgentUID); err == nil || !strings.Contains(err.Error(), "refusing") {
					t.Fatalf("ensureRunDir over a planted %s = %v, want a refusal", p.name, err)
				}
				if info, err := os.Lstat(planted); err != nil || info.IsDir() {
					t.Fatalf("planted entry was replaced: %v, %v", info, err)
				}
				assertMode(t, outside, 0o700)
				wantACL(t, outside, ACLAccess, nil)
			})
		}
	}
}

// TestEnsureSessionDirShared 는 켰을 때 대화 작업 디렉터리를 세션 uid에만 읽기·쓰기로 열고(첨부 업로드가 먼저
// 만들었어도), 큰 출력은 sessions/cmd-output/<sid>에 세션 uid 읽기로 두는지 본다. sessions/와 sessions/cmd-output은
// 모든 uid가 지나가기만 한다. 끄면 예전처럼 작업 디렉터리 안의 cmd-output이다.
func TestEnsureSessionDirShared(t *testing.T) {
	requireACLTempDir(t)
	cases := []struct {
		name     string
		agentUID int
		wantOut  string // sessions 기준
	}{
		{name: "on", agentUID: testAgentUID, wantOut: "cmd-output/conv-1"},
		{name: "off", wantOut: "conv-1/cmd-output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := filepath.Join(t.TempDir(), "sessions")
			dir := filepath.Join(sessions, "conv-1")
			if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o755); err != nil {
				t.Fatal(err)
			}
			dirs, err := ensureSessionDir(dir, tc.agentUID)
			if err != nil {
				t.Fatal(err)
			}
			if dirs.work != dir {
				t.Errorf("work = %q, want %q", dirs.work, dir)
			}
			if want := filepath.Join(sessions, filepath.FromSlash(tc.wantOut)); dirs.toolOutput != want {
				t.Errorf("toolOutput = %q, want %q", dirs.toolOutput, want)
			}
			if tc.agentUID == 0 {
				return
			}
			rx, rwx := ACLRead|ACLExecute, aclPermAll
			assertMode(t, sessions, 0o711)
			assertMode(t, filepath.Join(sessions, "cmd-output"), 0o711)
			assertMode(t, dir, 0o770)
			wantACL(t, dir, ACLAccess, ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rwx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rwx}, {Tag: ACLOther}})
			assertMode(t, dirs.toolOutput, 0o750)
			wantACL(t, dirs.toolOutput, ACLAccess, ACL{{Tag: ACLUserOwner, Perm: rwx}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: rx}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: rx}, {Tag: ACLOther}})
		})
	}
}

// TestShareOpened 는 서버 소유 파일·디렉터리를 규칙대로 맞추고(특수 비트 빼기, 소유 그룹·other 권한 없애기), 밖의
// 서버 파일을 하드 링크로 들여온 것과 FIFO는 건드리지 않는지 본다. 서버 전용 디렉터리는 이름 있는 항목을 지우고
// 기본 ACL은 그대로 둔다.
func TestShareOpened(t *testing.T) {
	requireACLTempDir(t)
	base := t.TempDir()
	open := func(t *testing.T, path string) (*os.File, fs.FileInfo) {
		t.Helper()
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		f := os.NewFile(uintptr(fd), path)
		t.Cleanup(func() { f.Close() })
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return f, info
	}
	rw := ACLRead | ACLWrite

	t.Run("setuid file in a write area", func(t *testing.T) {
		p := filepath.Join(base, "tool")
		writeMode(t, p, fs.ModeSetuid|0o755)
		f, info := open(t, p)
		if err := ShareOpened(f, info, testAgentUID, WorkspaceWrite); err != nil {
			t.Fatal(err)
		}
		assertMode(t, p, 0o770)
		wantACL(t, p, ACLAccess, ACL{{Tag: ACLUserOwner, Perm: aclPermAll}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: aclPermAll}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: aclPermAll}, {Tag: ACLOther}})
	})
	t.Run("file in a read area", func(t *testing.T) {
		p := filepath.Join(base, "upload.txt")
		writeMode(t, p, 0o666)
		f, info := open(t, p)
		if err := ShareOpened(f, info, testAgentUID, WorkspaceRead); err != nil {
			t.Fatal(err)
		}
		assertMode(t, p, 0o640)
		wantACL(t, p, ACLAccess, ACL{{Tag: ACLUserOwner, Perm: rw}, {Tag: ACLNamedUser, ID: testAgentUID, Perm: ACLRead}, {Tag: ACLGroupOwner}, {Tag: ACLMask, Perm: ACLRead}, {Tag: ACLOther}})
	})
	t.Run("server-only file", func(t *testing.T) {
		p := filepath.Join(base, "stray.txt")
		writeMode(t, p, 0o666)
		f, info := open(t, p)
		if err := ShareOpened(f, info, testAgentUID, WorkspaceServerOnly); err != nil {
			t.Fatal(err)
		}
		assertMode(t, p, 0o600)
		wantACL(t, p, ACLAccess, nil)
	})
	t.Run("server-only dir drops the agent entry", func(t *testing.T) {
		p := filepath.Join(base, "conv-9")
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := ShareDir(p, testAgentUID, WorkspaceWrite); err != nil {
			t.Fatal(err)
		}
		if err := ShareDir(p, 0, WorkspaceServerOnly); err != nil {
			t.Fatal(err)
		}
		assertMode(t, p, 0o700)
		wantACL(t, p, ACLAccess, nil)
	})
	t.Run("hard link and fifo are left alone", func(t *testing.T) {
		key := filepath.Join(base, "oauth.key")
		writeMode(t, key, 0o640)
		hard := filepath.Join(base, "hard.key")
		if err := os.Link(key, hard); err != nil {
			t.Fatal(err)
		}
		fifo := filepath.Join(base, "fifo")
		if err := syscall.Mkfifo(fifo, 0o666); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{hard, fifo} {
			f, info := open(t, p)
			if err := ShareOpened(f, info, testAgentUID, WorkspaceWrite); err != nil {
				t.Fatal(err)
			}
		}
		assertMode(t, key, 0o640)
		wantACL(t, key, ACLAccess, nil)
		// FIFO는 os.Open이 쓰는 쪽을 기다리며 멈추므로 ACL 대신 모드로 본다(ACL을 쓰면 그룹 자리가 mask로 바뀐다).
		if info, err := os.Lstat(fifo); err != nil || info.Mode().Perm()&0o077 != 0o066&^umask(t) {
			t.Fatalf("fifo mode changed: %v, %v", info.Mode(), err)
		}
	})
	t.Run("missing dir", func(t *testing.T) {
		if err := ShareDir(filepath.Join(base, "missing"), testAgentUID, WorkspaceRead); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ShareDir(missing) = %v, want fs.ErrNotExist", err)
		}
	})
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

// umask 는 지금 프로세스의 umask다(읽으려면 바꿨다가 되돌려야 한다).
func umask(t *testing.T) fs.FileMode {
	t.Helper()
	old := syscall.Umask(0o022)
	syscall.Umask(old)
	return fs.FileMode(old)
}

func writeMode(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
