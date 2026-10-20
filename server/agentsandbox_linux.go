package server

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
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
//	tasks/<tid>(할당 있음)                     작업 uid 읽기. work·i<iid>와 그 안은 읽기·쓰기, 나머지(uploads,
//	                                           cmd-output 등)와 그 안은 읽기
//	sessions/conv-<id>(할당 있음)              세션 uid 읽기·쓰기(그 안 포함)
//	sessions/cmd-output                        모든 uid가 지나가기만(agent.EnsureContainerDir)
//	sessions/cmd-output/conv-<id>(할당 있음)   세션 uid 읽기(그 안 포함)
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
			if name == agent.ToolOutputDirName && info.IsDir() {
				// sessions/cmd-output은 모든 세션이 지나가는 공유 디렉터리다. sessions/는 에이전트가 쓰지 못해 경로로 다뤄도 된다.
				if err := agent.EnsureContainerDir(filepath.Join(dataDir, top, name)); err != nil {
					return err
				}
				return eachEntry(f, func(out *os.File, outInfo fs.FileInfo, sid string) error {
					return shareUnit(out, outInfo, owners.sessionUID(sid), agent.WorkspaceRead)
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
