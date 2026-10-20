package agent

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// SetACL 은 연 파일 f에 kind ACL을 쓴다. 경로를 다시 풀지 않으므로(fsetxattr) 연 뒤 그 자리가 링크로 바뀌어도
// f 자체에만 쓴다. 링크를 거부하는 것은 여는 쪽의 책임이다. os.Open은 링크를 따라가 대상 파일에 쓰게 되므로
// O_RDONLY|O_NOFOLLOW(디렉터리는 O_DIRECTORY도)로 연다. O_PATH로 연 기술자에는 EBADF로 실패한다.
// 접근 ACL을 쓰면 커널이 모드의 그룹 자리를 mask(없으면 소유 그룹)로 바꾼다. 기본 ACL은
// 디렉터리에만 쓸 수 있다. 소유자가 아니면 EPERM, 파일 시스템이 ACL을 지원하지 않으면 ENOTSUP(EOPNOTSUPP)를 올린다.
func SetACL(f *os.File, kind ACLKind, acl ACL) error {
	data, err := encodeACL(acl)
	if err != nil {
		return err
	}
	name, err := syscall.BytePtrFromString(string(kind))
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := controlFd(f, func(fd uintptr) {
		_, _, errno = syscall.Syscall6(syscall.SYS_FSETXATTR, fd, uintptr(unsafe.Pointer(name)),
			uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0, 0)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return fmt.Errorf("set %s on %s: %w", kind, f.Name(), errno)
	}
	return nil
}

// GetACL 은 연 파일 f의 kind ACL을 읽는다. 그 종류의 ACL이 없으면 ErrNoACL이다(접근 ACL이 없으면 권한은 모드
// 비트뿐이고, 기본 ACL이 없으면 새 항목은 umask를 따른다).
func GetACL(f *os.File, kind ACLKind) (ACL, error) {
	name, err := syscall.BytePtrFromString(string(kind))
	if err != nil {
		return nil, err
	}
	// 크기를 먼저 묻고 읽는다. 그사이 다른 쪽이 ACL을 늘리면 ERANGE라 다시 묻는다.
	for range 3 {
		size, err := fgetxattr(f, name, nil)
		if err != nil {
			return nil, aclReadError(f, kind, err)
		}
		buf := make([]byte, size)
		n, err := fgetxattr(f, name, buf)
		if errors.Is(err, syscall.ERANGE) {
			continue
		}
		if err != nil {
			return nil, aclReadError(f, kind, err)
		}
		return decodeACL(buf[:n])
	}
	return nil, fmt.Errorf("read %s on %s: the ACL kept changing size", kind, f.Name())
}

func aclReadError(f *os.File, kind ACLKind, err error) error {
	if errors.Is(err, syscall.ENODATA) {
		return ErrNoACL
	}
	return fmt.Errorf("read %s on %s: %w", kind, f.Name(), err)
}

// fgetxattr 는 buf가 비면 값의 크기를, 아니면 읽은 바이트 수를 돌려준다.
func fgetxattr(f *os.File, name *byte, buf []byte) (int, error) {
	var ptr unsafe.Pointer
	if len(buf) > 0 {
		ptr = unsafe.Pointer(&buf[0])
	}
	var n uintptr
	var errno syscall.Errno
	if err := controlFd(f, func(fd uintptr) {
		n, _, errno = syscall.Syscall6(syscall.SYS_FGETXATTR, fd, uintptr(unsafe.Pointer(name)), uintptr(ptr), uintptr(len(buf)), 0, 0)
	}); err != nil {
		return 0, err
	}
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

// controlFd 는 f의 기술자로 fn을 부른다. fn이 도는 동안 f가 닫히지 않는다.
func controlFd(f *os.File, fn func(fd uintptr)) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	return conn.Control(fn)
}
