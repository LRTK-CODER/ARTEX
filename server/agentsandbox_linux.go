package server

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/Autumn-27/artex/agent"
)

// Linux capability·prctl 상수(include/uapi/linux/capability.h, prctl.h). syscall 패키지에 없다.
const (
	linuxCapabilityVersion3 = 0x20080522
	capKill                 = 5
	prCapAmbient            = 47
	prCapAmbientClearAll    = 4
)

type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// enableKillCapability 는 permitted에 있는 CAP_KILL을 모든 스레드의 effective에 올리고 inheritable·ambient cap을 비운다.
//
// norma는 시간 초과·TaskStop·shell_close에서 프로세스 그룹에 SIGKILL을 보낸다. 에이전트 프로세스는 다른
// uid라서 CAP_KILL 없이는 EPERM으로 실패해 프로세스가 남는다. CAP_KILL은 ambient로 받는다. systemd는
// AmbientCapabilities=CAP_KILL, Docker 이미지는 엔트리포인트가 "artex-sandbox launch"로 ambient에 올린 뒤
// start.sh를 실행한다. 서버 실행 파일에 파일 cap을 두지 않는 이유: 파일 cap이 있으면 커널이 secure-exec로
// 실행해 Go 런타임이 GOTRACEBACK=none을 강제하고, 패닉 때 goroutine 추적이 남지 않는다. ambient로만 받은
// cap은 secure-exec를 일으키지 않는다.
// inheritable·ambient를 비우는 이유: ambient CAP_KILL이 서버가 띄우는 감싸지 않은 자식(ripgrep 등)에게 새지 않게 한다.
// capset은 스레드 단위라 AllThreadsSyscall로 모든 스레드에 적용한다(cgo로 빌드하면 ENOTSUP).
func enableKillCapability() error {
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("read server capabilities: %w", errno)
	}
	if data[0].permitted&(1<<capKill) == 0 {
		return errors.New("the server has no CAP_KILL, so it cannot stop agent processes running as another uid (Docker image: the entrypoint starts the server through \"artex-sandbox launch\" when " + AgentUIDEnv + "/" + AgentGIDEnv + " are set; keep KILL in cap_add/cap_drop; systemd: AmbientCapabilities=CAP_KILL)")
	}
	data[0].effective |= 1 << capKill
	data[0].inheritable, data[1].inheritable = 0, 0
	hdr = capHeader{version: linuxCapabilityVersion3}
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("raise CAP_KILL on all threads (a cgo build cannot do this): %w", errno)
	}
	if _, _, errno := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("clear ambient capabilities: %w", errno)
	}
	return nil
}

// setSharedUmask 는 서버가 만드는 작업 공간 디렉터리·파일에 그룹 쓰기를 남긴다(디렉터리 0775, 새 파일 0664).
// 키 파일처럼 0600으로 만드는 파일에는 영향이 없다.
func setSharedUmask() {
	syscall.Umask(0o002)
}

// shareRule 은 작업 공간 항목에 더하고 뺄 권한이다.
type shareRule struct {
	add, remove fs.FileMode
}

// 작업 공간 공유 규칙. 에이전트 그룹 쓰기는 작업 공간 디렉터리(sessions/<sid>, tasks/<tid>/work,
// tasks/<tid>/i<iid>)와 그 안에만 준다. 그 위(데이터 디렉터리, tasks/·sessions/·drafts/, tasks/<tid>)와 drafts, 큰 출력
// 디렉터리(cmd-output), tasks/<tid> 아래의 다른 디렉터리(첨부 uploads, 끈 상태에서 planner가 만든 디렉터리) 아래는
// 에이전트 그룹이 읽고 지나가기만 한다. 작업 공간의 부모에 쓰기가 있으면 에이전트가
// 서버 소유 작업 공간을 옮기고 그 자리에 링크를 둘 수 있다. 배치는 agent의 sharedRunDirs가 정본이다.
var (
	ruleDataDir       = shareRule{add: 0o010, remove: 0o062}                 // 그룹 통과만. 나머지 항목(트래픽 저장소 등)을 지키려고 그룹 읽기·쓰기를 뺀다
	ruleContainer     = shareRule{add: fs.ModeSetgid | 0o050, remove: 0o022} // 그 아래 새 디렉터리가 에이전트 그룹을 물려받게 setgid
	ruleReadOnlyFile  = shareRule{add: 0o040, remove: 0o022}
	ruleTaskDir       = shareRule{add: fs.ModeSetgid | 0o050, remove: fs.ModeSticky | 0o022} // 예전 배치의 sticky·그룹 쓰기를 뺀다
	ruleWorkspaceDir  = shareRule{add: fs.ModeSetgid | 0o070}
	ruleWorkspaceFile = shareRule{add: 0o060}
)

