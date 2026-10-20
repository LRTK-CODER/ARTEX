//go:build !windows

package agent

import (
	"os"
	"path/filepath"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestFileToolWritesShareWithGroup 은 에이전트 uid 분리(#165)를 켰을 때 서버 uid로 도는 Write·Edit가 만든
// 파일에 그룹 쓰기가 붙는지, 끄면 norma가 만든 권한(0644) 그대로인지 본다. 에이전트 명령은 다른 uid라서
// 그룹 쓰기가 없으면 서버가 쓴 파일을 고치지 못한다.
func TestFileToolWritesShareWithGroup(t *testing.T) {
	cases := []struct {
		name      string
		share     bool
		wantGroup os.FileMode
	}{
		{name: "sharing on", share: true, wantGroup: 0o060},
		{name: "sharing off", share: false, wantGroup: 0o040},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			scope := FileScope{Writable: []string{workspace}, ShareWritesWithGroup: tc.share}
			tools := map[string]actool.CoreTool{}
			for _, tool := range confineFileTools(actool.DefaultTools(), scope) {
				tools[tool.Name()] = tool
			}
			toolCtx := &actool.ToolContext{WorkingDir: workspace}

			if res := callTool(t, tools["Write"], toolCtx, map[string]any{"file_path": "report.md", "content": "draft"}); res.IsError {
				t.Fatalf("write: %q", res.Flatten())
			}
			path := filepath.Join(workspace, "report.md")
			assertGroupBits(t, path, tc.wantGroup)

			// Edit는 기존 파일의 권한을 그대로 두므로, 그룹 쓰기를 뺀 파일도 Edit 뒤에는 다시 맞아야 한다.
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			if res := callTool(t, tools["Edit"], toolCtx, map[string]any{"file_path": "report.md", "old_string": "draft", "new_string": "final"}); res.IsError {
				t.Fatalf("edit: %q", res.Flatten())
			}
			assertGroupBits(t, path, tc.wantGroup)
		})
	}
}

// TestFileToolRefusedWriteDoesNotChangeMode 는 거부된 쓰기가 밖의 파일 권한을 바꾸지 않는지 본다.
func TestFileToolRefusedWriteDoesNotChangeMode(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	keyPath := filepath.Join(outside, "oauth.key")
	mustWrite(t, keyPath, fakeKeyContent)
	scope := FileScope{Writable: []string{workspace}, ShareWritesWithGroup: true}
	for _, tool := range confineFileTools(actool.DefaultTools(), scope) {
		if tool.Name() != "Write" {
			continue
		}
		if res := callTool(t, tool, &actool.ToolContext{WorkingDir: workspace}, map[string]any{"file_path": keyPath, "content": "x"}); !res.IsError {
			t.Fatalf("write outside scope was not refused: %q", res.Flatten())
		}
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perm = %o, want 600", perm)
	}
}

func assertGroupBits(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm() & 0o070; got != want {
		t.Fatalf("%s group bits = %o, want %o (mode %o)", filepath.Base(path), got, want, info.Mode().Perm())
	}
}
