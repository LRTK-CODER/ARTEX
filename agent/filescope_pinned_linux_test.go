package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// sharedFileTools 는 uid 분리를 켠 상태(ShareWritesWithGroup)로 workspace만 쓸 수 있는 파일 도구를 만든다.
func sharedFileTools(t *testing.T, workspace string) map[string]actool.CoreTool {
	t.Helper()
	tools := map[string]actool.CoreTool{}
	for _, tool := range confineFileTools(actool.DefaultTools(), FileScope{Writable: []string{workspace}, ShareWritesWithGroup: true}) {
		tools[tool.Name()] = tool
	}
	return tools
}

// TestPinnedFileToolsBehaveLikeNorma 는 켠 상태에서 파일 도구가 기술자 경로(/proc/self/fd)로 norma를 부르더라도
// 결과 글은 에이전트가 준 경로로 나오고 norma와 같은 일을 하는지 본다.
func TestPinnedFileToolsBehaveLikeNorma(t *testing.T) {
	workspace := t.TempDir()
	tools := sharedFileTools(t, workspace)
	tc := &actool.ToolContext{WorkingDir: workspace}

	steps := []struct {
		name, tool string
		input      map[string]any
		wantError  bool
		wantText   string
	}{
		{"write new file in a new dir", "Write", map[string]any{"file_path": "sub/report.md", "content": "draft one\n"}, false, "Created sub/report.md (10 bytes)"},
		{"overwrite", "Write", map[string]any{"file_path": "sub/report.md", "content": "draft two\n"}, false, "Overwrote sub/report.md (10 bytes)"},
		{"edit", "Edit", map[string]any{"file_path": "sub/report.md", "old_string": "two", "new_string": "three"}, false, "Edited sub/report.md (1 replacement(s))"},
		{"multiedit", "MultiEdit", map[string]any{"file_path": "sub/report.md", "edits": []map[string]any{{"old_string": "draft", "new_string": "final"}}}, false, "Applied 1 edits to sub/report.md"},
		{"read", "Read", map[string]any{"file_path": "sub/report.md"}, false, "final three"},
		{"edit missing string", "Edit", map[string]any{"file_path": "sub/report.md", "old_string": "nope", "new_string": "x"}, true, "old_string not found in sub/report.md"},
		{"read directory", "Read", map[string]any{"file_path": "sub"}, true, "sub is a directory"},
		{"write over directory", "Write", map[string]any{"file_path": "sub", "content": "x"}, true, "sub is an existing directory"},
		{"edit missing file", "Edit", map[string]any{"file_path": "missing.md", "old_string": "a", "new_string": "b"}, true, "missing.md"},
	}
	for _, s := range steps {
		res := callTool(t, tools[s.tool], tc, s.input)
		text := res.Flatten()
		if res.IsError != s.wantError || !strings.Contains(text, s.wantText) {
			t.Errorf("%s: IsError=%v %q, want IsError=%v containing %q", s.name, res.IsError, text, s.wantError, s.wantText)
		}
		if strings.Contains(text, "/proc/self/fd") {
			t.Errorf("%s: result leaks the pinned path: %q", s.name, text)
		}
	}
	data, err := os.ReadFile(filepath.Join(workspace, "sub", "report.md"))
	if err != nil || string(data) != "final three\n" {
		t.Fatalf("file content = %q, %v", data, err)
	}
	assertGroupBits(t, filepath.Join(workspace, "sub", "report.md"), 0o060)

	// 바이너리 안내 줄도 에이전트가 준 경로로 나온다.
	if err := os.WriteFile(filepath.Join(workspace, "blob.bin"), []byte{0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if text := callTool(t, tools["Read"], tc, map[string]any{"file_path": "blob.bin"}).Flatten(); !strings.HasPrefix(text, "[binary file blob.bin ") {
		t.Errorf("binary read = %q", text)
	}
}

// TestPinnedFileToolsRefuseSpecialFiles 는 켠 상태에서 FIFO를 열려 할 때 멈추지 않고 거부하는지 본다. 에이전트는
// 작업 공간에 FIFO를 둘 수 있고, norma가 그 경로를 다시 열면 서버 쪽 도구 호출이 멈춘다. 읽는 쪽이 없는 FIFO를
// 쓰기로 열면 커널이 ENXIO로 거부하고, 읽기로 열면 일반 파일이 아니라서 거부한다. 어느 쪽이든 거부면 된다.
func TestPinnedFileToolsRefuseSpecialFiles(t *testing.T) {
	workspace := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workspace, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools := sharedFileTools(t, workspace)
	tc := &actool.ToolContext{WorkingDir: workspace}
	for _, name := range []string{"Read", "Write", "Edit"} {
		res := callTool(t, tools[name], tc, map[string]any{"file_path": "pipe", "content": "x", "old_string": "a", "new_string": "b"})
		if !res.IsError || !strings.Contains(res.Flatten(), `"pipe"`) {
			t.Errorf("%s on a FIFO = %q", name, res.Flatten())
		}
	}
}

// TestPinnedFileToolsSurviveSwapRace 는 켠 상태에서 에이전트가 검사와 열기 사이에 이름을 밖의 키를 가리키는 링크로
// 바꾸는 경쟁을 흉내 낸다. 한 goroutine이 같은 이름을 일반 파일과 링크로 계속 바꾸는 동안 Read·Write·Edit를
// 되풀이해도 키 내용이 결과에 나오거나 키가 바뀌면 안 된다. 경로로 다시 여는 예전 방식은 이 경쟁에서 진다.
func TestPinnedFileToolsSurviveSwapRace(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "ws")
	keys := filepath.Join(base, "keys")
	for _, d := range []string{workspace, keys} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(keys, "jwt.key")
	mustWrite(t, key, fakeKeyContent)
	name := filepath.Join(workspace, "notes.txt")
	regular := filepath.Join(workspace, ".regular")
	link := filepath.Join(workspace, ".link")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 이름을 원자적으로 바꾼다(rename). 일반 파일은 "FAKE"를 담아 Edit도 매번 맞을 수 있게 한다.
			_ = os.WriteFile(regular, []byte("FAKE notes\n"), 0o644)
			_ = os.Rename(regular, name)
			_ = os.Symlink(key, link)
			_ = os.Rename(link, name)
		}
	}()

	tools := sharedFileTools(t, workspace)
	tc := &actool.ToolContext{WorkingDir: workspace}
	const rounds = 3000
	for i := 0; i < rounds; i++ {
		for _, call := range []struct {
			tool  string
			input map[string]any
		}{
			{"Read", map[string]any{"file_path": "notes.txt"}},
			{"Write", map[string]any{"file_path": "notes.txt", "content": "AGENT-CHOSEN"}},
			{"Edit", map[string]any{"file_path": "notes.txt", "old_string": "FAKE", "new_string": "AGENT"}},
		} {
			res := callTool(t, tools[call.tool], tc, call.input)
			if strings.Contains(res.Flatten(), fakeKeyContent) {
				close(stop)
				wg.Wait()
				t.Fatalf("round %d: %s returned the key through a swapped link: %q", i, call.tool, res.Flatten())
			}
		}
	}
	close(stop)
	wg.Wait()
	data, err := os.ReadFile(key)
	if err != nil || string(data) != fakeKeyContent {
		t.Fatalf("key changed through a swapped link: %q, %v", data, err)
	}
}

