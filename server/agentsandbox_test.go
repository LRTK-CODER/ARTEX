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

	"github.com/Autumn-27/artex/agent"
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
	if name != "npx" || !reflect.DeepEqual(args, []string{"-y", "pkg"}) || sb.Env(nil) != nil || sb.SharedEnv() != nil {
		t.Fatalf("nil sandbox changed the command: %s %v env=%v shared=%v", name, args, sb.Env(nil), sb.SharedEnv())
	}
	// 꺼져 있으면 실행에 uid를 붙이지 않고 오류도 없다(DB를 보지 않는다).
	if uid, env, err := sb.RunIdentity(context.Background(), nil, agent.RunInfo{TaskID: 1}); uid != 0 || env != nil || err != nil {
		t.Fatalf("nil sandbox RunIdentity = %d, %v, %v, want 0, nil, nil", uid, env, err)
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

func TestAgentSandboxWrapsCommand(t *testing.T) {
	sb := &AgentSandbox{helper: "/app/artex-sandbox", uid: 20000, gid: 20001, agentPath: "/usr/bin:/bin"}
	name, args := sb.Command("npx", []string{"-y", "@playwright/mcp"})
	if name != "/app/artex-sandbox" || !reflect.DeepEqual(args, []string{"exec", "--", "npx", "-y", "@playwright/mcp"}) {
		t.Fatalf("Command = %s %v", name, args)
	}
	cases := []struct {
		name     string
		identity []string
		want     map[string]string
	}{
		{
			// 사용자가 MCP 설정에 같은 이름을 적어도 서버 값이 이긴다. 실행의 uid와 같은 값의 gid가 들어간다(#177, #182).
			name:     "run uid",
			identity: identityEnv(20005, 20005),
			want:     map[string]string{"TOKEN": "t", AgentUIDEnv: "20005", AgentGIDEnv: "20005", AgentPathEnv: "/usr/bin:/bin"},
		},
		{
			name:     "shared uid",
			identity: sb.SharedEnv(),
			want:     map[string]string{"TOKEN": "t", AgentUIDEnv: "20000", AgentGIDEnv: "20001", AgentPathEnv: "/usr/bin:/bin"},
		},
		{
			// 실행의 uid를 싣지 못하면 사용자가 적은 값도 지운다. 도우미가 uid가 없다며 거부한다(fail closed).
			name: "no identity",
			want: map[string]string{"TOKEN": "t", AgentPathEnv: "/usr/bin:/bin"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := stdioMCPEnv(map[string]string{"TOKEN": "t", AgentUIDEnv: "0"}, sb.Env(tc.identity))
			if !reflect.DeepEqual(env, tc.want) {
				t.Fatalf("stdioMCPEnv = %v, want %v", env, tc.want)
			}
		})
	}
	if got := stdioMCPEnv(map[string]string{"TOKEN": "t"}, nil); !reflect.DeepEqual(got, map[string]string{"TOKEN": "t"}) {
		t.Fatalf("stdioMCPEnv(off) = %v", got)
	}
}

// TestAgentSandboxLookPath 는 켰을 때 python 같은 도구를 서버 PATH(셸 대체 하나뿐)가 아니라 에이전트 명령의
// PATH에서 찾는지 본다. 실행 권한이 없거나 디렉터리인 것, 상대 경로 항목은 고르지 않는다.
func TestAgentSandboxLookPath(t *testing.T) {
	bin, noExec, rel := t.TempDir(), t.TempDir(), "relbin"
	writeExecutable(t, filepath.Join(bin, "python3"), 0o755)
	writeExecutable(t, filepath.Join(noExec, "python3"), 0o644)
	if err := os.Mkdir(filepath.Join(noExec, "python"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb := &AgentSandbox{agentPath: strings.Join([]string{rel, noExec, bin}, string(os.PathListSeparator))}
	t.Setenv("PATH", t.TempDir()) // 서버 PATH에는 없다
	if got, err := sb.LookPath("python3"); err != nil || got != filepath.Join(bin, "python3") {
		t.Fatalf("LookPath(python3) = %q, %v, want %q", got, err, filepath.Join(bin, "python3"))
	}
	if got, err := sb.LookPath("python"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("LookPath(python) = %q, %v, want exec.ErrNotFound", got, err)
	}
	var off *AgentSandbox
	if _, err := off.LookPath("python3"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("off LookPath(python3) = %v, want the server PATH lookup to fail", err)
	}
}

func writeExecutable(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// 켠 뒤에는 서버 프로세스 환경에 공용 uid·gid가 남지 않는다(#177). norma와 서버는 자식 환경을 os.Environ()에서
// 시작하므로, 남아 있으면 실행의 uid를 싣지 못한 길이 공용 uid로 돈다. 지우면 도우미가 uid가 없다며 거부한다.
func TestClearAgentIdentityEnv(t *testing.T) {
	t.Setenv(AgentUIDEnv, "20000")
	t.Setenv(AgentGIDEnv, "20000")
	if err := clearAgentIdentityEnv(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{AgentUIDEnv, AgentGIDEnv} {
		if value, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s=%q left in the server environment", name, value)
		}
	}
}

// TestFindCertutil 은 uid 분리를 켤 때 에이전트 명령의 PATH에서 certutil을 찾고, 없으면 설치할 패키지 이름을 담아
// 시작을 막는 오류를 올리는지 본다(#204).
func TestFindCertutil(t *testing.T) {
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, certutilName), 0o755)
	t.Setenv("PATH", bin) // 서버 PATH에 있어도 에이전트 명령의 PATH를 본다
	missing := &AgentSandbox{agentPath: t.TempDir()}
	_, err := missing.findCertutil()
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("findCertutil(no certutil) = %v, want exec.ErrNotFound", err)
	}
	for _, pkg := range []string{"libnss3-tools", "nss-tools"} {
		if !strings.Contains(err.Error(), pkg) {
			t.Fatalf("findCertutil error %q lacks the package name %q", err, pkg)
		}
	}
	found := &AgentSandbox{agentPath: bin}
	if got, err := found.findCertutil(); err != nil || got != filepath.Join(bin, certutilName) {
		t.Fatalf("findCertutil = %q, %v, want %q", got, err, filepath.Join(bin, certutilName))
	}
}

// TestTrustProxyCAOff 는 uid 분리를 껐거나 캡처를 꺼 CA가 없으면 TrustProxyCA가 아무것도 실행하지 않고 HOME에 아무것도
// 만들지 않는지 본다(#204). 켠 쪽의 sandbox는 도우미 경로가 비어 있어 실행하면 실패한다.
func TestTrustProxyCAOff(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("not used"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		sandbox *AgentSandbox
		caCert  string
	}{
		{name: "uid separation off", sandbox: nil, caCert: ca},
		{name: "capture off", sandbox: &AgentSandbox{uid: 20000, gid: 20000}, caCert: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := tc.sandbox.TrustProxyCA(context.Background(), 20001, home, tc.caCert); err != nil {
				t.Fatalf("TrustProxyCA = %v, want nil", err)
			}
			if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
				t.Fatalf("HOME entries = %v, %v, want none", entries, err)
			}
		})
	}
}
