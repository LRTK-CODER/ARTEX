package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// 대상 uid·gid를 주는 환경 변수. 서버 설정과 같은 이름이라 서버 환경을 물려받은 자식이 그대로 읽는다.
const (
	uidEnv = "ARTEX_AGENT_UID"
	gidEnv = "ARTEX_AGENT_GID"
)

// 에이전트 uid·gid로 허용하는 범위. 도우미는 CAP_SETUID로 아무 uid나 될 수 있으므로, 서버가 넘긴 값을
// 믿지 않고 이 범위 밖(0, 시스템 계정, 서버 자신이 흔히 쓰는 1000·10001)이면 거부한다.
// 작업별 uid(#166)도 이 범위에서 고른다. 범위를 바꾸려면 도우미를 다시 빌드해 설치해야 한다.
const (
	minAgentID = 20000
	maxAgentID = 29999
)

// shellName 은 norma가 PATH에서 찾는 셸 이름이다. 이 이름의 링크로 실행되면 셸 대체로 동작한다.
const shellName = "bash"

// identity 는 바꿀 대상 uid·gid다. 보조 그룹은 늘 비운다.
type identity struct {
	uid int
	gid int
}

// parseIdentity 는 환경 변수에서 대상 uid·gid를 읽어 허용 범위 안인지 본다.
func parseIdentity(getenv func(string) string) (identity, error) {
	uid, err := parseID(uidEnv, getenv(uidEnv))
	if err != nil {
		return identity{}, err
	}
	gid, err := parseID(gidEnv, getenv(gidEnv))
	if err != nil {
		return identity{}, err
	}
	return identity{uid: uid, gid: gid}, nil
}

// parseID 는 부호 없는 십진수만 받는다. "+0", " 0", "0x0" 같은 표기로 범위 검사를 비껴가지 못하게 한다.
func parseID(name, raw string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s is not set", name)
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be a decimal number: %w", name, err)
	}
	if n < minAgentID || n > maxAgentID {
		return 0, fmt.Errorf("%s=%d is outside the allowed range %d-%d", name, n, minAgentID, maxAgentID)
	}
	return int(n), nil
}

// mode 는 도우미가 무엇을 실행할지다.
type mode string

const (
	modeShell mode = "shell" // argv[0]이 bash: 진짜 bash에 인자를 그대로 넘긴다
	modeExec  mode = "exec"  // exec -- <명령>
	modeCheck mode = "check" // 바꾼 뒤 상태만 찍는다
)

// invocation 은 명령줄을 푼 결과다. program은 PATH에서 찾을 이름이거나 경로다. argv는 program에 넘길
// argv 전체(argv[0] 포함)다.
type invocation struct {
	mode    mode
	program string
	argv    []string
}

var errUsage = errors.New("usage: artex-sandbox exec -- <command> [args...] | artex-sandbox check")

// parseInvocation 은 os.Args 전체를 받는다.
func parseInvocation(args []string) (invocation, error) {
	if len(args) == 0 {
		return invocation{}, errUsage
	}
	if filepath.Base(args[0]) == shellName {
		return invocation{mode: modeShell, program: shellName, argv: append([]string{shellName}, args[1:]...)}, nil
	}
	if len(args) < 2 {
		return invocation{}, errUsage
	}
	switch args[1] {
	case string(modeCheck):
		if len(args) != 2 {
			return invocation{}, errUsage
		}
		return invocation{mode: modeCheck}, nil
	case string(modeExec):
		// "--"를 꼭 받는다. 명령 이름이 "-"로 시작해도 도우미 옵션으로 읽히지 않게 하기 위해서다.
		if len(args) < 4 || args[2] != "--" || args[3] == "" {
			return invocation{}, errUsage
		}
		return invocation{mode: modeExec, program: args[3], argv: args[3:]}, nil
	}
	return invocation{}, errUsage
}

// stripShellDirs 는 PATH에서 셸 대체 디렉터리(isShellDir가 참인 항목)를 뺀다. 빼지 않으면 에이전트가
// 다시 부른 bash도 도우미를 찾게 되고, 에이전트 uid는 도우미를 실행할 수 없어 실패한다.
func stripShellDirs(path string, isShellDir func(dir string) bool) string {
	var kept []string
	for _, dir := range filepath.SplitList(path) {
		if dir != "" && isShellDir(dir) {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(filepath.ListSeparator))
}