// shareRuleFor 는 공유 디렉터리 top(tasks·sessions·drafts) 아래 depth 깊이(바로 아래가 1) 항목 name의 규칙과,
// 그 아래가 에이전트 읽기 전용 영역인지를 돌려준다. readOnly는 위에서 이미 읽기 전용 영역에 들어왔는지다.
func shareRuleFor(top string, depth int, name string, isDir, readOnly bool) (shareRule, bool) {
	isSessionToolOutput := top == "sessions" && depth == 1 && isDir && name == agent.ToolOutputDirName
	isTaskNonWorkspace := top == "tasks" && depth == 2 && isDir && !agent.IsTaskWorkspaceName(name)
	readOnly = readOnly || top == "drafts" || isSessionToolOutput || isTaskNonWorkspace
	switch {
	case readOnly || (depth == 1 && !isDir):
		// drafts와 uploads는 사용자 첨부, cmd-output은 서버가 쓰는 큰 출력이라 에이전트는 읽기만 한다. tasks/·sessions/
		// 바로 아래 파일도 작업 공간 밖이다.
		if isDir {
			return ruleContainer, readOnly
		}
		return ruleReadOnlyFile, readOnly
	case top == "tasks" && depth == 1:
		return ruleTaskDir, false
	case top == "tasks" && depth == 2 && !isDir:
		return ruleReadOnlyFile, false // 예전 배치에서 planner가 tasks/<tid>에 둔 파일. 이제 작업 공간 밖이다
	case isDir:
		return ruleWorkspaceDir, false
	}
	return ruleWorkspaceFile, false
}

// prepareSharedWorkspace 는 작업 공간을 에이전트 그룹(gid)과 함께 쓰도록 그룹과 권한을 맞춘다(규칙은 shareRuleFor).
// 기능을 켜기 전에 서버가 만든 파일·디렉터리도 바꾼다. 바꾸지 않으면 기능을 켠 뒤 예전 작업을 이어 할 때 에이전트가
// 자기 작업 공간에 쓰지 못한다.
//
// 에이전트는 작업 공간 안의 항목을 언제든 링크로 바꿀 수 있다. 그래서 경로를 다시 풀지 않는다. 각 항목을 부모
// 디렉터리의 파일 기술자 기준으로 O_NOFOLLOW로 열고, 연 기술자에만 fchown·fchmod한다. 링크, 서버 소유가 아닌 것
// (에이전트가 만든 것), 하드 링크가 여럿인 파일, 일반 파일·디렉터리가 아닌 것은 건드리지 않는다. 걷는 도중
// 사라졌거나 읽을 수 없는 항목은 건너뛴다. 에이전트가 서버 시작을 막지 못하게 하기 위해서다.
func prepareSharedWorkspace(dataDir string, gid int) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := os.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open data dir: %w", err)
	}
	defer data.Close()
	if err := shareRequired(data, gid, ruleDataDir); err != nil {
		return err
	}
	changed := 0
	for _, name := range sharedWorkspaceDirs {
		// 데이터 디렉터리는 에이전트가 쓰지 못하므로 바로 아래 이름은 경로로 만들어도 된다.
		if err := os.Mkdir(filepath.Join(dataDir, name), 0o775); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create workspace dir: %w", err)
		}
		top, err := openNoFollow(data, name)
		if err != nil {
			return fmt.Errorf("open workspace dir %s: %w", name, err)
		}
		err = shareRequired(top, gid, ruleContainer)
		if err == nil {
			err = shareTree(top, name, 1, false, gid, &changed)
		}
		top.Close()
		if err != nil {
			return fmt.Errorf("share workspace %s with group %d: %w", filepath.Join(dataDir, name), gid, err)
		}
	}
	if changed > 0 {
		log.Printf("[sandbox] 작업 공간 %d개 항목의 그룹을 %d로 맞췄다", changed, gid)
	}
	return nil
}

