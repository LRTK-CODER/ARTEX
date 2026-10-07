package cliprov

import (
	"os"
	"os/exec"
)

// setProcessGroup 은 Windows 에서 할 일이 없다. 손자 정리는 Job Object 가 필요한데
// 아직 Windows 에서 CLI provider 를 쓰는 경로가 없어 자식만 끝낸다.
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup 은 Windows 에서 자식만 끝낸다.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// exitedOnItsOwn 은 Windows 에서 늘 거짓이다. Process.Kill(TerminateProcess)로 끝난
// 프로세스도 Exited 가 참이라 스스로 끝났는지 가릴 수 없어, ctx 상태를 먼저 본다.
func exitedOnItsOwn(*os.ProcessState) bool { return false }
