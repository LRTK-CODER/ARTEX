//go:build !windows

// Windows에는 syscall.Stat_t가 없고 에이전트 uid 분리(#165)도 Linux 전용이라 이 파일을 빼고
// workspace_windows.go로 빌드한다. Linux가 아닌 유닉스에서는 SetACL이 오류라 켠 상태의 함수가 실패한다.

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"syscall"
)

// 에이전트 명령이 작업·세션마다 다른 uid로 돌 때(#165, #182) 서버 uid 코드가 작업 공간을 손보는 함수들이다. 규칙은
// workspaceacl.go에 있다. 에이전트는 작업 공간 안의 항목을 링크로 바꿀 수 있으므로, 경로를 다시 풀지 않고
// O_NOFOLLOW로 연 파일 기술자에만 권한을 바꾼다(SetACL은 fsetxattr라 연 기술자에만 쓴다).

// specialModeBits 는 작업 공간 항목에서 빼는 특수 비트다. setgid는 예전 공용 그룹 배치(#175)의 것이고, 파일의
// setuid·setgid는 에이전트가 실행할 수 있는 서버 소유 파일이면 서버 uid로 가는 길이다.
const specialModeBits = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// containerDirKeep 은 공유 디렉터리(데이터 디렉터리, tasks/, sessions/, sessions/cmd-output)에서 남기는 권한이다.
// 소유 그룹과 other는 지나가기(x)만 남긴다. 작업·세션 uid는 1024개라 ACL 항목으로 하나씩 통과를 줄 수 없어(ext4
// ACL은 한 블록에 들어가야 한다) other 지나가기로 준다. 목록은 보지 못하고, 그 아래 작업·세션 디렉터리는 각자
// ACL로 닫혀 있다.
const (
	containerDirRemove fs.FileMode = 0o066
	containerDirAdd    fs.FileMode = 0o001
)

// EnsureContainerDir 는 공유 디렉터리 dir을 만들고(없으면) 소유 그룹·other의 읽기·쓰기를 빼고 other 지나가기를
// 더한다. 특수 비트도 뺀다. 그 자리에 디렉터리가 아닌 것이 있거나 서버 소유가 아니면 오류다. 부모는 에이전트가
// 쓰지 못하는 디렉터리여야 한다.
func EnsureContainerDir(dir string) error {
	if err := os.Mkdir(dir, 0o711); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create directory: %w", err)
	}
	f, info, err := openOwnedDir(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	want := (info.Mode().Perm() &^ containerDirRemove) | containerDirAdd
	if info.Mode()&(fs.ModePerm|specialModeBits) == want {
		return nil
	}
	if err := f.Chmod(want); err != nil {
		return fmt.Errorf("set permissions of %s: %w", dir, err)
	}
	return nil
}

// ShareDir 는 이미 있는 서버 소유 디렉터리 dir을 서버와 에이전트 uid agentUID만 access대로 쓰게 맞춘다
// (ShareOpened). 없으면 fs.ErrNotExist를 잇는 오류다. 그 자리에 링크·파일이 있거나 서버 소유가 아니면 오류다.
func ShareDir(dir string, agentUID int, access WorkspaceAccess) error {
	return ensureACLDir(dir, agentUID, access, false)
}

// ensureACLDir 는 create면 디렉터리 dir을 만들고(없으면), 서버와 agentUID만 access대로 쓰게 맞춘다. create가
// 아니고 dir이 없으면 fs.ErrNotExist를 잇는 오류다.
//
// 그 자리에 디렉터리가 아닌 것(에이전트가 미리 둔 링크·파일)이 있거나 서버 소유가 아니면 쓰지 않고 오류를
// 올린다. 지우고 다시 만들면 지우기와 만들기 사이에 에이전트가 다시 끼워 넣을 수 있어서다.
func ensureACLDir(dir string, agentUID int, access WorkspaceAccess, create bool) error {
	if create {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create working directory: %w", err)
		}
	}
	f, info, err := openOwnedDir(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return ShareOpened(f, info, agentUID, access)
}

