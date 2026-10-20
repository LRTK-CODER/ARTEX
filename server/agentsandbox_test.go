//go:build !windows

package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// writeFakeHelper 는 도우미 대신 쓸 셸 스크립트를 만든다. 실제 도우미와 같은 방식으로 "check"에 답한다.
func writeFakeHelper(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), SandboxHelperName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnableAgentSandboxOffByDefault(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	sb, err := EnableAgentSandbox(context.Background(), AgentSandboxConfig{Helper: "/nonexistent", DataDir: t.TempDir()})
	if err != nil || sb != nil {
		t.Fatalf("EnableAgentSandbox(off) = %v, %v, want nil, nil", sb, err)
	}
	if got := os.Getenv("PATH"); got != "/usr/bin:/bin" {
		t.Fatalf("PATH changed while off: %q", got)
	}
	// 꺼져 있으면 서버가 띄우는 명령과 환경을 바꾸지 않는다.
	name, args := sb.Command("npx", []string{"-y", "pkg"})
	if name != "npx" || !reflect.DeepEqual(args, []string{"-y", "pkg"}) || sb.Env() != nil || sb.SharesWorkspace() {
		t.Fatalf("nil sandbox changed the command: %s %v env=%v", name, args, sb.Env())
	}
}

// TestEnableAgentSandboxFailsLoudly 는 켰는데 준비할 수 없으면 원인을 담은 오류로 멈추는지 본다.
// 조용히 꺼지면 사용자는 분리된 줄 알고 운영한다.
func TestEnableAgentSandboxFailsLoudly(t *testing.T) {
	cases := []struct {
		name     string
		uid, gid string
		want     string
	}{
		{name: "uid only", uid: "20000", want: "set both"},
		{name: "gid only", gid: "20000", want: "set both"},
		{name: "not a number", uid: "agent", gid: "20000", want: "must be a number"},
		{name: "server uid", uid: strconv.Itoa(os.Getuid()), gid: "20000", want: "server's own uid"},
		{name: "server group", uid: "20000", gid: strconv.Itoa(os.Getgid()), want: "server's own primary group"},
		{name: "server not in group", uid: "20000", gid: "29999", want: "not a member of group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", "/usr/bin:/bin")
			helper := writeFakeHelper(t, `echo "uid=20000 gid=20000 groups=0 capeff=0 no_new_privs=1"`)
			sb, err := EnableAgentSandbox(context.Background(), AgentSandboxConfig{UID: tc.uid, GID: tc.gid, Helper: helper, DataDir: t.TempDir()})
			if err == nil {
				t.Fatalf("EnableAgentSandbox = %+v, want error", sb)
			}
			if runtime.GOOS != "linux" {
				if !errors.Is(err, ErrAgentSandboxUnsupported) {
					t.Fatalf("error = %v, want ErrAgentSandboxUnsupported on %s", err, runtime.GOOS)
				}
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if got := os.Getenv("PATH"); got != "/usr/bin:/bin" {
				t.Fatalf("PATH changed after a failed start: %q", got)
			}
		})
	}
}

// TestSandboxHelperSelfCheckFailures 는 도우미가 없거나, 권한이 없어(파일 cap 없음, no-new-privileges)
// 실패하거나, 다른 uid를 보고하면 자기 검사가 도우미의 출력을 담아 실패하는지 본다.
func TestSandboxHelperSelfCheckFailures(t *testing.T) {
	cases := []struct {
		name   string
		helper func(t *testing.T) string
		want   string
	}{
		{
			name:   "missing helper",
			helper: func(t *testing.T) string { return filepath.Join(t.TempDir(), SandboxHelperName) },
			want:   "self-check failed",
		},
		{
			name: "no file capability",
			helper: func(t *testing.T) string {
				return writeFakeHelper(t, `echo "artex-sandbox: setgid 20000: operation not permitted" >&2; exit 126`)
			},
			want: "setgid 20000: operation not permitted",
		},
		{
			name: "wrong uid",
			helper: func(t *testing.T) string {
				return writeFakeHelper(t, `echo "uid=10001 gid=10001 groups=0 capeff=0 no_new_privs=1"`)
			},
			want: "want it to start with",
		},
		{
			name:   "uid prefix only",
			helper: func(t *testing.T) string { return writeFakeHelper(t, `echo "uid=200001 gid=20000 groups=0"`) },
			want:   "want it to start with",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSandboxHelper(context.Background(), tc.helper(t), 20000, 20000)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkSandboxHelper error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestSandboxHelperSelfCheckPasses(t *testing.T) {
	// 실제 도우미처럼 환경 변수로 받은 uid·gid를 보고한다.
	helper := writeFakeHelper(t, `[ "$1" = check ] || exit 2; echo "uid=$ARTEX_AGENT_UID gid=$ARTEX_AGENT_GID groups=0 capeff=0 no_new_privs=1"`)
	if err := checkSandboxHelper(context.Background(), helper, 20000, 20001); err != nil {
		t.Fatal(err)
	}
}

// TestInstallShellDirPrependsPATH 는 켰을 때 PATH 맨 앞이 셸 대체 디렉터리이고, norma처럼 이름 "bash"로
// 찾으면 도우미를 가리키는 링크가 나오는지 본다. 디렉터리는 에이전트가 링크를 바꾸지 못하게 서버만 쓴다.
func TestInstallShellDirPrependsPATH(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TMPDIR", t.TempDir())
	helper := writeFakeHelper(t, `exit 0`)
	dir, err := installShellDir(helper)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := os.Getenv("PATH"), dir+string(os.PathListSeparator)+"/usr/bin:/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	found, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	if found != filepath.Join(dir, "bash") {
		t.Fatalf("bash resolves to %q, want the shell dir link", found)
	}
	if target, err := os.Readlink(found); err != nil || target != helper {
		t.Fatalf("bash link = %q, %v, want %q", target, err, helper)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("shell dir perm = %o, want 700", perm)
	}
}

func TestAgentSandboxWrapsCommand(t *testing.T) {
	sb := &AgentSandbox{helper: "/app/artex-sandbox", uid: 20000, gid: 20001}
	name, args := sb.Command("npx", []string{"-y", "@playwright/mcp"})
	if name != "/app/artex-sandbox" || !reflect.DeepEqual(args, []string{"exec", "--", "npx", "-y", "@playwright/mcp"}) {
		t.Fatalf("Command = %s %v", name, args)
	}
	// 사용자가 MCP 설정에 같은 이름을 적어도 서버 설정이 이긴다. 도우미가 범위를 다시 보지만 다른 uid를 고르게 두지 않는다.
	env := stdioMCPEnv(map[string]string{"TOKEN": "t", AgentUIDEnv: "0"}, sb)
	want := map[string]string{"TOKEN": "t", AgentUIDEnv: "20000", AgentGIDEnv: "20001"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("stdioMCPEnv = %v, want %v", env, want)
	}
	if got := stdioMCPEnv(map[string]string{"TOKEN": "t"}, nil); !reflect.DeepEqual(got, map[string]string{"TOKEN": "t"}) {
		t.Fatalf("stdioMCPEnv(off) = %v", got)
	}
}
