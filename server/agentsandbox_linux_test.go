package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
)

// aclTagOrder 는 커널이 ACL 항목을 저장하는 순서다(종류, 같은 종류 안에서는 id 오름차순).
var aclTagOrder = map[agent.ACLTag]int{agent.ACLUserOwner: 0, agent.ACLNamedUser: 1, agent.ACLGroupOwner: 2, agent.ACLNamedGroup: 3, agent.ACLMask: 4, agent.ACLOther: 5}

// agentACL 은 서버 소유 항목에서 에이전트 uid가 perm을 받는 ACL을 커널 순서로 만든다. extraUsers는 더 넣을 이름 있는
// 사용자 항목(rwx)이다.
func agentACL(ownerPerm agent.ACLPerm, uid int, perm agent.ACLPerm, extraUsers ...int) agent.ACL {
	acl := agent.ACL{{Tag: agent.ACLUserOwner, Perm: ownerPerm}, {Tag: agent.ACLNamedUser, ID: uint32(uid), Perm: perm},
		{Tag: agent.ACLGroupOwner}, {Tag: agent.ACLMask, Perm: perm}, {Tag: agent.ACLOther}}
	for _, u := range extraUsers {
		acl = append(acl, agent.ACLEntry{Tag: agent.ACLNamedUser, ID: uint32(u), Perm: agent.ACLRead | agent.ACLWrite | agent.ACLExecute})
	}
	slices.SortFunc(acl, func(x, y agent.ACLEntry) int {
		if c := aclTagOrder[x.Tag] - aclTagOrder[y.Tag]; c != 0 {
			return c
		}
		return int(x.ID) - int(y.ID)
	})
	return acl
}

// readACLOf 는 path의 kind ACL을 읽는다. 없으면 nil이다.
func readACLOf(t *testing.T, path string, kind agent.ACLKind) agent.ACL {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	acl, err := agent.GetACL(f, kind)
	if errors.Is(err, agent.ErrNoACL) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return acl
}