// shareTree 는 dir 바로 아래 항목을 규칙대로 맞추고, 서버 소유 디렉터리면 그 안으로 들어간다. readOnly는 dir이
// 에이전트 읽기 전용 영역 안인지다(shareRuleFor).
func shareTree(dir *os.File, top string, depth int, readOnly bool, gid int, changed *int) error {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir.Name(), err)
	}
	for _, name := range names {
		f, err := openNoFollow(dir, name)
		if err != nil {
			if skippableOpenError(err) {
				continue
			}
			return fmt.Errorf("open %s: %w", filepath.Join(dir.Name(), name), err)
		}
		err = shareEntry(f, top, depth, name, readOnly, gid, changed)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func shareEntry(f *os.File, top string, depth int, name string, readOnly bool, gid int, changed *int) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return nil // 에이전트가 만든 것(다른 uid)은 에이전트가 맡는다
	}
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		if st.Nlink != 1 {
			return nil // 다른 곳의 서버 파일을 하드 링크로 들여왔을 수 있다
		}
	default:
		return nil // FIFO·소켓 등
	}
	rule, childReadOnly := shareRuleFor(top, depth, name, info.IsDir(), readOnly)
	result, err := shareOpened(f, info, gid, rule)
	if err != nil {
		return err
	}
	if result == shareChanged {
		*changed++
	}
	if info.IsDir() {
		return shareTree(f, top, depth+1, childReadOnly, gid, changed)
	}
	return nil
}

// openNoFollow 는 dir 기준으로 name을 링크를 따라가지 않고 연다. O_NONBLOCK은 FIFO에서 멈추지 않게 한다.
func openNoFollow(dir *os.File, name string) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// skippableOpenError 는 걷기를 멈추지 않고 건너뛸 열기 실패다. ELOOP는 링크, ENOENT는 그사이 사라진 항목,
// EACCES는 읽을 수 없는 디렉터리(에이전트가 0700으로 바꾼 것 등), ENXIO는 소켓이다.
func skippableOpenError(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ELOOP, syscall.ENOENT, syscall.EACCES, syscall.ENXIO} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// shareResult 는 shareOpened가 한 일이다.
type shareResult int

const (
	shareSkipped   shareResult = iota // 서버 소유가 아니어서 건너뛰었다
	shareUnchanged                    // 이미 맞았다
	shareChanged                      // 그룹이나 권한을 바꿨다
)

// specialModeBits 는 권한 비교에 넣는 특수 비트다.
const specialModeBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// shareRequired 는 서버 소유여야 하는 디렉터리(데이터 디렉터리, tasks·sessions·drafts)를 규칙대로 맞춘다.
// 서버 소유가 아니면 오류다.
func shareRequired(f *os.File, gid int, rule shareRule) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() || !info.IsDir() {
		return fmt.Errorf("%s is not a directory owned by the server (uid %d); fix its ownership before enabling agent uid separation", f.Name(), os.Getuid())
	}
	if _, err := shareOpened(f, info, gid, rule); err != nil {
		return fmt.Errorf("share %s with group %d: %w", f.Name(), gid, err)
	}
	return nil
}

