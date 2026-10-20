package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 에이전트 명령을 서버와 다른 uid로 실행하는 기능(#165).
//
// 서버와 에이전트 명령이 같은 uid면 프롬프트 주입을 당한 에이전트가 서버 키(<키 디렉터리>/oauth.key),
// /proc/<서버 pid>/environ(DB DSN, API 키), 실행 파일 옆 업데이트 경로에 닿는다. 이 기능을 켜면 에이전트
// 명령은 도우미(cmd/artex-sandbox)를 거쳐 설정한 uid·gid로 돈다.
//
//   - norma의 Bash(동기·백그라운드)·shell_open·명령형 사용자 지정 도구: PATH에서 bash를 찾으므로 PATH 맨 앞에
//     도우미를 가리키는 bash 링크를 둔다(norma는 고치지 않는다).
//   - stdio MCP, 사용자 지정 python 도구: 서버가 직접 띄우므로 "도우미 exec -- <명령>"으로 감싼다.
//   - 파일 도구(Read/Write 등)는 서버 프로세스 안에서 서버 uid로 돈다. 경로 제한(agent/filescope.go)이 맡는다.
//
// 기능은 기본 꺼짐이다. AgentUIDEnv·AgentGIDEnv가 둘 다 비어 있으면 아무것도 바꾸지 않는다.

// AgentUIDEnv·AgentGIDEnv 는 에이전트 명령을 실행할 uid·gid를 정하는 환경 변수다. 도우미도 같은 이름을
// 읽는다. 허용 범위(20000-29999)는 도우미 코드에 고정돼 있다.
const (
	AgentUIDEnv = "ARTEX_AGENT_UID"
	AgentGIDEnv = "ARTEX_AGENT_GID"
)

// SandboxHelperName 은 실행 파일 옆에 두는 도우미 파일 이름이다.
const SandboxHelperName = "artex-sandbox"

// sandboxCheckTimeout 은 시작 때 도우미 자기 검사에 주는 시간이다. 정상이면 수 밀리초에 끝난다.
const sandboxCheckTimeout = 10 * time.Second

// sharedWorkspaceDirs 는 서버와 에이전트가 함께 쓰는 데이터 디렉터리 아래 디렉터리다(작업·대화 작업 공간과
// 작업 생성 전 첨부). 데이터 디렉터리의 나머지(트래픽 저장소 등)는 에이전트 그룹에 쓰기를 주지 않는다.
var sharedWorkspaceDirs = []string{"tasks", "sessions", "drafts"}

// ErrAgentSandboxUnsupported 는 Linux가 아닌 시스템에서 기능을 켰을 때 올린다.
var ErrAgentSandboxUnsupported = errors.New("agent uid separation is supported only on Linux")

// AgentSandboxConfig 는 EnableAgentSandbox의 입력이다. UID·GID는 환경 변수 원문이다.
type AgentSandboxConfig struct {
	UID     string
	GID     string
	Helper  string // 도우미 실행 파일 경로
	DataDir string // 서버 데이터 디렉터리(작업 공간)
}

// AgentSandbox 는 켜진 uid 분리 설정이다. nil이면 꺼진 것이고, 메서드는 명령을 바꾸지 않는다.
type AgentSandbox struct {
	helper   string
	uid      int
	gid      int
	shellDir string
}

// EnableAgentSandbox 는 설정이 켜져 있으면 uid 분리를 준비하고, 꺼져 있으면 nil, nil을 돌려준다.
//
// 켜져 있으면 다음을 하고, 하나라도 실패하면 원인을 담은 오류를 올린다(조용히 끄지 않는다).
// 도우미 자기 검사, 서버의 CAP_KILL 켜기(다른 uid가 된 자식을 시간 초과·TaskStop에서 죽이려면 필요하다),
// 작업 공간을 에이전트 그룹의 setgid 디렉터리로 맞추기, umask 002, PATH 맨 앞에 셸 대체 디렉터리 넣기,
// 그 PATH로 bash를 실행해 에이전트 uid가 나오는지 확인.
//
// 프로세스 환경(PATH)과 umask를 바꾸므로 에이전트가 자식 프로세스를 띄우기 전, 시작할 때 한 번만 부른다.
func EnableAgentSandbox(ctx context.Context, cfg AgentSandboxConfig) (*AgentSandbox, error) {
	if cfg.UID == "" && cfg.GID == "" {
		return nil, nil
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%s/%s is set: %w", AgentUIDEnv, AgentGIDEnv, ErrAgentSandboxUnsupported)
	}
	uid, gid, err := parseAgentIDs(cfg.UID, cfg.GID)
	if err != nil {
		return nil, err
	}
	if err := checkServerIdentity(uid, gid); err != nil {
		return nil, err
	}
	helper, err := filepath.Abs(cfg.Helper)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox helper path: %w", err)
	}
	if err := checkSandboxHelper(ctx, helper, uid, gid); err != nil {
		return nil, err
	}
	if err := enableKillCapability(); err != nil {
		return nil, err
	}
	if err := prepareSharedWorkspace(cfg.DataDir, gid); err != nil {
		return nil, err
	}
	setSharedUmask()
	shellDir, err := installShellDir(helper)
	if err != nil {
		return nil, err
	}
	if err := checkShellIdentity(ctx, uid); err != nil {
		return nil, err
	}
	return &AgentSandbox{helper: helper, uid: uid, gid: gid, shellDir: shellDir}, nil
}

