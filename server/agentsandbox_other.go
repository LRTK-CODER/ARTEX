//go:build !linux

package server

import "context"

// EnableAgentSandbox는 Linux가 아니면 이 함수들을 부르기 전에 ErrAgentSandboxUnsupported로 멈춘다.
// 아래는 빌드만 되게 둔 것이다.

func enableKillCapability() error { return ErrAgentSandboxUnsupported }

func checkWorkspaceACL(string, int, aclSetter) error { return ErrAgentSandboxUnsupported }

func prepareWorkspaceRoots(string) error { return ErrAgentSandboxUnsupported }

func applyWorkspaceACL(string, workspaceOwners) error { return ErrAgentSandboxUnsupported }

func restrictDataDir(string) error { return ErrAgentSandboxUnsupported }

func installShellDir(string, string) (string, error) { return "", ErrAgentSandboxUnsupported }

func shareTaskWorkspace(string, string, int) error { return ErrAgentSandboxUnsupported }

func (s *AgentSandbox) removeAgentOwned(context.Context, string) ([]error, error) {
	return nil, ErrAgentSandboxUnsupported
}

func liveAgentProcesses(int, int) ([]int, error) { return nil, ErrAgentSandboxUnsupported }
