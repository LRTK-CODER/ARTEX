//go:build !windows

// Windows에는 O_NOFOLLOW·syscall.Stat_t가 없고 에이전트 uid 분리(#165)도 Linux 전용이라 이 파일을 빼고
// workspace_windows.go로 빌드한다.

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// 에이전트 명령이 다른 uid로 돌 때(#165) 서버 uid 코드가 작업 공간을 손보는 함수들이다. 에이전트는 작업 공간
// 안의 항목을 링크로 바꿀 수 있으므로, 경로를 다시 풀지 않고 O_NOFOLLOW로 연 파일 기술자에만 권한을 바꾼다.

// shareRunDir 는 작업 공간 디렉터리 dir을 만들고 에이전트 그룹이 쓸 수 있게 맞춘다(그룹 rwx, setgid).
// sticky가 참이면 sticky 비트도 준다. tasks/<tid>처럼 다른 작업 공간(i<iid>)을 품은 디렉터리에 쓴다.
// sticky가 있으면 에이전트는 서버가 만든 항목의 이름을 바꾸거나 지우지 못한다.
//
// 그 자리에 디렉터리가 아닌 것(에이전트가 미리 둔 링크·파일)이 있거나 서버 소유가 아니면 쓰지 않고 오류를
// 올린다. 지우고 다시 만들면 지우기와 만들기 사이에 에이전트가 다시 끼워 넣을 수 있어서다. 부모는 서버가 만든
// 디렉터리여야 한다(tasks/, sessions/는 에이전트가 쓰지 못한다).
func shareRunDir(dir string, sticky bool) error {
	if err := os.Mkdir(dir, 0o775); err != nil && !errors.Is(err, fs.ErrExist) {
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
	want := info.Mode().Perm() | 0o070 | fs.ModeSetgid
	if sticky {
		want |= fs.ModeSticky
	}
	if info.Mode()&(fs.ModePerm|fs.ModeSetgid|fs.ModeSticky) == want {
		return nil
	}
	if err := f.Chmod(want); err != nil {
		return fmt.Errorf("share working directory %s with the agent group: %w", dir, err)
	}
	return nil
}

// addGroupWrite 는 norma가 0644로 만든 파일에 그룹 읽기·쓰기를 더한다. 작업 공간은 에이전트 그룹의 setgid
// 디렉터리라서 그룹은 이미 에이전트 그룹이다. 확인과 권한 바꾸기 사이에 에이전트가 파일을 링크로 바꿔도
// 밖의 파일을 바꾸지 않게 O_NOFOLLOW로 열고 그 기술자로 바꾼다. 서버 소유의 일반 파일이고 하드 링크가
// 하나일 때만 바꾼다(밖의 파일을 하드 링크로 들여온 것은 건드리지 않는다).
func addGroupWrite(target string) error {
	fd, err := syscall.Open(target, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), target)
	defer f.Close()
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
