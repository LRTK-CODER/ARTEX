package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/traffic"
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

// aclCheckDirPrefix 는 ACL 자기 검사가 데이터 디렉터리에 잠깐 만드는 디렉터리 이름 앞부분이다.
const aclCheckDirPrefix = ".acl-check-"

// checkWorkspaceACL 은 데이터 디렉터리가 작업 공간을 나누는 데 필요한 POSIX ACL을 지원하는지 본다(#182). 데이터
// 디렉터리 안에 빈 디렉터리를 만들어 에이전트 uid 항목이 든 접근 ACL과 기본 ACL을 setACL로 쓰고, 다시 읽어 같은지
// 확인한 뒤 지운다. 지원하지 않거나(일부 NFS·FUSE), uid를 받지 않거나(사용자 이름공간에 없는 uid), 쓴 것과 다르게
// 남으면 원인을 담은 오류다. 조용히 넘어가면 작업 공간이 작업마다 나뉘지 않은 채 돈다.
func checkWorkspaceACL(dataDir string, uid int, setACL aclSetter) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	dir, err := os.MkdirTemp(dataDir, aclCheckDirPrefix)
	if err != nil {
		return fmt.Errorf("create the ACL check directory in %s: %w", dataDir, err)
	}
	defer func() {
		if err := os.Remove(dir); err != nil {
			log.Printf("[sandbox] ACL 자기 검사 디렉터리를 지우지 못했다: %v", err)
		}
	}()
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open the ACL check directory: %w", err)
	}
	defer f.Close()
	// 순서는 커널 순서다. 읽은 값과 그대로 비교한다.
	want := agent.ACL{
		{Tag: agent.ACLUserOwner, Perm: agent.ACLRead | agent.ACLWrite | agent.ACLExecute},
		{Tag: agent.ACLNamedUser, ID: uint32(uid), Perm: agent.ACLRead | agent.ACLWrite | agent.ACLExecute},
		{Tag: agent.ACLGroupOwner},
		{Tag: agent.ACLMask, Perm: agent.ACLRead | agent.ACLWrite | agent.ACLExecute},
		{Tag: agent.ACLOther},
	}
	for _, kind := range []agent.ACLKind{agent.ACLAccess, agent.ACLDefault} {
		if err := setACL(f, kind, want); err != nil {
			return workspaceACLError(dataDir, uid, err)
		}
		got, err := agent.GetACL(f, kind)
		if err != nil {
			return workspaceACLError(dataDir, uid, err)
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("the data directory %s did not keep the POSIX ACL written for agent uid separation (%s: wrote %v, read back %v)", dataDir, kind, want, got)
		}
	}
	return nil
}

// workspaceACLError 는 ACL 자기 검사 실패를 사용자가 고칠 수 있는 문장으로 바꾼다.
func workspaceACLError(dataDir string, uid int, err error) error {
	switch {
	case errors.Is(err, syscall.ENOTSUP):
		return fmt.Errorf("the data directory %s does not support POSIX ACLs, which agent uid separation needs to give each task's workspace only to its own agent uid; put the data directory on a file system with ACL support such as ext4, xfs, btrfs or tmpfs (some NFS, FUSE and network file systems lack it): %w", dataDir, err)
	case errors.Is(err, agent.ErrNoACL):
		return fmt.Errorf("the data directory %s accepted a POSIX ACL but did not keep it; use a file system with ACL support such as ext4, xfs, btrfs or tmpfs: %w", dataDir, err)
	case errors.Is(err, syscall.EINVAL):
		return fmt.Errorf("the data directory %s rejected a POSIX ACL entry for uid %d (is the uid mapped in this user namespace?): %w", dataDir, uid, err)
	}
	return fmt.Errorf("write a POSIX ACL in the data directory %s: %w", dataDir, err)
}

