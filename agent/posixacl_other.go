//go:build !linux

package agent

import (
	"errors"
	"os"
)

// 에이전트 uid 분리(#165)는 Linux 전용이라 다른 시스템에서는 ACL을 쓰지 않는다. 빌드만 되게 둔다.

var errACLUnsupported = errors.New("POSIX ACLs through xattrs are supported only on Linux")

// SetACL 은 Linux가 아니면 늘 오류다.
func SetACL(*os.File, ACLKind, ACL) error { return errACLUnsupported }

// GetACL 은 Linux가 아니면 늘 오류다.
func GetACL(*os.File, ACLKind) (ACL, error) { return nil, errACLUnsupported }
