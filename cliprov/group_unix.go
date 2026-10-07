//go:build unix

// unix 태그: 프로세스 그룹(Setpgid, kill(-pgid))은 유닉스 계열에만 있다.
// Windows 는 group_windows.go 를 쓴다.

package cliprov

import (
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