// prepareWorkspaceRoots 는 작업 공간을 담는 공유 디렉터리의 권한을 맞춘다(#182). 데이터 디렉터리와 tasks·sessions는
// 소유 그룹·other가 지나가기만 하고(agent.EnsureContainerDir), drafts(작업 생성 전 첨부)는 서버 전용이다. drafts는
// 파일 도구 Read(서버 uid)로만 읽는다. 예전 공용 그룹 배치(#175)의 그룹 읽기·쓰기와 setgid도 여기서 빠진다.
//
// 데이터 디렉터리 자체가 링크일 수 있어(설정한 -data 경로) 실제 경로로 바꿔 다룬다. 그 아래 이름은 에이전트가
// 쓰지 못하는 데이터 디렉터리 바로 아래라 경로로 다뤄도 된다.
func prepareWorkspaceRoots(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	for _, dir := range []string{resolved, filepath.Join(resolved, "tasks"), filepath.Join(resolved, "sessions")} {
		if err := agent.EnsureContainerDir(dir); err != nil {
			return fmt.Errorf("prepare %s for agent uid separation: %w", dir, err)
		}
	}
	drafts := filepath.Join(resolved, "drafts")
	if err := os.Mkdir(drafts, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create drafts dir: %w", err)
	}
	if err := agent.ShareDir(drafts, 0, agent.WorkspaceServerOnly); err != nil {
		return fmt.Errorf("make %s server-only: %w", drafts, err)
	}
	return nil
}

// applyWorkspaceACL 은 tasks·sessions 아래 서버 소유 항목 전체에 할당 표(owners)대로 ACL을 다시 건다(#182). 시작할
// 때 한 번 돈다. 실행 때 ensureRunDir·ensureSessionDir는 작업 공간 디렉터리만 맞추므로, 그 안에 이미 있는 서버 소유
// 파일(파일 도구가 쓴 것, 예전 배치에서 공용 그룹으로 쓰던 것)과, 할당이 없는(지운 대화 등) 디렉터리는 여기서 맞춘다.
//
//	tasks/<tid>(할당 있음)                     작업 uid 읽기. work·i<iid>·home·tmp와 그 안은 읽기·쓰기, 나머지
//	                                           (uploads, cmd-output 등)와 그 안은 읽기
//	sessions/conv-<id>(할당 있음)              세션 uid 읽기·쓰기(그 안 포함)
//	sessions/cmd-output·home·tmp               모든 uid가 지나가기만(agent.EnsureContainerDir)
//	sessions/cmd-output/conv-<id>(할당 있음)   세션 uid 읽기(그 안 포함)
//	sessions/home·tmp/conv-<id>(할당 있음)     세션 uid 읽기·쓰기(그 안 포함). 명령의 HOME·TMPDIR(#178)
//	그 밖(할당 없음, 모르는 이름, 시험 작업 공간)  서버 전용. 안으로 들어가지 않는다
//
// 에이전트는 작업 공간 안의 항목을 언제든 링크로 바꿀 수 있어 경로를 다시 풀지 않는다. 각 항목을 부모 디렉터리의 파일
// 기술자 기준으로 O_NOFOLLOW로 열고 그 기술자에만 쓴다. 에이전트가 만든 것(다른 uid), 하드 링크가 여럿인 파일, 링크,
// 걷는 도중 사라졌거나 읽을 수 없는 항목은 건너뛴다(agent.ShareOpened). 에이전트가 서버 시작을 막지 못하게 하기
// 위해서다. 예전 공용 uid(#175)가 만든 파일은 서버에 CAP_CHOWN이 없어 바꾸지 못한다. 새 작업 uid는 other 권한으로
// 그 파일을 읽기만 한다(README "에이전트 uid 분리").
func applyWorkspaceACL(dataDir string, owners workspaceOwners) error {
	data, err := os.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open data dir: %w", err)
	}
	defer data.Close()
	for _, top := range []string{"tasks", "sessions"} {
		dir, err := openNoFollow(data, top)
		if err != nil {
			return fmt.Errorf("open %s: %w", filepath.Join(dataDir, top), err)
		}
		err = eachEntry(dir, func(f *os.File, info fs.FileInfo, name string) error {
			if top == "tasks" {
				return shareTaskUnit(f, info, owners.taskUID(name))
			}
			if access, ok := sessionSideDirs[name]; ok && info.IsDir() {
				// sessions/cmd-output·home·tmp는 모든 세션이 지나가는 공유 디렉터리다. sessions/는 에이전트가 쓰지 못해 경로로 다뤄도 된다.
				if err := agent.EnsureContainerDir(filepath.Join(dataDir, top, name)); err != nil {
					return err
				}
				return eachEntry(f, func(unit *os.File, unitInfo fs.FileInfo, sid string) error {
					return shareUnit(unit, unitInfo, owners.sessionUID(sid), access)
				})
			}
			return shareUnit(f, info, owners.sessionUID(name), agent.WorkspaceWrite)
		})
		dir.Close()
		if err != nil {
			return fmt.Errorf("apply agent uid ACLs under %s: %w", filepath.Join(dataDir, top), err)
		}
	}
	return nil
}

