//go:build !linux

package server

// EnableAgentSandbox는 Linux가 아니면 이 함수들을 부르기 전에 ErrAgentSandboxUnsupported로 멈춘다.
// 아래는 빌드만 되게 둔 것이다.

func enableKillCapability() error { return ErrAgentSandboxUnsupported }

func setSharedUmask() {}

func checkWorkspaceACL(string, int, aclSetter) error { return ErrAgentSandboxUnsupported }

func prepareSharedWorkspace(string, int) error { return ErrAgentSandboxUnsupported }

func installShellDir(string, string) (string, error) { return "", ErrAgentSandboxUnsupported }
