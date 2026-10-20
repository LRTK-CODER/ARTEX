package agent

import (
	"fmt"
	"path/filepath"
	"strings"
)

// 에이전트 uid 분리를 켰을 때 에이전트 명령이 쓰는 HOME·TMPDIR이다(#178). 작업 uid로 나눠도 HOME(서버 사용자 홈,
// 쓰기 불가)과 공용 /tmp를 함께 쓰면 작업끼리 파일이 통하고, 쓸 수 없는 HOME 때문에 npm 같은 도구가 실패한다. 그래서
// 작업·세션마다 작업 공간 옆에 서버 소유 디렉터리를 두고 그 uid에만 ACL로 쓰기를 준다(workspaceacl.go).
//
//	tasks/<taskID>/home, tasks/<taskID>/tmp   작업(planner·mainagent·worker가 같은 작업 uid로 함께 쓴다)
//	sessions/home/<sid>, sessions/tmp/<sid>   대화형 세션(sessions/home·sessions/tmp는 지나가기만 하는 공유 디렉터리)
//
// 작업 공간(work, i<iid>, sessions/<sid>) 안에 두지 않는 이유: 에이전트가 그 이름을 링크로 바꿔 둘 수 있다. 부모를
// 에이전트가 쓰지 못하는 곳에 두면 서버가 경로로 다뤄도 된다.

// AgentHomeEnv 는 에이전트 명령의 HOME을 도우미(cmd/artex-sandbox)에게 넘기는 환경 변수다. 서버 프로세스의 HOME은
// 서버 사용자 홈이라 자식이 그대로 물려받으므로 HOME으로는 넘길 수 없다. 도우미는 권한을 버린 뒤 이 값으로 HOME을
// 바꾸고 이 변수는 지운다. TMPDIR는 도우미가 손대지 않으므로 그 이름 그대로 넘긴다.
const AgentHomeEnv = "ARTEX_AGENT_HOME"

// CommandHomeDirName·CommandTmpDirName 은 HOME·TMPDIR 디렉터리 이름이다. server의 작업 공간 권한 규칙도 이 이름을 쓴다.
const (
	CommandHomeDirName = "home"
	CommandTmpDirName  = "tmp"
)

// commandDirs 는 에이전트 명령 한 실행의 HOME·TMPDIR이다.
type commandDirs struct {
	home string
	tmp  string
}

// taskCommandDirs 는 작업 taskID의 HOME·TMPDIR 경로를 만들지 않고 돌려준다.
func taskCommandDirs(dataDir string, taskID int64) commandDirs {
	taskDir := runDirPath(dataDir, taskID, 0)
	return commandDirs{home: filepath.Join(taskDir, CommandHomeDirName), tmp: filepath.Join(taskDir, CommandTmpDirName)}
}

// sessionCommandDirs 는 대화 작업 디렉터리 sessionDir(sessions/<sid>)의 HOME·TMPDIR 경로를 만들지 않고 돌려준다.
func sessionCommandDirs(sessionDir string) commandDirs {
	sessions, name := filepath.Dir(sessionDir), filepath.Base(sessionDir)
	return commandDirs{home: filepath.Join(sessions, CommandHomeDirName, name), tmp: filepath.Join(sessions, CommandTmpDirName, name)}
}

// EnsureCommandDirs 는 uid 분리를 켰을 때 실행 ri(작업 또는 대화형 세션)의 HOME·TMPDIR를 만들고(없으면) 서버와
// agentUID만 쓰게 맞춘 뒤, 에이전트 명령에 덧붙일 환경 변수(AgentHomeEnv, TMPDIR)를 돌려준다. 작업 실행이면 작업
// 디렉터리(tasks/<tid>)도 그 uid가 지나가게 맞춘다(ensureRunDir와 같다). 그 자리에 링크·파일이 있거나 서버 소유가
// 아니면 오류다. 호출자는 그 실행을 시작하지 않는다(HOME·TMPDIR 없이 돌면 공용 /tmp로 떨어진다).
func EnsureCommandDirs(dataDir string, ri RunInfo, agentUID int) ([]string, error) {
	var dirs commandDirs
	switch {
	case ri.TaskID > 0:
		dirs = taskCommandDirs(dataDir, ri.TaskID)
		if err := EnsureContainerDir(filepath.Join(dataDir, "tasks")); err != nil {
			return nil, err
		}
		if err := ensureACLDir(runDirPath(dataDir, ri.TaskID, 0), agentUID, WorkspaceRead, true); err != nil {
			return nil, err
		}
	case isPlainName(ri.SessionID):
		sessions := filepath.Join(dataDir, "sessions")
		dirs = sessionCommandDirs(filepath.Join(sessions, ri.SessionID))
		for _, dir := range []string{sessions, filepath.Dir(dirs.home), filepath.Dir(dirs.tmp)} {
			if err := EnsureContainerDir(dir); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("run %+v belongs to no task or session", ri)
	}
	for _, dir := range []string{dirs.home, dirs.tmp} {
		if err := ensureACLDir(dir, agentUID, WorkspaceWrite, true); err != nil {
			return nil, err
		}
	}
	return []string{AgentHomeEnv + "=" + dirs.home, "TMPDIR=" + dirs.tmp}, nil
}

// isPlainName 은 name이 경로 구분자 없는 디렉터리 이름 하나인지 본다.
func isPlainName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`)
}