// restrictDataDir 은 데이터 디렉터리에서 작업 공간이 아닌 항목을 서버 전용으로 닫는다(#186). 데이터 디렉터리는 모든
// uid가 지나가는 공유 디렉터리라(prepareWorkspaceRoots), 그 바로 아래 항목이 umask대로 0755·0644면 아무 작업의
// 에이전트 uid나 이름으로 들어가 트래픽 저장소·보관 묶음·대화 기록을 읽는다.
//
//	tasks, sessions, drafts             손대지 않는다(prepareWorkspaceRoots·applyWorkspaceACL이 맞춘다)
//	traffic, traffic/_ca                모든 uid가 지나가기만(agent.EnsureContainerDir)
//	traffic/_ca/mitmproxy-ca-cert.pem   모든 uid가 읽기(0644). 에이전트 명령이 믿을 프록시 CA 인증서다(SSL_CERT_FILE)
//	그 밖(트래픽 색인·호스트, 보관 묶음 등)  서버 전용(디렉터리 0700, 파일 0600). 안쪽 전체를 맞춘다
//
// 데이터 디렉터리와 위 디렉터리에는 기본 ACL(agent.ServerOnlyDefaultACL)을 걸어, 서버가 나중에 새로 만드는 항목도
// umask와 상관없이 서버 전용이 되게 한다. 이 디렉터리들은 에이전트가 쓰지 못해 경로로 다뤄도 되지만, 걷기는
// applyWorkspaceACL과 같이 링크를 따라가지 않고 연 기술자에 쓴다. 서버 소유가 아닌 것(예전 root 컨테이너가 만든 것)은
// 바꿀 수 없어 건너뛴다.
func restrictDataDir(dataDir string) error {
	data, err := os.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open data dir: %w", err)
	}
	defer data.Close()
	if err := setServerOnlyDefault(data); err != nil {
		return err
	}
	return eachEntry(data, func(f *os.File, info fs.FileInfo, name string) error {
		switch {
		case workspaceRootNames[name]:
			return nil
		case name == trafficDirName && info.IsDir():
			return restrictTrafficDir(filepath.Join(dataDir, name), f)
		}
		return restrictTree(f, info)
	})
}

// workspaceRootNames 는 데이터 디렉터리 바로 아래에서 작업 공간을 담는 디렉터리 이름이다.
var workspaceRootNames = map[string]bool{"tasks": true, "sessions": true, "drafts": true}

// restrictTrafficDir 은 traffic(dir, f는 연 기술자)을 맞춘다. 에이전트 uid는 CA 인증서까지 지나가기만 하고, 나머지는
// 서버 전용이다.
func restrictTrafficDir(dir string, f *os.File) error {
	if err := ensureServerOnlyContainer(dir, f); err != nil {
		return err
	}
	return eachEntry(f, func(child *os.File, info fs.FileInfo, name string) error {
		if name != traffic.CADirName || !info.IsDir() {
			return restrictTree(child, info)
		}
		if err := ensureServerOnlyContainer(filepath.Join(dir, name), child); err != nil {
			return err
		}
		return eachEntry(child, func(file *os.File, fileInfo fs.FileInfo, fileName string) error {
			if fileName == traffic.CACertFileName && fileInfo.Mode().IsRegular() && isServerOwned(fileInfo) {
				return file.Chmod(0o644)
			}
			return restrictTree(file, fileInfo)
		})
	})
}

