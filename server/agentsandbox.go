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

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 에이전트 명령을 서버와 다른 uid로 실행하는 기능(#165).
//
// 서버와 에이전트 명령이 같은 uid면 프롬프트 주입을 당한 에이전트가 서버 키(<키 디렉터리>/oauth.key),
// /proc/<서버 pid>/environ(DB DSN, API 키), 실행 파일 옆 업데이트 경로에 닿는다. 이 기능을 켜면 에이전트
// 명령은 도우미(cmd/artex-sandbox)를 거쳐 그 작업·대화형 세션에 할당한 uid(#166 풀)와 공용 에이전트 gid로 돈다(#177).
// 설정한 공용 uid(AgentUIDEnv)는 작업·세션에 속하지 않는 명령(설정 화면의 MCP 도구 목록, 사용자 지정 도구 시험)에만 쓴다.
//
//   - norma의 Bash(동기·백그라운드)·shell_open·명령형 사용자 지정 도구: PATH에서 bash를 찾으므로 서버의
//     PATH를 도우미를 가리키는 bash 링크 하나만 든 셸 대체 디렉터리로 바꾼다(norma는 고치지 않는다). 원래 PATH는
//     AgentPathEnv로 넘기고 도우미가 권한을 버린 뒤 되돌린다. 셸 대체 링크가 없어지면 bash를 찾지 못해 명령이
//     실패한다. 서버 uid의 진짜 bash로 넘어가는 길이 없다.
//     uid·gid는 실행마다 BashEnv(norma ToolContext.Env)로 넘긴다(RunEnv).
//   - norma Monitor: 켜면 Monitor는 실행되지 않는다(백그라운드 Bash를 쓴다). Monitor는 ToolContext.Env를 넘기지
//     않아 실행의 uid가 도우미에 닿지 않고, 서버 환경의 공용 uid는 지웠으므로 도우미가 거부한다(fail closed).
//     norma는 원 저장소가 없어 고치거나 버전을 올릴 수 없다(#163).
//   - stdio MCP, 사용자 지정 python 도구: 서버가 직접 띄우므로 "도우미 exec -- <명령>"으로 감싸고 그 실행의 uid·gid를 넘긴다.
//   - 파일 도구(Read/Write 등)는 서버 프로세스 안에서 서버 uid로 돈다. 경로 제한(agent/filescope.go)이 맡는다.
//
// 기능은 기본 꺼짐이다. AgentUIDEnv·AgentGIDEnv가 둘 다 비어 있으면 아무것도 바꾸지 않는다.

// AgentUIDEnv·AgentGIDEnv 는 에이전트 명령을 실행할 uid·gid를 정하는 환경 변수다. 도우미도 같은 이름을
// 읽는다. 허용 범위(20000-29999)는 도우미 코드에 고정돼 있다.
const (
	AgentUIDEnv = "ARTEX_AGENT_UID"
	AgentGIDEnv = "ARTEX_AGENT_GID"
)

// AgentPathEnv 는 기능을 켰을 때 원래 PATH를 도우미에게 넘기는 환경 변수다. 서버 자신의 PATH는 셸 대체
// 디렉터리 하나뿐이라, 도우미가 권한을 버린 뒤 이 값으로 에이전트 명령의 PATH를 되돌린다.
const AgentPathEnv = "ARTEX_AGENT_PATH"

// SandboxHelperName 은 실행 파일 옆에 두는 도우미 파일 이름이다.
const SandboxHelperName = "artex-sandbox"

// SandboxShellDirName 은 실행 파일 옆에 두는 셸 대체 디렉터리 이름이다. Docker 이미지는 root 소유로 미리 넣고,
// 단일 바이너리는 서버가 처음 켤 때 만든다. 임시 디렉터리에 두지 않는 이유: systemd-tmpfiles 같은 정리 작업이
// 지울 수 있다.
const SandboxShellDirName = "artex-shell"

// sandboxCheckTimeout 은 시작 때 도우미 자기 검사에 주는 시간이다. 정상이면 수 밀리초에 끝난다.
const sandboxCheckTimeout = 10 * time.Second

// sharedWorkspaceDirs 는 서버와 에이전트가 함께 쓰는 데이터 디렉터리 아래 디렉터리다(작업·대화 작업 공간과
// 작업 생성 전 첨부). 데이터 디렉터리의 나머지(트래픽 저장소 등)는 에이전트 그룹에 쓰기를 주지 않는다.
var sharedWorkspaceDirs = []string{"tasks", "sessions", "drafts"}

// aclSetter 는 연 파일에 ACL을 쓰는 함수다(agent.SetACL). 자기 검사의 실패 경로를 시험하려고 인자로 받는다.
type aclSetter func(f *os.File, kind agent.ACLKind, acl agent.ACL) error

// ErrAgentSandboxUnsupported 는 Linux가 아닌 시스템에서 기능을 켰을 때 올린다.
var ErrAgentSandboxUnsupported = errors.New("agent uid separation is supported only on Linux")

// AgentSandboxConfig 는 EnableAgentSandbox의 입력이다. UID·GID는 환경 변수 원문이다.
type AgentSandboxConfig struct {
	UID      string
	GID      string
	Helper   string // 도우미 실행 파일 경로
	ShellDir string // 셸 대체 디렉터리(없으면 만든다)
	DataDir  string // 서버 데이터 디렉터리(작업 공간)
}

// AgentSandbox 는 켜진 uid 분리 설정이다. nil이면 꺼진 것이고, 메서드는 명령을 바꾸지 않는다.
type AgentSandbox struct {
	helper    string
	uid       int
	gid       int
	shellDir  string
	agentPath string // 기능을 켜기 전의 PATH. 에이전트 명령과 python 찾기에 쓴다
}

// EnableAgentSandbox 는 설정이 켜져 있으면 uid 분리를 준비하고, 꺼져 있으면 nil, nil을 돌려준다.
//
// 켜져 있으면 다음을 하고, 하나라도 실패하면 원인을 담은 오류를 올린다(조용히 끄지 않는다).
// 도우미 자기 검사, 서버의 CAP_KILL 켜기(다른 uid가 된 자식을 시간 초과·TaskStop에서 죽이려면 필요하다),
// 데이터 디렉터리가 POSIX ACL을 지원하는지 확인(checkWorkspaceACL), 작업 공간을 에이전트 그룹의 setgid 디렉터리로
// 맞추기, umask 002, 셸 대체 디렉터리 확인(없으면 만들기), 서버 PATH를 셸 대체 디렉터리 하나로 바꾸기, 그 PATH로
// bash를 실행해 에이전트 uid가 나오는지 확인, 프로세스 환경에서 공용 uid·gid 지우기(clearAgentIdentityEnv).
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
	if err := checkWorkspaceACL(cfg.DataDir, uid, agent.SetACL); err != nil {
		return nil, err
	}
	if err := prepareSharedWorkspace(cfg.DataDir, gid); err != nil {
		return nil, err
	}
	setSharedUmask()
	shellDir, err := installShellDir(cfg.ShellDir, helper)
	if err != nil {
		return nil, err
	}
	// norma Grep은 처음 쓸 때 PATH에서 rg를 찾아 기억한다. PATH를 바꾸기 전에 찾아 두지 않으면 순수 Go 검색으로 떨어진다.
	actool.EnsureRipgrep()
	agentPath, err := useShellPath(shellDir)
	if err != nil {
		return nil, err
	}
	if err := checkShellIdentity(ctx, uid); err != nil {
		return nil, err
	}
	if err := clearAgentIdentityEnv(); err != nil {
		return nil, err
	}
	return &AgentSandbox{helper: helper, uid: uid, gid: gid, shellDir: shellDir, agentPath: agentPath}, nil
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
const sandboxHelperHint = " (if this is a permission error: install the helper next to the artex binary as root with chown root:<server group> artex-sandbox; chmod 0750 artex-sandbox; setcap cap_setuid,cap_setgid+ep artex-sandbox, and do not use no-new-privileges or drop SETUID/SETGID. See README \"에이전트 uid 분리\")"

// useShellPath 는 서버 PATH를 셸 대체 디렉터리 하나로 바꾸고 원래 PATH를 AgentPathEnv에 둔다. 원래 PATH를
// 돌려준다. 서버 uid가 이름으로 찾는 명령은 셸 대체의 bash뿐이 된다. 링크가 없어지면 진짜 bash로 넘어가지 않고
// 찾기가 실패한다.
func useShellPath(shellDir string) (string, error) {
	agentPath := os.Getenv("PATH")
	if err := os.Setenv(AgentPathEnv, agentPath); err != nil {
		return "", fmt.Errorf("set %s: %w", AgentPathEnv, err)
	}
	if err := os.Setenv("PATH", shellDir); err != nil {
		return "", fmt.Errorf("set PATH: %w", err)
	}
	return agentPath, nil
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

// clearAgentIdentityEnv 는 프로세스 환경에서 공용 uid·gid를 지운다(#177). norma와 서버는 자식 환경을 os.Environ()에
// 실행의 값을 덧붙여 만든다. 남겨 두면 실행의 uid를 싣지 못한 길(배선 누락)이 공용 uid로 조용히 돈다. 지우면 도우미가
// "ARTEX_AGENT_UID is not set"으로 거부한다(fail closed).
func clearAgentIdentityEnv() error {
	for _, name := range []string{AgentUIDEnv, AgentGIDEnv} {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("unset %s: %w", name, err)
		}
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

// Env 는 감싼 명령(stdio MCP, python 도구)에 넘길 환경 변수다. identity(RunEnv·SharedEnv의 KEY=VALUE)에 원래
// PATH를 더한다. 도우미는 이 값으로 대상 uid·gid와 PATH를 정한다. identity가 비면 uid·gid가 없어 도우미가 거부한다.
// 꺼져 있으면 nil이다.
func (s *AgentSandbox) Env(identity []string) map[string]string {
	if s == nil {
		return nil
	}
	env := map[string]string{AgentPathEnv: s.agentPath}
	for _, kv := range identity {
		if name, value, ok := strings.Cut(kv, "="); ok {
			env[name] = value
		}
	}
	return env
}

// identityEnv 는 uid와 공용 에이전트 gid를 도우미가 읽는 환경 변수로 만든다. 작업 gid는 #182에서 나눈다.
func (s *AgentSandbox) identityEnv(uid int) []string {
	return []string{AgentUIDEnv + "=" + strconv.Itoa(uid), AgentGIDEnv + "=" + strconv.Itoa(s.gid)}
}

// SharedEnv 는 작업·대화형 세션에 속하지 않는 명령(설정 화면의 MCP 도구 목록, 사용자 지정 도구 시험)에 넘길 공용
// uid·gid다. 꺼져 있으면 nil이다.
func (s *AgentSandbox) SharedEnv() []string {
	if s == nil {
		return nil
	}
	return s.identityEnv(s.uid)
}

// errRunWithoutOwner 는 uid 분리를 켰는데 에이전트 실행이 작업에도 대화형 세션에도 속하지 않을 때 올린다.
var errRunWithoutOwner = errors.New("the agent run belongs to no task or conversation")

// RunEnv 는 에이전트 실행 ri의 명령에 넘길 uid·gid(KEY=VALUE)다(#177). 작업 실행은 작업 uid, 대화형 세션은 세션 uid를
// 받는다. 받지 못하면(풀 고갈, DB 오류, 속한 곳 없음) 오류다. 호출자는 그 실행을 시작하지 않는다. 꺼져 있으면 nil, nil이다.
func (s *AgentSandbox) RunEnv(ctx context.Context, pg *db.DB, ri agent.RunInfo) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	var uid int
	var err error
	switch {
	case ri.TaskID > 0:
		uid, err = s.TaskUID(ctx, pg, ri.TaskID)
	case ri.SessionID != "":
		conversationID, ok := conversationIDFromSessionKey(ri.SessionID)
		if !ok {
			return nil, fmt.Errorf("session %q: %w", ri.SessionID, errRunWithoutOwner)
		}
		uid, err = s.ConversationUID(ctx, pg, conversationID)
	default:
		err = errRunWithoutOwner
	}
	if err != nil {
		return nil, err
	}
	return s.identityEnv(uid), nil
}

// LookPath 는 에이전트 명령의 PATH(기능을 켜기 전의 PATH)에서 file을 찾는다. 켜면 서버 PATH에는 셸 대체만 있어
// exec.LookPath로는 python 같은 도구를 찾지 못한다. 꺼져 있으면 exec.LookPath와 같다.
func (s *AgentSandbox) LookPath(file string) (string, error) {
	if s == nil || strings.ContainsRune(file, filepath.Separator) {
		return exec.LookPath(file)
	}
	for _, dir := range filepath.SplitList(s.agentPath) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue // 상대 경로 항목은 작업 디렉터리에 따라 뜻이 바뀌므로 쓰지 않는다(exec.ErrDot과 같은 이유)
		}
		path := filepath.Join(dir, file)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s: %w", file, AgentPathEnv, exec.ErrNotFound)
}

// SharesWorkspace 는 서버가 쓴 작업 공간 파일에 그룹 쓰기를 줘야 하는지다. 켜져 있으면 에이전트는 다른
// uid라서 그룹 권한으로만 서버가 만든 파일을 고칠 수 있다.
func (s *AgentSandbox) SharesWorkspace() bool {
	return s != nil
}

// ShellDir 는 서버 PATH로 쓰는 셸 대체 디렉터리다. 꺼져 있으면 빈 문자열이다.
func (s *AgentSandbox) ShellDir() string {
	if s == nil {
		return ""
	}
	return s.shellDir
}

// TaskUID 는 켜져 있으면 작업의 에이전트 uid를 DB 풀에서 받는다(#166). 이미 받았으면 같은 값이다. 꺼져 있으면
// 0, nil이다. 풀이 비면 db.ErrAgentUIDPoolExhausted를 올린다.
func (s *AgentSandbox) TaskUID(ctx context.Context, pg *db.DB, taskID int64) (int, error) {
	if s == nil {
		return 0, nil
	}
	uid, err := pg.AcquireTaskAgentUID(ctx, taskID)
	if err != nil {
		return 0, fmt.Errorf("assign agent uid to task %d: %w", taskID, err)
	}
	return uid, nil
}

// ConversationUID 는 대화형 세션(conversations)에 대해 TaskUID와 같은 일을 한다.
func (s *AgentSandbox) ConversationUID(ctx context.Context, pg *db.DB, conversationID int64) (int, error) {
	if s == nil {
		return 0, nil
	}
	uid, err := pg.AcquireConversationAgentUID(ctx, conversationID)
	if err != nil {
		return 0, fmt.Errorf("assign agent uid to conversation %d: %w", conversationID, err)
	}
	return uid, nil
}