// TestPinnedFileToolsCloseDescriptors 는 켠 상태에서 도구 호출이 끝나면 연 기술자(허용 디렉터리 뿌리, 대상 파일)를
// 닫는지 본다. 성공·실패·거부 호출을 섞어 여러 번 부른 뒤 프로세스의 열린 기술자 수가 늘지 않아야 한다.
func TestPinnedFileToolsCloseDescriptors(t *testing.T) {
	workspace := t.TempDir()
	tools := sharedFileTools(t, workspace)
	tc := &actool.ToolContext{WorkingDir: workspace}
	calls := []struct {
		tool  string
		input map[string]any
	}{
		{"Write", map[string]any{"file_path": "a.txt", "content": "alpha"}},
		{"Read", map[string]any{"file_path": "a.txt"}},
		{"Edit", map[string]any{"file_path": "a.txt", "old_string": "alpha", "new_string": "beta"}},
		{"Edit", map[string]any{"file_path": "a.txt", "old_string": "missing", "new_string": "x"}},
		{"Read", map[string]any{"file_path": "missing.txt"}},
		{"Read", map[string]any{"file_path": "/etc/hostname"}},
	}
	callAll := func() {
		for _, c := range calls {
			callTool(t, tools[c.tool], tc, c.input)
		}
	}
	callAll() // 처음 한 번에 생기는 지연 초기화(예: 파일 도구 내부 캐시)를 세지 않는다
	before := openDescriptors(t)
	for i := 0; i < 50; i++ {
		callAll()
	}
	if after := openDescriptors(t); after > before {
		t.Fatalf("open descriptors grew from %d to %d", before, after)
	}
}

func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
