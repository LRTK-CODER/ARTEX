package agent

import (
	"errors"
	"io/fs"
	"os"
)

// 에이전트 uid 분리(#165)는 Linux 전용이라 Windows에서는 작업 공간을 나눌 일이 없다. 빌드만 되게 둔다.

var errShareUnsupported = errors.New("separating workspaces per agent uid is not supported on Windows")

// EnsureContainerDir 는 Windows에서 늘 오류다.
func EnsureContainerDir(string) error { return errShareUnsupported }

// ShareDir 는 Windows에서 늘 오류다.
func ShareDir(string, int, WorkspaceAccess) error { return errShareUnsupported }

// ShareOpened 는 Windows에서 늘 오류다.
func ShareOpened(*os.File, fs.FileInfo, int, WorkspaceAccess) error { return errShareUnsupported }

func ensureACLDir(string, int, WorkspaceAccess, bool) error { return errShareUnsupported }

func addGroupWrite(*os.File) error { return errShareUnsupported }

func pinnedPath(*os.File) (string, error) { return "", errShareUnsupported }