// skipWithoutACL 은 t.TempDir()의 파일 시스템이 POSIX ACL을 지원하지 않으면 건너뛴다.
func skipWithoutACL(t *testing.T) {
	t.Helper()
	if err := checkWorkspaceACL(filepath.Join(t.TempDir(), "acl-probe"), 20000, agent.SetACL); errors.Is(err, syscall.ENOTSUP) {
		t.Skipf("temp dir file system does not support POSIX ACLs: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
}

// TestApplyWorkspaceACL 은 시작할 때 할당 표대로 작업 공간 ACL을 다시 거는지 본다(#182). 데이터 디렉터리와
// tasks·sessions는 모든 uid가 지나가기만 하고, drafts는 서버 전용이다. 할당이 있는 작업·대화의 서버 소유 항목은
// 그 uid에만 열리고(작업 공간은 읽기·쓰기, 그 밖은 읽기), 할당이 없는 것과 모르는 이름은 서버 전용으로 닫히며 안으로
// 들어가지 않는다. 예전 공용 그룹 배치(#175)의 setgid·sticky·그룹 쓰기는 빠진다. 밖을 가리키는 링크를 따라가지
// 않고, 밖의 서버 파일을 하드 링크로 들여온 것도 바꾸지 않는다. 데이터 디렉터리 경로가 링크여도 된다.
func TestApplyWorkspaceACL(t *testing.T) {
	skipWithoutACL(t)
	const taskUID, convUID = 20001, 20002
	server := os.Getuid()
	rwx := agent.ACLRead | agent.ACLWrite | agent.ACLExecute
	rw, rx, r := agent.ACLRead|agent.ACLWrite, agent.ACLRead|agent.ACLExecute, agent.ACLRead
	base := t.TempDir()
	realData := filepath.Join(base, "data")
	dataDir := filepath.Join(base, "data-link")
	outsideDir := filepath.Join(base, "outside")
	outside := filepath.Join(outsideDir, "oauth.key")
	mkdirWithMode(t, outsideDir, 0o700)
	writeFileWithMode(t, outside, 0o600)
	mkdirWithMode(t, realData, 0o777)
	if err := os.Symlink(realData, dataDir); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(realData, "tasks", "7")
	mkdirWithMode(t, filepath.Join(task, "i3", "sub"), 0o777)
	writeFileWithMode(t, filepath.Join(task, "i3", "sub", "deep.txt"), 0o666)
	writeFileWithMode(t, filepath.Join(task, "i3", "tool"), fs.ModeSetuid|0o755)
	mkdirWithMode(t, filepath.Join(task, "work"), fs.ModeSetgid|0o775)
	writeFileWithMode(t, filepath.Join(task, "work", "notes.md"), 0o664)
	writeFileWithMode(t, filepath.Join(task, "plan.md"), 0o664)
	mkdirWithMode(t, filepath.Join(task, "uploads"), 0o775)
	writeFileWithMode(t, filepath.Join(task, "uploads", "a.txt"), 0o664)
	mkdirWithMode(t, filepath.Join(task, "cmd-output", "i3"), 0o777)
	writeFileWithMode(t, filepath.Join(task, "cmd-output", "i3", "output-1-1.txt"), 0o666)
	mkdirWithMode(t, filepath.Join(task, "home"), 0o700)
	mkdirWithMode(t, filepath.Join(task, "tmp"), 0o700)
	writeFileWithMode(t, filepath.Join(task, "tmp", "scratch"), 0o600)
	if err := os.Chmod(task, fs.ModeSetgid|fs.ModeSticky|0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(task, "i3", "key-link")); err != nil {
		t.Fatal(err)
	}
	hardLinked := filepath.Join(base, "server.key")
	writeFileWithMode(t, hardLinked, 0o640)
	if err := os.Link(hardLinked, filepath.Join(task, "i3", "hard.key")); err != nil {
		t.Fatal(err)
	}
	mkdirWithMode(t, filepath.Join(realData, "tasks", "9"), 0o755)
	writeFileWithMode(t, filepath.Join(realData, "tasks", "9", "secret.txt"), 0o644)
	if err := os.Symlink(outsideDir, filepath.Join(realData, "tasks", "8")); err != nil {
		t.Fatal(err)
	}
	writeFileWithMode(t, filepath.Join(realData, "tasks", "stray.txt"), 0o666)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "conv-1", "uploads"), 0o775)
	writeFileWithMode(t, filepath.Join(realData, "sessions", "conv-1", "uploads", "f.txt"), 0o644)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "cmd-output", "conv-1"), 0o777)
	writeFileWithMode(t, filepath.Join(realData, "sessions", "cmd-output", "conv-1", "out.txt"), 0o666)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "cmd-output", "conv-5"), 0o777)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "conv-5"), 0o775)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "home"), 0o777)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "tmp"), 0o777)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "home", "conv-1"), 0o700)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "tmp", "conv-1"), 0o700)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "tmp", "conv-5"), 0o700)
	mkdirWithMode(t, filepath.Join(realData, "sessions", "tool-test-1"), 0o770)
	mkdirWithMode(t, filepath.Join(realData, "drafts", "d1", "uploads"), 0o777)
	writeFileWithMode(t, filepath.Join(realData, "traffic.db"), 0o644)
	if err := os.Chmod(filepath.Join(realData, "tasks"), fs.ModeSetgid|0o777); err != nil {
		t.Fatal(err)
	}

	if err := prepareWorkspaceRoots(dataDir); err != nil {
		t.Fatal(err)
	}
	owners := workspaceOwners{tasks: map[int64]int{7: taskUID}, conversations: map[int64]int{1: convUID}}
	if err := applyWorkspaceACL(dataDir, owners); err != nil {
		t.Fatal(err)
	}

	readDir := agentACL(rwx, taskUID, rx)
	writeDir := agentACL(rwx, taskUID, rwx)
	writeDefault := agentACL(rwx, taskUID, rwx, server)
	convWrite := agentACL(rwx, convUID, rwx)
	cases := []struct {
		rel         string // realData 기준. 비면 realData
		mode        fs.FileMode
		access, def agent.ACL
	}{
		{rel: "", mode: 0o711},
		{rel: "tasks", mode: 0o711},
		{rel: "sessions", mode: 0o711},
		{rel: "drafts", mode: 0o700},
		{rel: "drafts/d1", mode: 0o755},
		{rel: "traffic.db", mode: 0o644},
		{rel: "tasks/stray.txt", mode: 0o600},
		{rel: "tasks/9", mode: 0o700},
		{rel: "tasks/9/secret.txt", mode: 0o644},
		{rel: "tasks/7", mode: 0o750, access: readDir, def: readDir},
		{rel: "tasks/7/plan.md", mode: 0o640, access: agentACL(rw, taskUID, r)},
		{rel: "tasks/7/work", mode: 0o770, access: writeDir, def: writeDefault},
		{rel: "tasks/7/work/notes.md", mode: 0o660, access: agentACL(rw, taskUID, rw)},
		{rel: "tasks/7/i3", mode: 0o770, access: writeDir, def: writeDefault},
		{rel: "tasks/7/i3/sub", mode: 0o770, access: writeDir, def: writeDefault},
		{rel: "tasks/7/i3/sub/deep.txt", mode: 0o660, access: agentACL(rw, taskUID, rw)},
		{rel: "tasks/7/i3/tool", mode: 0o770, access: agentACL(rwx, taskUID, rwx)},
		{rel: "tasks/7/uploads", mode: 0o750, access: readDir, def: readDir},
		{rel: "tasks/7/uploads/a.txt", mode: 0o640, access: agentACL(rw, taskUID, r)},
		{rel: "tasks/7/cmd-output", mode: 0o750, access: readDir, def: readDir},
		{rel: "tasks/7/cmd-output/i3", mode: 0o750, access: readDir, def: readDir},
		{rel: "tasks/7/cmd-output/i3/output-1-1.txt", mode: 0o640, access: agentACL(rw, taskUID, r)},
		{rel: "tasks/7/home", mode: 0o770, access: writeDir, def: writeDefault},
		{rel: "tasks/7/tmp", mode: 0o770, access: writeDir, def: writeDefault},
		{rel: "tasks/7/tmp/scratch", mode: 0o660, access: agentACL(rw, taskUID, rw)},
		{rel: "sessions/conv-1", mode: 0o770, access: convWrite, def: agentACL(rwx, convUID, rwx, server)},
		{rel: "sessions/conv-1/uploads", mode: 0o770, access: convWrite, def: agentACL(rwx, convUID, rwx, server)},
		{rel: "sessions/conv-1/uploads/f.txt", mode: 0o660, access: agentACL(rw, convUID, rw)},
		{rel: "sessions/cmd-output", mode: 0o711},
		{rel: "sessions/cmd-output/conv-1", mode: 0o750, access: agentACL(rwx, convUID, rx), def: agentACL(rwx, convUID, rx)},
		{rel: "sessions/cmd-output/conv-1/out.txt", mode: 0o640, access: agentACL(rw, convUID, r)},
		{rel: "sessions/cmd-output/conv-5", mode: 0o700},
		{rel: "sessions/home", mode: 0o711},
		{rel: "sessions/home/conv-1", mode: 0o770, access: convWrite, def: agentACL(rwx, convUID, rwx, server)},
		{rel: "sessions/tmp", mode: 0o711},
		{rel: "sessions/tmp/conv-1", mode: 0o770, access: convWrite, def: agentACL(rwx, convUID, rwx, server)},
		{rel: "sessions/tmp/conv-5", mode: 0o700},
		{rel: "sessions/conv-5", mode: 0o700},
		{rel: "sessions/tool-test-1", mode: 0o700},
	}
	for _, tc := range cases {
		p := filepath.Join(realData, filepath.FromSlash(tc.rel))
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != tc.mode {
			t.Errorf("%s mode = %v, want %v", tc.rel, got, tc.mode)
		}
		if got := readACLOf(t, p, agent.ACLAccess); !slices.Equal(got, tc.access) {
			t.Errorf("%s access ACL = %v, want %v", tc.rel, got, tc.access)
		}
		if info.IsDir() {
			if got := readACLOf(t, p, agent.ACLDefault); !slices.Equal(got, tc.def) {
				t.Errorf("%s default ACL = %v, want %v", tc.rel, got, tc.def)
			}
		}
	}
	for _, p := range []string{outsideDir, outside, hardLinked} {
		if acl := readACLOf(t, p, agent.ACLAccess); acl != nil {
			t.Errorf("%s outside the workspace got an ACL %v", p, acl)
		}
	}
	for p, want := range map[string]fs.FileMode{outsideDir: 0o700, outside: 0o600, hardLinked: 0o640} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s outside the workspace changed: %v, %v", p, info.Mode(), err)
		}
	}
}