// shareOpened 는 연 파일 f의 그룹을 gid로 바꾸고 권한에 rule.add를 더하고 rule.remove를 뺀다. 파일의
// setuid·setgid는 지운다. 에이전트 그룹이 실행할 수 있는 서버 소유 setuid 파일은 서버 uid로 가는 길이다.
// 디렉터리의 sticky는 rule.remove에 없으면 남긴다.
func shareOpened(f *os.File, info fs.FileInfo, gid int, rule shareRule) (shareResult, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return shareSkipped, nil
	}
	current := info.Mode() & (fs.ModePerm | specialModeBits)
	want := (info.Mode().Perm() | rule.add) &^ rule.remove
	if info.IsDir() {
		want |= info.Mode() & fs.ModeSticky &^ rule.remove
	} else {
		want &^= specialModeBits
	}
	if int(st.Gid) == gid && current == want {
		return shareUnchanged, nil
	}
	if int(st.Gid) != gid {
		// 서버가 그 그룹에 속하므로 root 없이 바꿀 수 있다(checkServerIdentity가 확인했다).
		if err := f.Chown(-1, gid); err != nil {
			return shareSkipped, err
		}
	}
	// chown은 파일의 setgid 비트를 지울 수 있어 그 뒤에 권한을 맞춘다.
	if err := f.Chmod(want); err != nil {
		return shareSkipped, err
	}
	return shareChanged, nil
}

// installShellDir 는 셸 대체 디렉터리 dir을 확인하고, 없으면 만든다. dir 안의 "bash"는 도우미를 가리키는 링크다.
//
// 이미 있으면(Docker 이미지에 root 소유로 들어 있거나 예전 시작이 만든 것) 고치지 않고 확인만 한다. 디렉터리가
// 링크가 아니고, root나 서버 소유이고, 그룹·다른 사용자 쓰기가 없고, bash가 도우미를 가리키는 링크여야 한다.
// 에이전트가 링크를 바꿔 서버 uid로 자기 코드를 실행시키지 못하게, 에이전트가 쓸 수 있는 곳에 두지 않는다.
func installShellDir(dir, helper string) (string, error) {
	if dir == "" {
		return "", errors.New("no sandbox shell directory configured")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve shell dir: %w", err)
	}
	link := filepath.Join(dir, "bash")
	if err := os.Mkdir(dir, 0o750); err == nil {
		if err := os.Symlink(helper, link); err != nil {
			return "", fmt.Errorf("create shell link: %w", err)
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("create shell dir %s (the server needs write access to the directory of the artex binary once, or create it as root: mkdir -m 0750 %s && ln -s %s %s): %w", dir, dir, helper, link, err)
	}
	if err := checkShellDir(dir, helper); err != nil {
		return "", err
	}
	return dir, nil
}

// checkShellDir 는 installShellDir의 조건을 확인한다.
func checkShellDir(dir, helper string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat shell dir: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.IsDir():
		return fmt.Errorf("shell dir %s is not a directory", dir)
	case !ok || (st.Uid != 0 && int(st.Uid) != os.Getuid()):
		return fmt.Errorf("shell dir %s must be owned by root or the server (uid %d)", dir, os.Getuid())
	case info.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("shell dir %s is writable by group or others (mode %v); chmod 0750 it", dir, info.Mode().Perm())
	}
	link := filepath.Join(dir, "bash")
	linkInfo, err := os.Lstat(link)
	if err != nil {
		return fmt.Errorf("shell dir %s has no bash link: %w", dir, err)
	}
	if linkInfo.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("%s must be a symbolic link to the sandbox helper", link)
	}
	target, err := os.Stat(link)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", link, err)
	}
	helperInfo, err := os.Stat(helper)
	if err != nil {
		return fmt.Errorf("stat sandbox helper: %w", err)
	}
	if !os.SameFile(target, helperInfo) {
		dest, _ := os.Readlink(link) // 오류 문구에만 쓴다
		return fmt.Errorf("%s points to %q, not the sandbox helper %s; remove %s and restart", link, dest, helper, dir)
	}
	return nil
}
