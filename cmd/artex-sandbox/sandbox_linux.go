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
	capKill                 = 5
	prSetNoNewPrivs         = 38
	prGetNoNewPrivs         = 39
	prCapAmbient            = 47
	prCapAmbientRaise       = 2
	prCapAmbientClearAll    = 4
)

// goTracebackEnv 는 도우미가 넘기는 환경에서 지운다. 도우미는 파일 cap으로 실행돼(secure-exec) Go 런타임이
// GOTRACEBACK=none을 환경에 넣는다. 그대로 넘기면 서버(launch)와 에이전트가 실행한 Go 프로그램이 패닉 추적을 잃는다.
// 사용자가 처음 준 값은 런타임이 이미 덮어써 되살릴 수 없다.
const goTracebackEnv = "GOTRACEBACK"

// agentUmask 는 에이전트가 만든 파일·디렉터리에서 other 권한을 뺀다(#178). 작업 공간·HOME·TMPDIR는 기본 ACL이 있어
// umask를 쓰지 않고(서버 uid 항목으로 서버가 읽고 지운다), 그 밖(공용 /tmp 등)에 만든 것은 다른 작업 uid가 읽지 못한다.
const agentUmask = 0o007

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
	if inv.mode == modeLaunch {
		return launch(inv)
	}
	id, err := parseIdentity(os.Getenv)
	if err != nil {
		return err
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("stat own executable: %w", err)
	}
	// 기능을 켠 서버의 PATH는 셸 대체 디렉터리 하나뿐이고 원래 PATH는 pathEnv로 온다. 없으면(직접 실행한 경우) PATH를 쓴다.
	path := os.Getenv(pathEnv)
	if path == "" {
		path = os.Getenv("PATH")
	}
	path = stripShellDirs(path, func(dir string) bool {
		info, err := os.Stat(filepath.Join(dir, shellName))
		return err == nil && os.SameFile(info, self)
	})
	if err := os.Setenv("PATH", path); err != nil {
		return fmt.Errorf("set PATH: %w", err)
	}
	// uid·gid는 에이전트 명령에 필요 없다. NMAP_PRIVILEGED=1(Docker 이미지 기본값)은 nmap에 raw 소켓이 있다고
	// 알리는데, 이 셸은 cap이 없고 no_new_privs라 nmap 파일 cap도 무시된다. 남기면 nmap이 SYN 스캔을 기본으로
	// 골라 실패하므로 지워 연결 스캔(-sT)을 기본으로 쓰게 한다.
	for _, name := range []string{uidEnv, gidEnv, pathEnv, "NMAP_PRIVILEGED", goTracebackEnv} {
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("unset %s: %w", name, err)
		}
	}
	home, err := commandHome(os.Getenv(homeEnv), func() string {
		if u, err := user.LookupId(strconv.Itoa(id.uid)); err == nil {
			return u.HomeDir
		}
		return "" // passwd 항목이 없으면 HOME을 그대로 둔다(셸은 그래도 돈다)
	})
	if err != nil {
		return err
	}
	if err := os.Unsetenv(homeEnv); err != nil {
		return fmt.Errorf("unset %s: %w", homeEnv, err)
	}
	if home != "" {
		if err := os.Setenv("HOME", home); err != nil {
			return fmt.Errorf("set HOME: %w", err)
		}
	}

	// capset·prctl은 스레드 단위다. 권한을 버린 스레드에서 그대로 execve 해야 새 프로그램이 그 상태를 물려받는다.
	runtime.LockOSThread()
	if err := dropPrivileges(id); err != nil {
		return err
	}
	state, err := verifyDropped(id)
	if err != nil {
		return err
	}
	if inv.mode == modeCheck {
		// 다시 읽은 값을 찍는다. 서버는 앞의 uid·gid만 비교하지만 진단 출력이 실제와 어긋나지 않게 한다.
		fmt.Printf("uid=%d gid=%d groups=%d capeff=%x no_new_privs=%d\n", state.uid, state.gid, state.groups, state.capEff, state.noNewPrivs)
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

// launch 는 uid·gid를 바꾸지 않고, 파일 cap으로 받은 CAP_KILL 하나만 permitted·inheritable에 남겨 ambient로 올린 뒤
// 명령을 실행한다. 실행된 서버는 CAP_KILL을 ambient로만 받으므로 secure-exec가 아니다(commoncap.c: 새 permitted가
// ambient의 부분집합). SETUID·SETGID는 서버에 넘기지 않는다. no_new_privs는 켜지 않는다. 켜면 서버가 에이전트
// 명령을 띄울 때 이 도우미의 파일 cap이 무시된다. 서버는 시작할 때 ambient·inheritable을 다시 비운다.
func launch(inv invocation) error {
	runtime.LockOSThread()
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("read capabilities: %w", errno)
	}
	if data[0].permitted&(1<<capKill) == 0 {
		return errors.New("launch needs CAP_KILL in the helper's file capabilities (setcap cap_kill,cap_setuid,cap_setgid+ep artex-sandbox) and KILL in the container's capability set")
	}
	data = [2]capData{{permitted: 1 << capKill, inheritable: 1 << capKill}}
	hdr = capHeader{version: linuxCapabilityVersion3}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("keep only CAP_KILL: %w", errno)
	}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientRaise, capKill, 0, 0, 0); errno != 0 {
		return fmt.Errorf("raise ambient CAP_KILL: %w", errno)
	}
	if err := os.Unsetenv(goTracebackEnv); err != nil {
		return fmt.Errorf("unset %s: %w", goTracebackEnv, err)
	}
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
	// bounding 집합은 비우지 않는다(CAP_SETPCAP이 없어 비울 수도 없다). no_new_privs가 켜져 있고 permitted·inheritable·
	// ambient가 0이라 exec로 bounding 안의 cap을 다시 얻을 길이 없다.
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set no_new_privs: %w", errno)
	}
	return nil
}