// parseAgentIDs 는 두 값이 함께 있는지와 숫자인지만 본다. 허용 범위는 도우미가 판단한다(한곳에 둔다).
func parseAgentIDs(rawUID, rawGID string) (int, int, error) {
	if rawUID == "" || rawGID == "" {
		return 0, 0, fmt.Errorf("set both %s and %s to enable agent uid separation", AgentUIDEnv, AgentGIDEnv)
	}
	uid, err := strconv.Atoi(rawUID)
	if err != nil {
		return 0, 0, fmt.Errorf("%s must be a number: %w", AgentUIDEnv, err)
	}
	gid, err := strconv.Atoi(rawGID)
	if err != nil {
		return 0, 0, fmt.Errorf("%s must be a number: %w", AgentGIDEnv, err)
	}
	return uid, gid, nil
}

// checkServerIdentity 는 서버와 에이전트가 분리되는 조합인지 본다. 에이전트 uid·gid가 서버와 같으면 경계가
// 없다. 서버가 에이전트 그룹에 속하지 않으면 작업 공간을 함께 쓸 수 없다.
func checkServerIdentity(uid, gid int) error {
	if uid == os.Getuid() {
		return fmt.Errorf("%s=%d is the server's own uid; use a separate agent user", AgentUIDEnv, uid)
	}
	if gid == os.Getgid() {
		return fmt.Errorf("%s=%d is the server's own primary group; the agent could then run the sandbox helper (mode 0750 root:<server group>)", AgentGIDEnv, gid)
	}
	groups, err := os.Getgroups()
	if err != nil {
		return fmt.Errorf("read server groups: %w", err)
	}
	for _, g := range groups {
		if g == gid {
			return nil
		}
	}
	return fmt.Errorf("the server (uid %d) is not a member of group %d (%s); add it as a supplementary group so the server and the agent can share the workspace", os.Getuid(), gid, AgentGIDEnv)
}

// checkSandboxHelper 는 도우미를 "check"로 실행해 에이전트 uid·gid로 바뀌고 cap이 0인지 확인한다.
// 도우미가 없거나, 파일 cap이 없거나, no-new-privileges로 cap이 무시되면 여기서 도우미의 오류가 그대로 보인다.
func checkSandboxHelper(ctx context.Context, helper string, uid, gid int) error {
	ctx, cancel := context.WithTimeout(ctx, sandboxCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "check")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		AgentUIDEnv + "=" + strconv.Itoa(uid),
		AgentGIDEnv + "=" + strconv.Itoa(gid),
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sandbox helper self-check failed (%s check: %v): %s%s", helper, err, strings.TrimSpace(out.String()), sandboxHelperHint)
	}
	want := fmt.Sprintf("uid=%d gid=%d ", uid, gid)
	if got := strings.TrimSpace(out.String()); !strings.HasPrefix(got, want) {
		return fmt.Errorf("sandbox helper self-check reported %q, want it to start with %q", got, want)
	}
	return nil
}

