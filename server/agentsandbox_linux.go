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

// enableKillCapability 는 permitted에 있는 CAP_KILL을 모든 스레드의 effective에 올리고 ambient cap을 비운다.
//
// norma는 시간 초과·TaskStop·shell_close에서 프로세스 그룹에 SIGKILL을 보낸다. 에이전트 프로세스는 다른
// uid라서 CAP_KILL 없이는 EPERM으로 실패해 프로세스가 남는다. Docker 이미지는 서버 실행 파일에 cap_kill+p를,
// systemd는 AmbientCapabilities=CAP_KILL을 준다. effective 비트를 파일에 두지 않는 이유는 cap을 주지 않는
// 환경(cap_drop: ALL)에서 실행 자체가 막히지 않게 하려는 것이다.
// ambient를 비우는 이유: systemd가 준 ambient CAP_KILL이 서버가 띄우는 감싸지 않은 자식(ripgrep 등)에게 새지 않게 한다.
// capset은 스레드 단위라 AllThreadsSyscall로 모든 스레드에 적용한다(cgo로 빌드하면 ENOTSUP).
func enableKillCapability() error {
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("read server capabilities: %w", errno)
	}
	if data[0].permitted&(1<<capKill) == 0 {
		return errors.New("the server has no CAP_KILL, so it cannot stop agent processes running as another uid (Docker image: setcap cap_kill+p on /app/artex and keep KILL in cap_add/cap_drop; systemd: AmbientCapabilities=CAP_KILL)")
	}
	data[0].effective |= 1 << capKill
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

// prepareSharedWorkspace 는 작업 공간 디렉터리를 에이전트 그룹(gid)의 setgid 디렉터리로 맞춘다.
//
// 데이터 디렉터리 자체는 그룹을 gid로 바꾸고 그룹 권한을 실행(통과)만 남긴다. 에이전트가 그 아래 작업 공간에
// 닿아야 하지만, 그룹 쓰기가 남으면 트래픽 저장소 같은 나머지 항목을 지우거나 바꿔치기할 수 있다. 다른 사용자
// 쓰기도 같은 이유로 지운다.
//
// sharedWorkspaceDirs는 그룹 읽기·쓰기와 setgid를 주고, 기능을 켜기 전에 서버가 만든 파일·디렉터리도 같은
// 그룹·권한으로 바꾼다. 바꾸지 않으면 기능을 켠 뒤 예전 작업을 이어 할 때 에이전트가 자기 작업 공간에 쓰지
// 못한다. 에이전트가 만든 것(다른 uid)은 건드리지 않는다.
func prepareSharedWorkspace(dataDir string, gid int) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := shareWithGroup(dataDir, gid, 0o010, 0o062); err != nil {
		return err
	}
	changed := 0
	for _, name := range sharedWorkspaceDirs {
		root := filepath.Join(dataDir, name)
		if err := os.MkdirAll(root, 0o775); err != nil {
			return fmt.Errorf("create workspace dir: %w", err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// 에이전트가 자기 디렉터리를 0700으로 바꿨으면 읽지 못한다. 서버가 바꿀 것도 없으니 건너뛴다.
				if errors.Is(err, fs.ErrPermission) {
					return nil
				}
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil // 링크를 따라가면 작업 공간 밖 파일의 권한을 바꿀 수 있다
			}
			bits := fs.FileMode(0o060)
			if d.IsDir() {
				bits = 0o070 | fs.ModeSetgid
			}
			result, err := shareOwnedWithGroup(path, gid, bits, 0)
			if result == shareChanged {
				changed++
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("share workspace %s with group %d: %w", root, gid, err)
		}
	}
	if changed > 0 {
		log.Printf("[sandbox] 작업 공간 %d개 항목의 그룹을 %d로 맞췄다", changed, gid)
	}
	return nil
}

// shareResult 는 shareOwnedWithGroup이 한 일이다.
type shareResult int

const (
	shareSkipped   shareResult = iota // 서버 소유가 아니거나 링크라서 건너뛰었다
	shareUnchanged                    // 이미 맞았다
	shareChanged                      // 그룹이나 권한을 바꿨다
)

// specialModeBits 는 권한 비교에 넣는 특수 비트다.
const specialModeBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// shareWithGroup 은 서버 소유인 path의 그룹을 gid로 바꾸고 권한에 add를 더하고 remove를 뺀다. 서버 소유가 아니면 오류다.
func shareWithGroup(path string, gid int, add, remove fs.FileMode) error {
	result, err := shareOwnedWithGroup(path, gid, add, remove)
	if err != nil {
		return fmt.Errorf("share %s with group %d: %w", path, gid, err)
	}
	if result == shareSkipped {
		return fmt.Errorf("%s is not owned by the server (uid %d); fix its ownership before enabling agent uid separation", path, os.Getuid())
	}
	return nil
}

// shareOwnedWithGroup 은 path가 서버 소유일 때만 그룹을 gid로 바꾸고 권한에 add를 더하고 remove를 뺀다.
// 링크를 따라가지 않도록 Lstat으로 보고 링크는 건너뛴다.
func shareOwnedWithGroup(path string, gid int, add, remove fs.FileMode) (shareResult, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return shareSkipped, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&fs.ModeSymlink != 0 || int(st.Uid) != os.Getuid() {
		return shareSkipped, nil
	}
	current := info.Mode() & (fs.ModePerm | specialModeBits)
	// 파일의 setuid·setgid는 지운다. 에이전트 그룹이 실행할 수 있는 서버 소유 setuid 파일은 서버 uid로 가는 길이다.
	want := (info.Mode().Perm() | add) &^ remove
	if info.IsDir() {
		want |= info.Mode() & fs.ModeSticky
	}
	if int(st.Gid) == gid && current == want {
		return shareUnchanged, nil
	}
	if int(st.Gid) != gid {
		// 서버가 그 그룹에 속하므로 root 없이 바꿀 수 있다(checkServerIdentity가 확인했다).
		if err := os.Lchown(path, -1, gid); err != nil {
			return shareSkipped, err
		}
	}
	// chown은 파일의 setgid 비트를 지울 수 있어 그 뒤에 권한을 맞춘다.
	if err := os.Chmod(path, want); err != nil {
		return shareSkipped, err
	}
	return shareChanged, nil
}