// droppedState 는 verifyDropped가 다시 읽은 상태다. check 모드가 그대로 찍는다.
type droppedState struct {
	uid, gid, groups int
	capEff           uint64
	noNewPrivs       uintptr
}

// verifyDropped 는 바꾼 상태를 다시 읽어 확인한다. 커널·런타임이 조용히 일부만 적용했을 때 그대로 실행하지 않는다.
func verifyDropped(id identity) (droppedState, error) {
	state := droppedState{uid: syscall.Getuid(), gid: syscall.Getgid()}
	if state.uid != id.uid || syscall.Geteuid() != id.uid {
		return state, fmt.Errorf("uid is %d/%d after setuid %d", state.uid, syscall.Geteuid(), id.uid)
	}
	if state.gid != id.gid || syscall.Getegid() != id.gid {
		return state, fmt.Errorf("gid is %d/%d after setgid %d", state.gid, syscall.Getegid(), id.gid)
	}
	groups, err := syscall.Getgroups()
	if err != nil {
		return state, fmt.Errorf("read supplementary groups: %w", err)
	}
	state.groups = len(groups)
	if len(groups) != 0 {
		return state, fmt.Errorf("supplementary groups remain: %v", groups)
	}
	hdr := capHeader{version: linuxCapabilityVersion3}
	var data [2]capData
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return state, fmt.Errorf("read capabilities: %w", errno)
	}
	state.capEff = uint64(data[1].effective)<<32 | uint64(data[0].effective)
	for _, d := range data {
		if d.effective != 0 || d.permitted != 0 || d.inheritable != 0 {
			return state, errors.New("capabilities remain after dropping")
		}
	}
	nnp, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0, 0, 0, 0)
	if errno != 0 {
		return state, fmt.Errorf("read no_new_privs: %w", errno)
	}
	state.noNewPrivs = nnp
	if nnp != 1 {
		return state, errors.New("no_new_privs is not set")
	}
	return state, nil
}
