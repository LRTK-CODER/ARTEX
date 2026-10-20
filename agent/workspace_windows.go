package agent

import (
	"errors"
	"os"
)

// 에이전트 uid 분리(#165)는 Linux 전용이라 Windows에서는 작업 공간을 그룹과 나눌 일이 없다. 빌드만 되게 둔다.

var errShareUnsupported = errors.New("sharing the workspace with an agent group is not supported on Windows")

func ensureSharedWorkDir(string) error { return errShareUnsupported }

func ensureServerDir(string) error { return errShareUnsupported }

func addGroupWrite(*os.File) error { return errShareUnsupported }

func pinnedPath(*os.File) (string, error) { return "", errShareUnsupported }