// ensureServerOnlyContainer 는 dir(f는 연 기술자)을 모든 uid가 지나가기만 하는 디렉터리로 두고, 안에 새로 생기는
// 항목은 서버 전용이 되게 기본 ACL을 건다.
func ensureServerOnlyContainer(dir string, f *os.File) error {
	if err := agent.EnsureContainerDir(dir); err != nil {
		return fmt.Errorf("prepare %s for agent uid separation: %w", dir, err)
	}
	return setServerOnlyDefault(f)
}

// restrictTree 는 f와, f가 서버 소유 디렉터리면 그 안 전체를 서버 전용으로 맞춘다.
func restrictTree(f *os.File, info fs.FileInfo) error {
	if err := agent.ShareOpened(f, info, 0, agent.WorkspaceServerOnly); err != nil {
		return err
	}
	if !info.IsDir() || !isServerOwned(info) {
		return nil
	}
	if err := setServerOnlyDefault(f); err != nil {
		return err
	}
	return eachEntry(f, func(child *os.File, childInfo fs.FileInfo, _ string) error {
		return restrictTree(child, childInfo)
	})
}

// setServerOnlyDefault 는 디렉터리 f에 서버 전용 기본 ACL을 건다.
func setServerOnlyDefault(f *os.File) error {
	if err := agent.SetACL(f, agent.ACLDefault, agent.ServerOnlyDefaultACL()); err != nil {
		return fmt.Errorf("set the server-only default ACL on %s: %w", f.Name(), err)
	}
	return nil
}

// sessionSideDirs 는 sessions/ 아래에서 세션마다 하위 디렉터리(<이름>/conv-<id>)를 두는 공유 디렉터리와, 그 세션
// uid가 하위 디렉터리에서 갖는 권한이다. 큰 출력은 서버만 쓰고, 명령의 HOME·TMPDIR는 세션 uid가 쓴다.
var sessionSideDirs = map[string]agent.WorkspaceAccess{
	agent.ToolOutputDirName:  agent.WorkspaceRead,
	agent.CommandHomeDirName: agent.WorkspaceWrite,
	agent.CommandTmpDirName:  agent.WorkspaceWrite,
}

// shareTaskUnit 은 tasks/<tid>를 맞춘다. 할당이 없으면 서버 전용으로 닫고 안으로 들어가지 않는다.
func shareTaskUnit(f *os.File, info fs.FileInfo, uid int) error {
	if uid == 0 || !info.IsDir() {
		return agent.ShareOpened(f, info, 0, agent.WorkspaceServerOnly)
	}
	if err := agent.ShareOpened(f, info, uid, agent.WorkspaceRead); err != nil {
		return err
	}
	if !isServerOwned(info) {
		return nil
	}
	return eachEntry(f, func(child *os.File, childInfo fs.FileInfo, name string) error {
		access := agent.WorkspaceRead
		if childInfo.IsDir() && agent.IsTaskWorkspaceName(name) {
			access = agent.WorkspaceWrite
		}
		return shareTree(child, childInfo, uid, access)
	})
}

// shareUnit 은 작업 단위 디렉터리(sessions/conv-<id>, sessions/cmd-output/conv-<id>) 하나를 맞춘다. 할당이 없으면
// 서버 전용으로 닫고 안으로 들어가지 않는다.
func shareUnit(f *os.File, info fs.FileInfo, uid int, access agent.WorkspaceAccess) error {
	if uid == 0 || !info.IsDir() {
		return agent.ShareOpened(f, info, 0, agent.WorkspaceServerOnly)
	}
	return shareTree(f, info, uid, access)
}

