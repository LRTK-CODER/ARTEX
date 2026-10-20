//go:build !windows

// Windows에는 syscall.Stat_t가 없고 에이전트 uid 분리(#165)도 Linux 전용이라 이 파일을 빼고
// workspace_windows.go로 빌드한다.

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

// 에이전트 명령이 다른 uid로 돌 때(#165) 서버 uid 코드가 작업 공간을 손보는 함수들이다. 에이전트는 작업 공간
// 안의 항목을 링크로 바꿀 수 있으므로, 경로를 다시 풀지 않고 O_NOFOLLOW로 연 파일 기술자에만 권한을 바꾼다.

// 디렉터리 권한 규칙. ensureOwnedDir가 더하고 뺀다.
const (
	sharedWorkDirAdd    = fs.ModeSetgid | 0o070 // 작업 공간: 에이전트 그룹 rwx, 새 항목이 그룹을 물려받게 setgid
	sharedWorkDirRemove = fs.ModeSticky         // sticky는 쓰지 않는다(protected_regular=2에서 서버가 에이전트 파일을 다시 못 쓴다)
	serverDirAdd        = fs.ModeSetgid | 0o050 // 서버 전용: 에이전트 그룹은 읽고 지나가기만
	serverDirRemove     = fs.ModeSticky | 0o022
)

// ensureSharedWorkDir 는 작업 공간 디렉터리 dir을 만들고 에이전트 그룹이 쓸 수 있게 맞춘다. 부모는 에이전트가
// 쓰지 못하는 서버 디렉터리여야 한다(tasks/<tid>, sessions/).
func ensureSharedWorkDir(dir string) error {
	return ensureOwnedDir(dir, sharedWorkDirAdd, sharedWorkDirRemove)
}

// ensureServerDir 는 dir을 만들고 서버만 쓰는 디렉터리로 맞춘다. 예전에 그룹 쓰기로 만들어졌으면 되돌린다.
func ensureServerDir(dir string) error {
	return ensureOwnedDir(dir, serverDirAdd, serverDirRemove)
}

// ensureOwnedDir 는 디렉터리 dir을 만들고 권한에 add를 더하고 remove를 뺀다.
//
// 그 자리에 디렉터리가 아닌 것(에이전트가 미리 둔 링크·파일)이 있거나 서버 소유가 아니면 쓰지 않고 오류를
// 올린다. 지우고 다시 만들면 지우기와 만들기 사이에 에이전트가 다시 끼워 넣을 수 있어서다. 권한은 O_NOFOLLOW로
// 연 기술자로 바꾼다. 부모는 서버가 만든 디렉터리여야 한다.
func ensureOwnedDir(dir string, add, remove fs.FileMode) error {
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create working directory: %w", err)
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return fmt.Errorf("%s is not a directory (a symbolic link or file is in its place); refusing to use it as a working directory", dir)
		}
		return fmt.Errorf("open working directory %s: %w", dir, err)
	}
	f := os.NewFile(uintptr(fd), dir)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat working directory %s: %w", dir, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by the server (uid %d); refusing to use it as a working directory", dir, os.Getuid())
	}
	mask := fs.ModePerm | fs.ModeSetgid | fs.ModeSticky
	want := (info.Mode()&mask | add) &^ remove
	if info.Mode()&mask == want {
		return nil
	}
	if err := f.Chmod(want); err != nil {
		return fmt.Errorf("set permissions of working directory %s: %w", dir, err)
	}
	return nil
}

// addGroupWrite 는 norma가 0644로 만든 파일 f에 그룹 읽기·쓰기를 더한다. 작업 공간은 에이전트 그룹의 setgid
// 디렉터리라서 그룹은 이미 에이전트 그룹이다. f는 파일 도구가 os.Root 안에서 연 기술자라 경로를 다시 풀지 않는다.
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
