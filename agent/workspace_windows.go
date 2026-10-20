package agent

import "errors"

// 에이전트 uid 분리(#165)는 Linux 전용이라 Windows에서는 작업 공간을 그룹과 나눌 일이 없다. 빌드만 되게 둔다.

var errShareUnsupported = errors.New("sharing the workspace with an agent group is not supported on Windows")

func shareRunDir(string, bool) error { return errShareUnsupported }

func addGroupWrite(string) error { return errShareUnsupported }