// shareTree 는 f와, f가 서버 소유 디렉터리면 그 안 전체를 access로 맞춘다. 에이전트가 만든 디렉터리 안으로는
// 들어가지 않는다(그 안은 에이전트가 맡는다).
func shareTree(f *os.File, info fs.FileInfo, uid int, access agent.WorkspaceAccess) error {
	if err := agent.ShareOpened(f, info, uid, access); err != nil {
		return err
	}
	if !info.IsDir() || !isServerOwned(info) {
		return nil
	}
	return eachEntry(f, func(child *os.File, childInfo fs.FileInfo, _ string) error {
		return shareTree(child, childInfo, uid, access)
	})
}

// eachEntry 는 dir 바로 아래 항목마다 링크를 따라가지 않고 열어 fn을 부른다. 열 수 없는 항목(skippableOpenError)은
// 건너뛴다.
func eachEntry(dir *os.File, fn func(f *os.File, info fs.FileInfo, name string) error) error {
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
		info, err := f.Stat()
		if err == nil {
			err = fn(f, info, name)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// isServerOwned 는 info가 서버 uid 소유인지다.
func isServerOwned(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
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

// shareTaskWorkspace 는 tasks/<taskID> 하나를 작업 uid에 맞춘다. applyWorkspaceACL이 시작할 때 작업마다 하는 일과 같다
// (shareTaskUnit). 작업 공간이 없으면(보관 패키지에 파일이 없던 작업) 아무것도 하지 않는다.
func shareTaskWorkspace(dataDir, taskID string, uid int) error {
	data, err := os.Open(dataDir)
	if err != nil {
		return fmt.Errorf("open data dir: %w", err)
	}
	defer data.Close()
	tasks, err := openNoFollow(data, "tasks")
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Join(dataDir, "tasks"), err)
	}
	defer tasks.Close()
	f, err := openNoFollow(tasks, taskID)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Join(tasks.Name(), taskID), err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return shareTaskUnit(f, info, uid)
}

// oPath 는 open(2)의 O_PATH다(include/uapi/asm-generic/fcntl.h). syscall 패키지에 없다. 릴리스 대상(amd64, arm64)에서
// 같은 값이다. 읽기 권한이 없는 디렉터리(에이전트가 chmod 000으로 바꾼 것)도 열어 fstat할 수 있다.
const oPath = 0x200000

// agentDirVisitor 는 walkAgentOwnedDirs가 서버 소유 디렉터리(dir, 연 fd) 하나에서 찾은, 한 uid 소유의 하위 디렉터리
// 이름들을 받는다.
type agentDirVisitor func(dir *os.File, uid int, names []string)

// isAgentOwnedDir 는 RemoveAll이 쓰는 walkAgentOwnedDirs의 판단이다. 서버 uid 소유가 아니면 에이전트(다른 uid) 소유다.
func isAgentOwnedDir(st *syscall.Stat_t) bool {
	return int(st.Uid) != os.Getuid()
}

// removeAgentOwned 는 path 아래의 에이전트 소유 디렉터리를 소유 uid마다 도우미로 지운다(RemoveAll). 도우미가 보고한
// 오류는 모아 돌려주고, 걷기 오류는 따로 돌려준다.
func (s *AgentSandbox) removeAgentOwned(ctx context.Context, path string) ([]error, error) {
	var helperErrs []error
	err := walkAgentOwnedDirs(path, isAgentOwnedDir, func(dir *os.File, uid int, names []string) {
		if err := s.removeAsOwner(ctx, dir, uid, names); err != nil {
			helperErrs = append(helperErrs, err)
		}
	})
	return helperErrs, err
}

// walkAgentOwnedDirs 는 root 아래에서 에이전트 소유(isAgentOwned)인 디렉터리 가운데 가장 위의 것들을, 바로 위 서버
// 소유 디렉터리마다 소유 uid별로 묶어 visit에 넘긴다(AgentSandbox.RemoveAll). 에이전트 소유 디렉터리 안으로는 들어가지
// 않는다(그 안은 소유 uid가 지운다).
//
// 경로를 다시 풀지 않는다(#179). root를 링크를 따라가지 않고 연 뒤, 항목마다 부모 fd 기준으로 O_PATH|O_NOFOLLOW로
// 열고 그 fd를 fstat해 소유자와 유형을 본다. 서버 소유 디렉터리는 그 fd에서 다시 열어(openat ".") 내려간다. 그래서 걷는
// 도중 이름이 링크나 다른 디렉터리로 바뀌어도 root 밖으로 내려가지 않고, 링크는 건너뛴다. visit에 넘기는 dir도 이렇게
// 연 fd다. 열기 전에 사라진 항목은 건너뛴다. 남은 것은 호출자가 다시 지울 때 오류로 보인다. 같은 디렉터리의 이름은
// 정렬한 순서로 보고, 서버 소유 하위 디렉터리를 모두 걸은 뒤에 그 디렉터리의 묶음을 visit에 넘긴다.
func walkAgentOwnedDirs(root string, isAgentOwned func(*syscall.Stat_t) bool, visit agentDirVisitor) error {
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: root, Err: err}
	}
	dir := os.NewFile(uintptr(fd), root)
	defer dir.Close()
	return walkAgentOwnedIn(dir, isAgentOwned, visit)
}