// sandboxHelperHint 는 자기 검사가 실패했을 때 흔한 원인을 알려 준다.
const sandboxHelperHint = " (install the helper next to the artex binary as root: chown root:<server group> artex-sandbox; chmod 0750 artex-sandbox; setcap cap_setuid,cap_setgid+ep artex-sandbox. Do not run the container with no-new-privileges or drop SETUID/SETGID. See README \"에이전트 uid 분리\")"

// installShellDir 는 도우미를 가리키는 bash 링크를 둔 디렉터리를 만들어 PATH 맨 앞에 넣는다.
// 디렉터리는 서버 uid 소유 0700이다. 에이전트가 링크를 바꿔 서버 uid로 자기 코드를 실행시키지 못하게,
// 에이전트가 쓸 수 있는 작업 공간이 아니라 임시 디렉터리에 만든다.
func installShellDir(helper string) (string, error) {
	dir, err := os.MkdirTemp("", "artex-shell-")
	if err != nil {
		return "", fmt.Errorf("create shell dir: %w", err)
	}
	if err := os.Symlink(helper, filepath.Join(dir, "bash")); err != nil {
		return "", fmt.Errorf("create shell link: %w", err)
	}
	path := dir
	if old := os.Getenv("PATH"); old != "" {
		path += string(os.PathListSeparator) + old
	}
	if err := os.Setenv("PATH", path); err != nil {
		return "", fmt.Errorf("set PATH: %w", err)
	}
	return dir, nil
}

// checkShellIdentity 는 norma와 같은 방식(PATH의 "bash -c")으로 셸을 띄워 에이전트 uid가 나오는지 본다.
func checkShellIdentity(ctx context.Context, uid int) error {
	ctx, cancel := context.WithTimeout(ctx, sandboxCheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", "-c", "id -u").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run bash through the sandbox shell: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if got := strings.TrimSpace(string(out)); got != strconv.Itoa(uid) {
		return fmt.Errorf("bash through the sandbox shell runs as uid %s, want %d", got, uid)
	}
	return nil
}

// Command 는 서버가 직접 띄우는 에이전트 명령(stdio MCP, python 도구)을 도우미로 감싼다. 꺼져 있으면 그대로다.
func (s *AgentSandbox) Command(name string, args []string) (string, []string) {
	if s == nil {
		return name, args
	}
	return s.helper, append([]string{"exec", "--", name}, args...)
}

// Env 는 감싼 명령에 넘길 환경 변수다. 도우미는 이 값으로 대상 uid·gid를 정한다. 꺼져 있으면 nil이다.
func (s *AgentSandbox) Env() map[string]string {
	if s == nil {
		return nil
	}
	return map[string]string{AgentUIDEnv: strconv.Itoa(s.uid), AgentGIDEnv: strconv.Itoa(s.gid)}
}

// SharesWorkspace 는 서버가 쓴 작업 공간 파일에 그룹 쓰기를 줘야 하는지다. 켜져 있으면 에이전트는 다른
// uid라서 그룹 권한으로만 서버가 만든 파일을 고칠 수 있다.
func (s *AgentSandbox) SharesWorkspace() bool {
	return s != nil
}

// ShellDir 는 PATH 맨 앞에 넣은 셸 대체 디렉터리다. 꺼져 있으면 빈 문자열이다.
func (s *AgentSandbox) ShellDir() string {
	if s == nil {
		return ""
	}
	return s.shellDir
}