// TestInstallShellDir 는 셸 대체 디렉터리가 없으면 서버 전용(0750)으로 만들고, useShellPath가 서버 PATH를 그
// 디렉터리 하나로 바꾸고 원래 PATH를 AgentPathEnv에 두는지 본다. norma처럼 이름 "bash"로 찾으면 도우미를
// 가리키는 링크가 나와야 한다.
func TestInstallShellDir(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv(AgentPathEnv, "")
	helper := writeFakeHelper(t, `exit 0`)
	dir, err := installShellDir(filepath.Join(t.TempDir(), SandboxShellDirName), helper)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o750 {
		t.Fatalf("shell dir perm = %o, want 750", perm)
	}
	// 이미 있는 디렉터리는 확인만 하고 그대로 쓴다(다시 시작, Docker 이미지에 미리 든 것).
	if again, err := installShellDir(dir, helper); err != nil || again != dir {
		t.Fatalf("installShellDir on an existing dir = %q, %v", again, err)
	}
	agentPath, err := useShellPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH"); got != dir {
		t.Fatalf("PATH = %q, want only the shell dir %q", got, dir)
	}
	if agentPath != "/usr/bin:/bin" || os.Getenv(AgentPathEnv) != "/usr/bin:/bin" {
		t.Fatalf("agent PATH = %q, %s = %q, want the original PATH", agentPath, AgentPathEnv, os.Getenv(AgentPathEnv))
	}
	found, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(found); err != nil || found != filepath.Join(dir, "bash") || target != helper {
		t.Fatalf("bash resolves to %q -> %q (%v), want the shell dir link to %q", found, target, err, helper)
	}
}

