package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
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
// 명령은 도우미(cmd/artex-sandbox)를 거쳐 그 작업·대화형 세션에 할당한 uid(#166 풀)와 같은 값의 gid로 돈다(#177, #182).
// 설정한 공용 uid·gid(AgentUIDEnv·AgentGIDEnv)는 작업·세션에 속하지 않는 명령(설정 화면의 MCP 도구 목록, 사용자 지정
// 도구 시험)에만 쓴다. 작업 공간은 서버 소유이고 POSIX ACL로 그 작업·세션 uid에만 열린다(agent/workspaceacl.go).
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
// 데이터 디렉터리가 POSIX ACL을 지원하는지 확인(checkWorkspaceACL), 데이터 디렉터리와 tasks·sessions·drafts의 권한
// 맞추기(prepareWorkspaceRoots), 셸 대체 디렉터리 확인(없으면 만들기), 서버 PATH를 셸 대체 디렉터리 하나로 바꾸기,
// 그 PATH로 bash를 실행해 에이전트 uid가 나오는지 확인, 프로세스 환경에서 공용 uid·gid 지우기(clearAgentIdentityEnv).
// 작업·세션마다의 ACL은 DB의 할당 표가 필요해 PrepareAgentWorkspaces가 다시 건다.
//
// 프로세스 환경(PATH)을 바꾸므로 에이전트가 자식 프로세스를 띄우기 전, 시작할 때 한 번만 부른다.
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
	if err := prepareWorkspaceRoots(cfg.DataDir); err != nil {
		return nil, err
	}
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
// 없다. 작업 공간은 ACL로 나누므로(#182) 서버가 에이전트 그룹에 속할 필요는 없다.
func checkServerIdentity(uid, gid int) error {
	if uid == os.Getuid() {
		return fmt.Errorf("%s=%d is the server's own uid; use a separate agent user", AgentUIDEnv, uid)
	}
	if gid == os.Getgid() {
		return fmt.Errorf("%s=%d is the server's own primary group; the agent could then run the sandbox helper (mode 0750 root:<server group>)", AgentGIDEnv, gid)
	}
	return nil
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

// Env 는 감싼 명령(stdio MCP, python 도구)에 넘길 환경 변수다. identity(RunIdentity·SharedEnv의 KEY=VALUE)에 원래
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

// identityEnv 는 uid·gid를 도우미가 읽는 환경 변수로 만든다.
func identityEnv(uid, gid int) []string {
	return []string{AgentUIDEnv + "=" + strconv.Itoa(uid), AgentGIDEnv + "=" + strconv.Itoa(gid)}
}

// SharedEnv 는 작업·대화형 세션에 속하지 않는 명령(설정 화면의 MCP 도구 목록, 사용자 지정 도구 시험)에 넘길 공용
// uid·gid다. 꺼져 있으면 nil이다.
func (s *AgentSandbox) SharedEnv() []string {
	if s == nil {
		return nil
	}
	return identityEnv(s.uid, s.gid)
}

// errRunWithoutOwner 는 uid 분리를 켰는데 에이전트 실행이 작업에도 대화형 세션에도 속하지 않을 때 올린다.
var errRunWithoutOwner = errors.New("the agent run belongs to no task or conversation")

// RunIdentity 는 에이전트 실행 ri의 uid와 그 명령에 넘길 uid·gid(KEY=VALUE)다(#177). 작업 실행은 작업 uid, 대화형
// 세션은 세션 uid를 받고, gid는 uid와 같은 값이다(#182, 풀 사용자마다 같은 번호의 그룹이 있다). 받지 못하면(풀 고갈,
// DB 오류, 속한 곳 없음) 오류다. 호출자는 그 실행을 시작하지 않는다. 꺼져 있으면 0, nil, nil이다.
func (s *AgentSandbox) RunIdentity(ctx context.Context, pg *db.DB, ri agent.RunInfo) (int, []string, error) {
	if s == nil {
		return 0, nil, nil
	}
	var uid int
	var err error
	switch {
	case ri.TaskID > 0:
		uid, err = s.TaskUID(ctx, pg, ri.TaskID)
	case ri.SessionID != "":
		conversationID, ok := conversationIDFromSessionKey(ri.SessionID)
		if !ok {
			return 0, nil, fmt.Errorf("session %q: %w", ri.SessionID, errRunWithoutOwner)
		}
		uid, err = s.ConversationUID(ctx, pg, conversationID)
	default:
		err = errRunWithoutOwner
	}
	if err != nil {
		return 0, nil, err
	}
	return uid, identityEnv(uid, uid), nil
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

// PrepareAgentWorkspaces 는 uid 분리를 켰을 때 DB의 할당 표(agent_uids)대로 작업 공간 ACL을 다시 건다(#182). 할당이
// 있는 작업·대화의 작업 공간은 그 uid에만 열고, 할당이 없는 것(지운 대화 등)은 서버 전용으로 닫는다. 에이전트가
// 명령을 실행하기 전, 서버가 시작할 때 한 번 부른다. 실패하면 원인을 올린다(시작하지 않는다). 꺼져 있으면 nil이다.
func PrepareAgentWorkspaces(ctx context.Context, m *Manager, sandbox *AgentSandbox) error {
	if sandbox == nil {
		return nil
	}
	assignments, err := m.pg.ListAgentUIDs(ctx)
	if err != nil {
		return fmt.Errorf("read agent uid assignments: %w", err)
	}
	owners := workspaceOwners{tasks: map[int64]int{}, conversations: map[int64]int{}}
	for _, a := range assignments {
		if a.TaskID != 0 {
			owners.tasks[a.TaskID] = a.UID
		} else {
			owners.conversations[a.ConversationID] = a.UID
		}
	}
	if err := applyWorkspaceACL(m.dir, owners); err != nil {
		return err
	}
	log.Printf("[sandbox] 작업 %d개·대화 %d개의 할당 uid로 작업 공간 ACL을 다시 걸었다", len(owners.tasks), len(owners.conversations))
	return nil
}

// workspaceOwners 는 작업·대화 id에서 할당한 uid를 찾는 표다(DB agent_uids).
type workspaceOwners struct {
	tasks         map[int64]int
	conversations map[int64]int
}

// taskUID 는 tasks/ 아래 이름 name의 작업 uid다. 작업 id 모양이 아니거나 할당이 없으면 0이다.
func (o workspaceOwners) taskUID(name string) int {
	if !isCanonicalPositiveID(name) {
		return 0
	}
	id, _ := strconv.ParseInt(name, 10, 64) // isCanonicalPositiveID가 확인했다
	return o.tasks[id]
}

// sessionUID 는 sessions/ 아래 이름 name(conv-<id>)의 대화 uid다. 대화 세션 이름이 아니거나 할당이 없으면 0이다.
func (o workspaceOwners) sessionUID(name string) int {
	id, ok := conversationIDFromSessionKey(name)
	if !ok {
		return 0
	}
	return o.conversations[id]
}

// agentRemoveTimeout 은 RemoveAll이 에이전트 uid로 지우는 도우미 실행 하나에 주는 시간이다. 큰 작업 공간도 수 초면 끝난다.
const agentRemoveTimeout = 5 * time.Minute

// agentRemoveScript 는 에이전트 uid로 돌아 인자의 디렉터리(그 uid 소유)를 지운다. 지울 자리는 경로가 아니라 서버가 연
// 디렉터리 fd 3으로 받는다. 서버가 경로를 다시 풀거나 chdir하지 않으므로, 걷기와 실행 사이에 상위 이름이 링크로
// 바뀌어도 서버가 연 디렉터리 밖에서 지우지 않는다(#179). cd에 실패하면 아무것도 지우지 않고 끝난다. 소유자는 자기
// 디렉터리를 chmod로 다시 열 수 있다(chmod -R은 디렉터리 권한을 먼저 바꾼 뒤 안을 읽는다). 일부를 지우지 못해도 끝
// 코드로 판단하지 않고, 서버가 다시 지워 남은 것이 있는지로 판단한다.
const agentRemoveScript = `cd -P /proc/self/fd/3 || exit 1; chmod -R u+rwX -- "$@"; rm -rf -- "$@"`

// RemoveAll 은 서버 전용 임시 자리로 옮긴 작업 파일 path(작업 삭제·보관의 임시 디렉터리)를 지운다(#179). 먼저 서버
// uid로 지운다. uid 분리를 켜면 에이전트 uid 소유의 디렉터리가 있을 수 있고, 에이전트가 그 권한을 바꾸면(chmod 000,
// setfacl) 서버는 안을 비우지 못한다. 서버에는 CAP_DAC_OVERRIDE·CAP_FOWNER가 없다. 그래서 서버 uid로 지우지 못하면,
// 서버 소유 디렉터리 바로 아래에 있는 다른 uid 소유 디렉터리마다 도우미로 그 소유 uid가 되어 지운 뒤 서버가 다시 지운다.
// 도우미는 늘 하던 대로 허용 범위 밖의 uid(0, 서버 uid 등)를 거부한다. 소유 uid는 파일 시스템에서 읽으므로 DB 할당이
// 없어진 뒤(예전 공용 uid의 파일 포함)에도 지운다.
//
// 걷기는 path부터 링크를 따라가지 않고 fd 기준으로 내려가고(walkAgentOwnedDirs), 도우미에는 경로 대신 서버가 연 부모
// 디렉터리 fd를 넘긴다. 그래서 걷는 도중이나 도우미를 띄우기 전에 이름이 링크로 바뀌어도 path 밖을 지우지 않는다.
// 호출자는 그 작업 uid의 프로세스를 먼저 끝낸다(StopProcesses). 꺼져 있으면 os.RemoveAll과 같다.
func (s *AgentSandbox) RemoveAll(ctx context.Context, path string) error {
	err := os.RemoveAll(path)
	if err == nil || s == nil {
		return err
	}
	helperErrs, walkErr := s.removeAgentOwned(ctx, path)
	if walkErr != nil {
		return errors.Join(append([]error{err, fmt.Errorf("find agent-owned directories under %s: %w", path, walkErr)}, helperErrs...)...)
	}
	if err := os.RemoveAll(path); err != nil {
		return errors.Join(append([]error{err}, helperErrs...)...)
	}
	if len(helperErrs) > 0 {
		log.Printf("[sandbox] %s를 지웠다(도우미가 보고한 오류는 무시함): %v", path, errors.Join(helperErrs...))
	}
	return nil
}

// removeAsOwner 는 도우미로 uid가 되어 dir 안의 names를 지운다. gid도 같은 값이다(풀 사용자마다 같은 번호의 그룹).
// dir은 경로가 아니라 연 fd로 넘긴다(agentRemoveScript).
func (s *AgentSandbox) removeAsOwner(ctx context.Context, dir *os.File, uid int, names []string) error {
	ctx, cancel := context.WithTimeout(ctx, agentRemoveTimeout)
	defer cancel()
	// "sh -c 스크립트 이름 인자..."에서 "artex-remove"는 $0이다. 디렉터리 이름은 인자로만 넘겨 셸이 해석하지 않는다.
	name, args := s.Command("sh", append([]string{"-c", agentRemoveScript, "artex-remove"}, names...))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{dir} // 자식에서 fd 3이 된다(agentRemoveScript)
	cmd.Env = append(identityEnv(uid, uid), AgentPathEnv+"="+s.agentPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remove %q in %s as agent uid %d: %v: %s", names, dir.Name(), uid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// agentStopTimeout 은 StopProcesses가 한 uid의 프로세스가 모두 끝나기를 기다리는 상한이다. SIGKILL을 받은 프로세스는
// 보통 수 밀리초 안에 끝난다. 디스크 입출력에 묶인 프로세스(D 상태)만 조금 더 걸린다.
const agentStopTimeout = 10 * time.Second

// agentStopRetry 는 StopProcesses가 남은 프로세스를 다시 죽이고 확인하는 간격이다.
const agentStopRetry = 50 * time.Millisecond

// agentKillScript 는 에이전트 uid로 돌아 그 uid의 다른 프로세스를 모두 SIGKILL로 죽인다. kill(-1)은 호출한 프로세스
// 자신을 빼고 보낼 권한이 있는 모든 프로세스에 보내는데, 권한을 버린 이 셸이 보낼 수 있는 것은 같은 uid의 프로세스뿐이라
// 다른 uid는 건드리지 않는다. 서버가 /proc에서 pid를 골라 죽이면 그사이 pid가 다른 프로세스에 다시 쓰일 수 있다.
// 죽인 뒤 "ready"를 찍고 표준 입력이 닫힐 때까지 살아 있다. 서버는 그동안 /proc를 읽어 남은 프로세스를 찾고, 이
// 프로세스가 보이는지로 /proc가 그 uid의 프로세스를 숨기지 않는지(hidepid) 확인한다.
const agentKillScript = `kill -9 -1 2>/dev/null; echo ready; exec cat >/dev/null`

// errAgentProcessesRemain 은 StopProcesses가 상한 안에 그 uid의 프로세스를 모두 끝내지 못했다는 뜻이다.
var errAgentProcessesRemain = errors.New("agent processes are still running")

// StopProcesses 는 에이전트 uid 하나의 프로세스를 모두 끝내고, 끝났는지 /proc에서 확인한다(#179). 작업·대화를 지우거나
// 작업을 보관하기 전에 부른다. 셸에서 떼어 띄운 프로세스(setsid, nohup, 이중 fork)는 실행이 끝나도 남아 cwd·열린
// fd로 작업 공간에 계속 닿는다. 지운 뒤 uid가 다른 작업에 다시 할당되면 새 작업의 작업 공간에도 닿는다. 그래서 파일을
// 옮기거나 지우거나 uid를 돌려주기 전에 끝낸다. 좀비는 작업 공간에 닿지 못하므로 끝난 것으로 본다.
//
// 상한(agentStopTimeout) 안에 끝나지 않거나, 도우미를 실행하지 못하거나, /proc에서 그 uid의 프로세스를 볼 수 없으면
// 오류다. 호출자는 지우기·보관을 멈춘다. 꺼져 있으면 아무것도 하지 않는다.
func (s *AgentSandbox) StopProcesses(ctx context.Context, uid int) error {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, agentStopTimeout)
	defer cancel()
	for {
		remaining, err := s.killProcesses(ctx, uid)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: agent uid %d still runs pids %v after %s", errAgentProcessesRemain, uid, remaining, agentStopTimeout)
		case <-time.After(agentStopRetry):
		}
	}
}

// killProcesses 는 도우미로 uid가 되어 그 uid의 프로세스를 한 번 모두 죽이고(agentKillScript), 그 직후 아직 살아 있는
// 그 uid의 프로세스 pid를 돌려준다. 죽인 프로세스가 실제로 끝나는 데는 조금 걸리므로 호출자가 다시 부른다.
func (s *AgentSandbox) killProcesses(ctx context.Context, uid int) ([]int, error) {
	name, args := s.Command("sh", []string{"-c", agentKillScript, "artex-kill"})
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.Env = append(identityEnv(uid, uid), AgentPathEnv+"="+s.agentPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the sandbox helper to stop agent uid %d: %w", uid, err)
	}
	ready, readErr := bufio.NewReader(stdout).ReadString('\n')
	var remaining []int
	var scanErr error
	if readErr == nil && ready == "ready\n" {
		remaining, scanErr = liveAgentProcesses(uid, cmd.Process.Pid)
	}
	stdin.Close() // 확인이 끝났으니 cat을 끝낸다
	waitErr := cmd.Wait()
	if readErr != nil || ready != "ready\n" {
		return nil, fmt.Errorf("stop agent uid %d: the sandbox helper did not run (%v, %v): %s", uid, readErr, waitErr, strings.TrimSpace(stderr.String()))
	}
	if scanErr != nil {
		return nil, fmt.Errorf("stop agent uid %d: %w", uid, scanErr)
	}
	return remaining, nil
}

// procStatus 는 /proc/<pid>/status에서 StopProcesses가 보는 값이다.
type procStatus struct {
	state byte  // State 줄의 첫 글자(R, S, D, Z, X 등)
	uids  []int // Uid 줄의 real, effective, saved, filesystem uid
}

// parseProcStatus 는 /proc/<pid>/status 내용에서 State와 Uid를 읽는다. 둘 중 하나라도 없으면 false다.
func parseProcStatus(data []byte) (procStatus, bool) {
	var st procStatus
	hasState := false
	for line := range strings.Lines(string(data)) {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\n"), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "State":
			if value != "" {
				st.state = value[0]
				hasState = true
			}
		case "Uid":
			for _, field := range strings.Fields(value) {
				uid, err := strconv.Atoi(field)
				if err != nil {
					return procStatus{}, false
				}
				st.uids = append(st.uids, uid)
			}
		}
	}
	return st, hasState && len(st.uids) > 0
}

// isAlive 는 프로세스가 아직 작업 공간에 닿을 수 있는지다. 좀비(Z)와 끝나는 중(X)은 cwd와 열린 fd를 이미 놓았다.
func (st procStatus) isAlive() bool {
	return st.state != 'Z' && st.state != 'X'
}

// reopenRestoredWorkspace 는 uid 분리를 켰을 때 복원한 작업 공간 tasks/<taskID>를 그 작업의 uid에 다시 연다(#179).
// 보관 패키지를 풀면 모든 파일이 서버 소유이고 ACL이 없어 에이전트가 자기 옛 파일을 읽거나 고치지 못한다. 작업 uid는
// 보관 중에도 할당이 남아 같은 값이 돌아온다. 할당이 없으면(기능을 켜기 전에 보관한 작업) 새로 받는다. 받지 못하면
// (풀 고갈 등) 오류다. 호출자는 복원을 되돌린다. 꺼져 있으면 아무것도 하지 않는다.
func (s *AgentSandbox) reopenRestoredWorkspace(ctx context.Context, pg *db.DB, dataDir string, taskID int64) error {
	if s == nil {
		return nil
	}
	uid, err := s.TaskUID(ctx, pg, taskID)
	if err != nil {
		return err
	}
	if err := shareTaskWorkspace(dataDir, strconv.FormatInt(taskID, 10), uid); err != nil {
		return fmt.Errorf("reopen restored workspace of task %d to agent uid %d: %w", taskID, uid, err)
	}
	return nil
}
