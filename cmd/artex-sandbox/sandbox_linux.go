package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

// Linux capability·prctl 상수. syscall 패키지에 없는 것만 둔다(include/uapi/linux/capability.h, prctl.h).
const (
	linuxCapabilityVersion3 = 0x20080522
	prSetNoNewPrivs         = 38
	prGetNoNewPrivs         = 39
	prCapAmbient            = 47
	prCapAmbientClearAll    = 4
)

// agentUmask 는 에이전트가 만든 파일·디렉터리에 그룹 쓰기를 남긴다. 작업 공간은 서버와 에이전트가 함께 속한
// 그룹의 setgid 디렉터리라서, 서버가 에이전트의 결과를 읽고 고치고 지울 수 있어야 한다.
const agentUmask = 0o002

type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

func run(args []string) error {
	inv, err := parseInvocation(args)
	if err != nil {
		return err
	}
	id, err := parseIdentity(os.Getenv)
	if err != nil {
		return err
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("stat own executable: %w", err)
	}
	path := stripShellDirs(os.Getenv("PATH"), func(dir string) bool {
		info, err := os.Stat(filepath.Join(dir, shellName))
		return err == nil && os.SameFile(info, self)
	})
	if err := os.Setenv("PATH", path); err != nil {
		return fmt.Errorf("set PATH: %w", err)
	}
	// uid·gid는 에이전트 명령에 필요 없다. NMAP_PRIVILEGED=1(Docker 이미지 기본값)은 nmap에 raw 소켓이 있다고
	// 알리는데, 이 셸은 cap이 없고 no_new_privs라 nmap 파일 cap도 무시된다. 남기면 nmap이 SYN 스캔을 기본으로
	// 골라 실패하므로 지워 연결 스캔(-sT)을 기본으로 쓰게 한다.
	for _, name := range []string{uidEnv, gidEnv, "NMAP_PRIVILEGED"} {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("unset %s: %w", name, err)
		}
	}
	// HOME이 서버 사용자의 홈(0700)이면 에이전트 uid는 쓰지 못해 npm·playwright 캐시가 실패한다. 에이전트
	// 사용자의 홈이 passwd에 있으면 그것으로 바꾼다. 없으면 그대로 둔다(셸은 그래도 돈다).
	if u, err := user.LookupId(strconv.Itoa(id.uid)); err == nil && u.HomeDir != "" {
		if err := os.Setenv("HOME", u.HomeDir); err != nil {
			return fmt.Errorf("set HOME: %w", err)
		}
	}

	// capset·prctl은 스레드 단위다. 권한을 버린 스레드에서 그대로 execve 해야 새 프로그램이 그 상태를 물려받는다.
	runtime.LockOSThread()
	if err := dropPrivileges(id); err != nil {
		return err
	}
	if err := verifyDropped(id); err != nil {
		return err
	}
	if inv.mode == modeCheck {
		fmt.Printf("uid=%d gid=%d groups=0 capeff=0 no_new_privs=1\n", id.uid, id.gid)
		return nil
	}
	syscall.Umask(agentUmask)
	// 권한을 버린 뒤 찾는다. 에이전트 uid가 실행할 수 없는 파일을 고르지 않게 하기 위해서다.
	program, err := exec.LookPath(inv.program)
	if err != nil {
		return fmt.Errorf("find %s: %w", inv.program, err)
	}
	if err := syscall.Exec(program, inv.argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", program, err)
	}
	return nil
}

// dropPrivileges 는 보조 그룹을 비우고 gid·uid를 바꾼 뒤 cap을 모두 버리고 no_new_privs를 켠다.
// 그룹을 uid보다 먼저 바꾼다. 서버가 root로 돌면 setuid가 cap을 지워 그 뒤에는 그룹을 바꿀 수 없다.
// cap은 uid를 바꾼 뒤에 버린다. 먼저 버리면 uid를 바꿀 수 없다.
func dropPrivileges(id identity) error {
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("clear supplementary groups: %w", err)
	}
	if err := syscall.Setgid(id.gid); err != nil {
		return fmt.Errorf("setgid %d: %w", id.gid, err)
	}
	if err := syscall.Setuid(id.uid); err != nil {
		return fmt.Errorf("setuid %d: %w", id.uid, err)
	}
	// 0이 아닌 uid에서 0이 아닌 uid로 바꾸면 커널이 cap을 지워 주지 않는다(capabilities(7)). 직접 비운다.
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("drop capabilities: %w", errno)
	}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("clear ambient capabilities: %w", errno)
	}
	// 에이전트가 setuid·파일 cap 실행 파일로 권한을 다시 얻지 못하게 한다. 도우미 자신도 여기에 든다.
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set no_new_privs: %w", errno)
	}
	return nil
}

// verifyDropped 는 바꾼 상태를 다시 읽어 확인한다. 커널·런타임이 조용히 일부만 적용했을 때 그대로 실행하지 않는다.
func verifyDropped(id identity) error {
	if syscall.Getuid() != id.uid || syscall.Geteuid() != id.uid {
		return fmt.Errorf("uid is %d/%d after setuid %d", syscall.Getuid(), syscall.Geteuid(), id.uid)
	}
	if syscall.Getgid() != id.gid || syscall.Getegid() != id.gid {
		return fmt.Errorf("gid is %d/%d after setgid %d", syscall.Getgid(), syscall.Getegid(), id.gid)
	}
	groups, err := syscall.Getgroups()
	if err != nil {
		return fmt.Errorf("read supplementary groups: %w", err)
	}
	if len(groups) != 0 {
		return fmt.Errorf("supplementary groups remain: %v", groups)
	}
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("read capabilities: %w", errno)
	}
	for _, d := range data {
		if d.effective != 0 || d.permitted != 0 || d.inheritable != 0 {
			return errors.New("capabilities remain after dropping")
		}
	}
	nnp, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0, 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("read no_new_privs: %w", errno)
	}
	if nnp != 1 {
		return errors.New("no_new_privs is not set")
	}
	return nil
}
