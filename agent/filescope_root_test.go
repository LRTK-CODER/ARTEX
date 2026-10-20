package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// TestFileToolsRejectSwappedWorkspaceRoot 는 데이터 디렉터리 아래의 허용 디렉터리(작업 공간) 경로에 링크가 끼어
// 있으면 파일 도구가 그 링크를 따라 허용 범위를 넓히지 않는지 본다. 에이전트 명령이 다른 uid로 돌면(#165) 에이전트는
// 작업 공간을 키 디렉터리를 가리키는 링크로 바꿔 둘 수 있고, 파일 도구는 서버 uid로 돈다. 링크가 없는 실제
// 디렉터리는 그대로 허용한다(대조군).
func TestFileToolsRejectSwappedWorkspaceRoot(t *testing.T) {
	cases := []struct {
		name string
		role fileRole
		// link는 데이터 디렉터리 기준으로 링크로 바꿀 경로, ws는 작업 디렉터리다.
		link, ws string
		in       func(dataDir string) fileScopeInput
		allowed  bool
	}{
		{
			name: "conversation workspace is a link", role: fileRoleConversation,
			link: "sessions/conv-1", ws: "sessions/conv-1",
			in: func(d string) fileScopeInput {
				return fileScopeInput{DataDir: d, SessionDir: filepath.Join(d, "sessions", "conv-1")}
			},
		},
		{
			name: "intent workspace is a link", role: fileRoleIntent,
			link: "tasks/1/i1", ws: "tasks/1/i1",
			in: func(d string) fileScopeInput { return fileScopeInput{DataDir: d, TaskID: 1, IntentID: 1} },
		},
		{
			name: "task workspace is a link", role: fileRoleTask,
			link: "tasks/1", ws: "tasks/1",
			in: func(d string) fileScopeInput { return fileScopeInput{DataDir: d, TaskID: 1} },
		},
		{
			name: "link above the workspace", role: fileRoleIntent,
			link: "tasks", ws: "tasks/1/i1",
			in: func(d string) fileScopeInput { return fileScopeInput{DataDir: d, TaskID: 1, IntentID: 1} },
		},
		{
			name: "real directory", role: fileRoleIntent,
			ws: "tasks/1/i1", allowed: true,
			in: func(d string) fileScopeInput { return fileScopeInput{DataDir: d, TaskID: 1, IntentID: 1} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			// 임시 디렉터리는 모든 역할이 읽고 쓴다. 밖(키 디렉터리)이 그 아래에 들지 않게 따로 둔다.
			t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
			dataDir := filepath.Join(base, "data")
			keys := filepath.Join(base, "keys")
			ws := filepath.Join(dataDir, filepath.FromSlash(tc.ws))
			if tc.link != "" {
				// 키 디렉터리 안에 링크 뒤의 나머지 경로를 만들어, 링크를 따라가면 그 안의 키에 닿게 한다.
				rest, err := filepath.Rel(filepath.Join(dataDir, filepath.FromSlash(tc.link)), ws)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(keys, rest), 0o755); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(keys, rest, "oauth.key"), fakeKeyContent)
				link := filepath.Join(dataDir, filepath.FromSlash(tc.link))
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(keys, link); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(ws, 0o755); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(ws, "oauth.key"), fakeKeyContent)
			}
			ctx := withFileScope(context.Background(), fileScopeFor(tc.role, tc.in(dataDir)))
			tools, _, cleanup := AugmentTools(ctx, "chat", actool.DefaultTools())
			defer cleanup()
			byName := map[string]actool.CoreTool{}
			for _, tool := range tools {
				byName[tool.Name()] = tool
			}
			toolCtx := &actool.ToolContext{WorkingDir: ws}

			read := callTool(t, byName["Read"], toolCtx, map[string]any{"file_path": "oauth.key"})
			ls := callTool(t, byName["LS"], toolCtx, map[string]any{})
			write := callTool(t, byName["Write"], toolCtx, map[string]any{"file_path": "planted.txt", "content": "x"})
			if tc.allowed {
				if read.IsError || ls.IsError || write.IsError {
					t.Fatalf("real workspace refused: read=%q ls=%q write=%q", read.Flatten(), ls.Flatten(), write.Flatten())
				}
				return
			}
			for name, res := range map[string]actool.Result{"Read": read, "LS": ls, "Write": write} {
				if !res.IsError || strings.Contains(res.Flatten(), fakeKeyContent) {
					t.Errorf("%s through a swapped workspace was not refused: %q", name, res.Flatten())
				}
			}
			if _, err := os.Lstat(filepath.Join(keys, "planted.txt")); err == nil {
				t.Error("Write created a file in the key directory")
			}
			if entries, _ := filepath.Glob(filepath.Join(keys, "*", "planted.txt")); len(entries) > 0 {
				t.Errorf("Write created %v in the key directory", entries)
			}
		})
	}
}