// openOwnedDir 는 dir을 링크를 따라가지 않고 열고, 서버 소유 디렉터리인지 확인한다.
//
// O_PATH가 아니라 O_RDONLY로 연다. O_PATH 기술자에는 fsetxattr·fgetxattr가 EBADF로 실패한다.
func openOwnedDir(dir string) (*os.File, fs.FileInfo, error) {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil, fmt.Errorf("%s is not a directory (a symbolic link or file is in its place); refusing to use it as a working directory", dir)
		}
		return nil, nil, fmt.Errorf("open working directory %s: %w", dir, err)
	}
	f := os.NewFile(uintptr(fd), dir)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("stat working directory %s: %w", dir, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not owned by the server (uid %d); refusing to use it as a working directory", dir, os.Getuid())
	}
	return f, info, nil
}

// ShareOpened 는 연 항목 f(info는 그 Stat)를 서버와 에이전트 uid agentUID만 access대로 쓰게 맞춘다. 소유 그룹과
// other의 권한은 없앤다. 디렉터리는 기본 ACL도 써서 안에 새로 생기는 항목이 같은 규칙을 물려받게 한다(서버 전용은
// 기본 ACL을 바꾸지 않는다). 특수 비트(setuid·setgid·sticky)는 뺀다.
//
// 서버 소유의 디렉터리와, 하드 링크가 하나뿐인 서버 소유 일반 파일만 바꾼다. 그 밖(에이전트가 만든 것, 밖의 서버
// 파일을 하드 링크로 들여온 것, FIFO·소켓)은 건드리지 않고 nil이다. f는 링크를 따라가지 않고 연 기술자여야 한다.
//
// 권한 비트는 그대로 두고 특수 비트만 먼저 뺀 뒤 접근 ACL을 한 번에 바꾼다. 같은 작업의 다른 실행이 그 디렉터리
// 안에서 돌고 있어도 권한이 잠깐 비는 순간이 없다.
func ShareOpened(f *os.File, info fs.FileInfo, agentUID int, access WorkspaceAccess) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return nil
	}
	isDir := info.IsDir()
	if !isDir && (!info.Mode().IsRegular() || st.Nlink != 1) {
		return nil
	}
	if info.Mode()&specialModeBits != 0 {
		if err := f.Chmod(info.Mode().Perm()); err != nil {
			return fmt.Errorf("clear special mode bits of %s: %w", f.Name(), err)
		}
	}
	ownerPerm := ACLPerm(info.Mode().Perm()>>6) & aclPermAll
	if isDir {
		ownerPerm = aclPermAll // 서버는 자기 디렉터리를 늘 읽고 쓰고 지나간다
	}
	if err := SetACL(f, ACLAccess, workspaceAccessACL(agentUID, access, ownerPerm)); err != nil {
		return err
	}
	if !isDir {
		return nil
	}
	if def := workspaceDefaultACL(agentUID, os.Getuid(), access); def != nil {
		return SetACL(f, ACLDefault, def)
	}
	return nil
}

// addGroupWrite 는 norma가 0644로 만든 파일 f에 그룹 읽기·쓰기를 더한다. 작업 공간에서는 파일에 기본 ACL이 물려
// 있어 그룹 자리가 mask다. norma가 준 모드(0644) 때문에 mask가 r--라 에이전트 uid가 서버가 쓴 파일을 고치지
// 못하므로, chmod로 mask를 rw-로 올린다. f는 파일 도구가 os.Root 안에서 연 기술자라 경로를 다시 풀지 않는다.
// 서버 소유의 일반 파일이고 하드 링크가 하나일 때만 바꾼다(밖의 파일을 하드 링크로 들여온 것은 건드리지 않는다).
func addGroupWrite(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(st.Uid) != os.Getuid() || st.Nlink != 1 || info.Mode().Perm()&0o060 == 0o060 {
		return nil
	}
	return f.Chmod(info.Mode().Perm() | 0o060)
}

// pinnedPath 는 연 기술자 f를 가리키는 경로다. 이 경로를 열면 커널이 경로를 다시 풀지 않고 f와 같은 파일을 연다
// (Linux /proc의 magic link). 에이전트 uid 분리는 Linux 전용이라 다른 시스템에서는 오류다.
func pinnedPath(f *os.File) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errShareUnsupported
	}
	return "/proc/self/fd/" + strconv.Itoa(int(f.Fd())), nil
}

var errShareUnsupported = errors.New("pinning a file for the file tools needs Linux /proc")