// TestInstallShellDirRejects 는 바꿔치기됐거나 다른 사람이 쓸 수 있는 셸 대체 디렉터리로는 시작하지 않는지 본다.
func TestInstallShellDirRejects(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir, helper string)
		want  string
	}{
		{
			name: "dir is a symbolic link",
			setup: func(t *testing.T, dir, helper string) {
				real := t.TempDir()
				mustSymlink(t, helper, filepath.Join(real, "bash"))
				mustSymlink(t, real, dir)
			},
			want: "is not a directory",
		},
		{
			name: "group writable",
			setup: func(t *testing.T, dir, helper string) {
				mkdirWithMode(t, dir, 0o770)
				mustSymlink(t, helper, filepath.Join(dir, "bash"))
			},
			want: "writable by group or others",
		},
		{
			name:  "no bash link",
			setup: func(t *testing.T, dir, helper string) { mkdirWithMode(t, dir, 0o750) },
			want:  "has no bash link",
		},
		{
			name: "bash is a regular file",
			setup: func(t *testing.T, dir, helper string) {
				mkdirWithMode(t, dir, 0o750)
				writeFileWithMode(t, filepath.Join(dir, "bash"), 0o755)
			},
			want: "must be a symbolic link",
		},
		{
			name: "bash points elsewhere",
			setup: func(t *testing.T, dir, helper string) {
				mkdirWithMode(t, dir, 0o750)
				mustSymlink(t, "/bin/sh", filepath.Join(dir, "bash"))
			},
			want: "not the sandbox helper",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			helper := writeFakeHelper(t, `exit 0`)
			dir := filepath.Join(t.TempDir(), SandboxShellDirName)
			tc.setup(t, dir, helper)
			if _, err := installShellDir(dir, helper); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("installShellDir error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestShellMissingFailsClosed 는 켠 뒤 셸 대체 링크가 없어지면 에이전트 명령의 모든 실행 경로(Bash 동기·백그라운드,
// Monitor, shell_open, 명령형 사용자 지정 도구)가 오류로 끝나고 아무것도 실행하지 않는지 본다. 서버 PATH에는 셸
// 대체 디렉터리만 있으므로 진짜 bash(서버 uid)로 넘어가는 길이 없다.
func TestShellMissingFailsClosed(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv(AgentPathEnv, "")
	helper := writeFakeHelper(t, `exec /bin/sh "$@"`)
	dir, err := installShellDir(filepath.Join(t.TempDir(), SandboxShellDirName), helper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := useShellPath(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "bash")); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	tasks, err := actool.NewManager("fail-closed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tasks.Cleanup)
	tc := &actool.ToolContext{WorkingDir: ws, Tasks: tasks}
	marker := func(name string) string { return "id -u > " + name }
	cases := []struct {
		name string
		call func() (actool.Result, error)
	}{
		{"bash sync", func() (actool.Result, error) {
			return callTool(actool.NewBash(), &actool.ToolContext{WorkingDir: ws}, map[string]any{"command": marker("sync")})
		}},
		{"bash managed", func() (actool.Result, error) {
			return callTool(actool.NewBash(), tc, map[string]any{"command": marker("managed")})
		}},
		{"bash background", func() (actool.Result, error) {
			return callTool(actool.NewBash(), tc, map[string]any{"command": marker("background"), "run_in_background": true})
		}},
		{"monitor", func() (actool.Result, error) {
			return callTool(actool.NewMonitor(), tc, map[string]any{"command": marker("monitor")})
		}},
		{"shell_open", func() (actool.Result, error) {
			return callTool(actool.NewShellOpen(), tc, map[string]any{"command": marker("shell")})
		}},
		{"custom command tool", func() (actool.Result, error) {
			return (&Server{}).runCommandTool(context.Background(), json.RawMessage(`{"command":"id -u > custom"}`), nil, &actool.ToolContext{WorkingDir: ws})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := c.call()
			if err == nil && !res.IsError {
				t.Fatalf("%s ran without the shell substitute: %q", c.name, res.Flatten())
			}
			t.Logf("%s: err=%v result=%q", c.name, err, res.Flatten())
		})
	}
	tasks.Cleanup()
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("commands ran and wrote %v in the workspace", entries)
	}
}

// TestToolTestWorkspace 는 사용자 지정 도구 시험 실행이 데이터 디렉터리가 아니라 sessions/ 아래 빈 임시 작업 공간에서
// 도는지 본다. 켜면 시험 명령이 도는 공용 uid에만 ACL로 쓰기를 준다(#182).
func TestToolTestWorkspace(t *testing.T) {
	cases := []struct {
		name    string
		sandbox *AgentSandbox
		want    fs.FileMode
		access  agent.ACL
	}{
		{name: "off", want: 0o700},
		{name: "on", sandbox: &AgentSandbox{uid: 20000, gid: 20000}, want: 0o770,
			access: agentACL(agent.ACLRead|agent.ACLWrite|agent.ACLExecute, 20000, agent.ACLRead|agent.ACLWrite|agent.ACLExecute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sandbox != nil {
				skipWithoutACL(t)
			}
			dataDir := t.TempDir()
			s := &Server{m: &Manager{dir: dataDir}, sandbox: tc.sandbox}
			ws, err := s.toolTestWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(ws) != filepath.Join(dataDir, toolTestWorkspaceDir) {
				t.Fatalf("workspace %s is not under %s", ws, filepath.Join(dataDir, toolTestWorkspaceDir))
			}
			info, err := os.Stat(ws)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != tc.want {
				t.Fatalf("workspace mode = %v, want %v", got, tc.want)
			}
			if got := readACLOf(t, ws, agent.ACLAccess); !slices.Equal(got, tc.access) {
				t.Fatalf("workspace ACL = %v, want %v", got, tc.access)
			}
		})
	}
}

func callTool(tool actool.CoreTool, tc *actool.ToolContext, input map[string]any) (actool.Result, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return actool.Result{}, err
	}
	return tool.Call(context.Background(), raw, tc)
}

func mkdirWithMode(t *testing.T, path string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func writeFileWithMode(t *testing.T, path string, perm fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// TestCheckWorkspaceACL 은 데이터 디렉터리 ACL 자기 검사(#182)를 본다. 지원하면 통과하고, ACL을 지원하지 않거나
// (ENOTSUP), uid 항목을 거부하거나(EINVAL), 쓰기가 성공했다고 하고 남지 않으면 원인이 적힌 오류로 멈춘다. 어느
// 경우든 검사 디렉터리를 남기지 않는다. 실패 경로는 파일 시스템을 바꿀 수 없어 쓰는 함수를 바꿔 낸다.
func TestCheckWorkspaceACL(t *testing.T) {
	const uid = 20000
	failWith := func(errno syscall.Errno) aclSetter {
		return func(*os.File, agent.ACLKind, agent.ACL) error { return errno }
	}
	cases := []struct {
		name    string
		setACL  aclSetter
		wantIs  error
		wantMsg string
	}{
		{name: "supported", setACL: agent.SetACL},
		{name: "unsupported", setACL: failWith(syscall.EOPNOTSUPP), wantIs: syscall.ENOTSUP, wantMsg: "does not support POSIX ACLs"},
		{name: "uid rejected", setACL: failWith(syscall.EINVAL), wantIs: syscall.EINVAL, wantMsg: "rejected a POSIX ACL entry for uid 20000"},
		{name: "silently dropped", setACL: func(*os.File, agent.ACLKind, agent.ACL) error { return nil },
			wantIs: agent.ErrNoACL, wantMsg: "did not keep it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			err := checkWorkspaceACL(dataDir, uid, tc.setACL)
			if tc.wantIs == nil && errors.Is(err, syscall.ENOTSUP) {
				t.Skipf("temp dir file system does not support POSIX ACLs: %v", err)
			}
			if tc.wantIs == nil {
				if err != nil {
					t.Fatalf("checkWorkspaceACL: %v", err)
				}
			} else {
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("err = %v, want it to wrap %v", err, tc.wantIs)
				}
				if !strings.Contains(err.Error(), tc.wantMsg) || !strings.Contains(err.Error(), dataDir) {
					t.Fatalf("err = %q, want it to name %q and the data dir", err, tc.wantMsg)
				}
			}
			entries, err := os.ReadDir(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("data dir has %d leftover entries (first %q), want none", len(entries), entries[0].Name())
			}
		})
	}
}