func walkAgentOwnedIn(dir *os.File, isAgentOwned func(*syscall.Stat_t) bool, visit agentDirVisitor) error {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir.Name(), err)
	}
	slices.Sort(names)
	namesByUID := map[int][]string{}
	var uids []int
	for _, name := range names {
		pathFD, err := syscall.Openat(int(dir.Fd()), name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open %s: %w", filepath.Join(dir.Name(), name), err)
		}
		var st syscall.Stat_t
		err = syscall.Fstat(pathFD, &st)
		if err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			syscall.Close(pathFD)
			if err != nil {
				return fmt.Errorf("stat %s: %w", filepath.Join(dir.Name(), name), err)
			}
			continue // 파일·링크는 서버가 부모에서 지운다
		}
		if isAgentOwned(&st) {
			syscall.Close(pathFD)
			uid := int(st.Uid)
			if _, seen := namesByUID[uid]; !seen {
				uids = append(uids, uid)
			}
			namesByUID[uid] = append(namesByUID[uid], name)
			continue
		}
		// O_PATH fd에서 "."을 다시 열면 이름을 거치지 않고 방금 fstat한 그 디렉터리가 열린다. 바로 내려가 닫으므로
		// 열린 fd는 깊이만큼만 쌓인다.
		childFD, err := syscall.Openat(pathFD, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		syscall.Close(pathFD)
		if err != nil {
			return fmt.Errorf("open %s: %w", filepath.Join(dir.Name(), name), err)
		}
		child := os.NewFile(uintptr(childFD), filepath.Join(dir.Name(), name))
		err = walkAgentOwnedIn(child, isAgentOwned, visit)
		child.Close()
		if err != nil {
			return err
		}
	}
	for _, uid := range uids {
		visit(dir, uid, namesByUID[uid])
	}
	return nil
}

// liveAgentProcesses 는 /proc에서 real·effective·saved·filesystem uid 가운데 하나라도 uid인 살아 있는 프로세스의 pid를
// 돌려준다(AgentSandbox.StopProcesses). probe는 그 uid로 돌고 있는 확인용 프로세스(도우미)다. 결과에서 빼고, 보이지
// 않으면 오류다. /proc가 다른 uid의 프로세스를 숨기면(hidepid) 남은 프로세스를 찾지 못해 끝났다고 잘못 판단하기 때문이다.
// 읽는 사이 끝난 프로세스는 건너뛴다.
func liveAgentProcesses(uid, probe int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var alive []int
	sawProbe := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue // 그사이 끝났거나(ENOENT, ESRCH) 읽을 수 없다. 읽을 수 없는 경우는 probe가 보이지 않는 것으로 드러난다
		}
		st, ok := parseProcStatus(data)
		if !ok || !slices.Contains(st.uids, uid) {
			continue
		}
		if pid == probe {
			sawProbe = true
			continue
		}
		if st.isAlive() {
			alive = append(alive, pid)
		}
	}
	if !sawProbe {
		return nil, fmt.Errorf("cannot see processes of agent uid %d in /proc (is /proc mounted with hidepid, or does the server run with systemd ProtectProc=?)", uid)
	}
	return alive, nil
}
