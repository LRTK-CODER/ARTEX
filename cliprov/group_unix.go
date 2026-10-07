//go:build unix

// unix 태그: 프로세스 그룹(Setpgid, kill(-pgid))은 유닉스 계열에만 있다.
// Windows 는 group_windows.go 를 쓴다.

package cliprov

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup 은 자식을 새 프로세스 그룹의 우두머리로 띄운다. 손자도 이 그룹에 든다.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 은 자식의 프로세스 그룹 전체에 SIGKILL 을 보낸다.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// exitedOnItsOwn 은 자식이 신호로 죽지 않고 스스로 끝났는지 알려 준다. 취소·시간 초과는
// 그룹에 SIGKILL 을 보내므로 그때 끝난 자식은 여기서 거짓이다.
func exitedOnItsOwn(state *os.ProcessState) bool {
	return state != nil && state.Exited()
}
