package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestPrepareSharedWorkspace 는 작업 공간 공유 규칙(shareRuleFor)대로 권한이 맞춰지는지 본다. 에이전트 그룹 쓰기는
// 작업 공간 디렉터리(sessions/<sid>, tasks/<tid>, tasks/<tid>/i<iid>)와 그 안에만 있고, 그 위와 drafts는 읽고
// 지나가기만 한다. tasks/<tid>에는 sticky가 붙는다. 작업 공간 밖을 가리키는 링크(파일·디렉터리)를 따라가지 않고,
// 밖의 서버 파일을 하드 링크로 들여온 것도 바꾸지 않는다. 테스트 사용자는 다른 그룹으로 chown할 수 없을 수 있어
// 자기 주 그룹을 에이전트 그룹으로 쓴다(그룹 변경 자체는 컨테이너 확인에서 본다).
func TestPrepareSharedWorkspace(t *testing.T) {
	gid := os.Getgid()
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	outsideDir := filepath.Join(base, "outside")
	outside := filepath.Join(outsideDir, "oauth.key")
	mkdirWithMode(t, outsideDir, 0o700)
	writeFileWithMode(t, outside, 0o600)
	mkdirWithMode(t, filepath.Join(dataDir, "tasks", "7", "i3"), 0o755)
	writeFileWithMode(t, filepath.Join(dataDir, "tasks", "7", "i3", "notes.md"), 0o644)
	writeFileWithMode(t, filepath.Join(dataDir, "tasks", "7", "plan.md"), 0o644)
	writeFileWithMode(t, filepath.Join(dataDir, "tasks", "stray.txt"), 0o666)
	mkdirWithMode(t, filepath.Join(dataDir, "sessions", "conv-1"), 0o755)
	mkdirWithMode(t, filepath.Join(dataDir, "drafts", "d1", "uploads"), 0o777)
	writeFileWithMode(t, filepath.Join(dataDir, "drafts", "d1", "uploads", "a.txt"), 0o666)
	writeFileWithMode(t, filepath.Join(dataDir, "traffic.db"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dataDir, "tasks", "7", "key-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(dataDir, "tasks", "8")); err != nil {
		t.Fatal(err)
	}
	hardLinked := filepath.Join(base, "server.key")
	writeFileWithMode(t, hardLinked, 0o600)
	if err := os.Link(hardLinked, filepath.Join(dataDir, "tasks", "7", "i3", "hard.key")); err != nil {
		t.Fatal(err)
	}
	// 그룹·다른 사용자 쓰기가 있는 데이터 디렉터리와 tasks/도 쓰기가 빠져야 한다. 에이전트 그룹이 나머지 항목이나
	// 작업 공간을 바꿔치기하지 못하게.
	if err := os.Chmod(dataDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dataDir, "tasks"), 0o777); err != nil {
		t.Fatal(err)
	}

	if err := prepareSharedWorkspace(dataDir, gid); err != nil {
		t.Fatal(err)
	}

	setgid, sticky := fs.ModeSetgid, fs.ModeSticky
	cases := []struct {
		path string
		want fs.FileMode
	}{
		{path: dataDir, want: 0o715},
		{path: filepath.Join(dataDir, "tasks"), want: setgid | 0o755},
		{path: filepath.Join(dataDir, "sessions"), want: setgid | 0o755},
		{path: filepath.Join(dataDir, "drafts"), want: setgid | 0o755},
		{path: filepath.Join(dataDir, "tasks", "stray.txt"), want: 0o644},
		{path: filepath.Join(dataDir, "tasks", "7"), want: setgid | sticky | 0o775},
		{path: filepath.Join(dataDir, "tasks", "7", "plan.md"), want: 0o664},
		{path: filepath.Join(dataDir, "tasks", "7", "i3"), want: setgid | 0o775},
		{path: filepath.Join(dataDir, "tasks", "7", "i3", "notes.md"), want: 0o664},
		{path: filepath.Join(dataDir, "sessions", "conv-1"), want: setgid | 0o775},
		{path: filepath.Join(dataDir, "drafts", "d1"), want: setgid | 0o755},
		{path: filepath.Join(dataDir, "drafts", "d1", "uploads"), want: setgid | 0o755},
		{path: filepath.Join(dataDir, "drafts", "d1", "uploads", "a.txt"), want: 0o644},
		{path: filepath.Join(dataDir, "traffic.db"), want: 0o644},
		{path: outsideDir, want: 0o700},
		{path: outside, want: 0o600},
		{path: hardLinked, want: 0o600},
	}
	for _, tc := range cases {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid | fs.ModeSticky); got != tc.want {
			t.Errorf("%s mode = %v, want %v", tc.path, got, tc.want)
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
// 도는지 본다. 켜면 에이전트 그룹이 쓸 수 있게(그룹 rwx, setgid) 맞춘다.
func TestToolTestWorkspace(t *testing.T) {
	cases := []struct {
		name    string
		sandbox *AgentSandbox
		want    fs.FileMode
	}{
		{name: "off", want: 0o700},
		{name: "on", sandbox: &AgentSandbox{}, want: fs.ModeSetgid | 0o770},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
