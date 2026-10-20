package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// 파일 도구 경로 제한(#169).
//
// norma의 파일 도구(Read/Write/Edit/MultiEdit/LS/Glob/Grep)는 서버 프로세스 안에서 서버 uid로
// 돌고 절대 경로를 그대로 연다. 그래서 프롬프트 주입을 당한 에이전트가 Bash 없이도 서버 키·바이너리·
// 다른 작업 공간을 읽고 쓸 수 있다. norma는 고치지 않으므로(#163) 여기서 도구를 감싸, 입력 경로를
// 심볼릭 링크까지 푼 실제 경로로 바꾼 뒤 역할별 허용 디렉터리 밖이면 norma에 넘기지 않고 거부한다.
//
// 검사한 실제 경로(링크를 모두 푼 절대 경로)로 입력을 바꿔 norma에 넘기므로 검사한 경로와 연 경로가 같다.
//
// 한계: 검사와 norma의 열기 사이에 에이전트가 (백그라운드 Bash 등으로) 실제 경로의 디렉터리를 링크로
// 바꾸면 빠져나갈 수 있다. 경쟁을 없애려면 os.Root로 파일 도구를 다시 구현해야 하고, 이는 후속으로 둔다.
// 하드 링크도 같은 파일이라 경로 검사로는 가려내지 못한다(#165의 uid 분리가 막는다). 임시 디렉터리는
// 모든 역할이 읽고 쓰므로 세션·작업끼리, 그리고 서버가 그곳에 둔 파일과 공유된다.

// FileScope 는 파일 도구가 닿을 수 있는 디렉터리 목록이다. Writable은 읽기·쓰기, Readable은 읽기만
// 허용한다. 비어 있으면 모든 경로를 거부한다.
type FileScope struct {
	Writable []string
	Readable []string
}

// fileRole 은 파일 도구 허용 경로를 정하는 에이전트 역할이다.
type fileRole string

const (
	fileRoleTask         fileRole = "task"         // planner·mainagent: 작업 디렉터리를 함께 쓴다
	fileRoleIntent       fileRole = "intent"       // worker: 의도마다 자기 디렉터리를 쓴다
	fileRoleConversation fileRole = "conversation" // 대화형(chat·auto·pentest·reporter·retester·사용자 지정)
)

// fileScopeInput 은 역할별 표를 채우는 데 필요한 경로 재료다. DataDir은 서버 데이터 디렉터리다.
type fileScopeInput struct {
	DataDir       string
	TaskID        int64
	IntentID      int64
	SourceTaskIDs []int64 // 상속 원본 작업(직접 원본만)
	SessionDir    string  // 대화형의 세션 작업 공간
}

// fileScopeFor 는 역할별 파일 도구 허용 경로 표다. #165·#166에서 허용 범위를 바꿀 때 이 함수만 고친다.
//
//	역할          쓰기                         읽기만(쓰기 경로에 더해)
//	task          tasks/<tid>, 임시 디렉터리    drafts, tasks/<원본>
//	intent        tasks/<tid>/i<iid>, 임시      tasks/<tid>, drafts, tasks/<원본>
//	conversation  sessions/<sid>, 임시          tasks, drafts
//
// 공통으로 AugmentTools가 보이는 스킬 디렉터리를 읽기에 더한다.
// 읽기 허용의 이유: 작업 생성 전 첨부는 drafts/<id>/uploads에 남고 그 절대 경로가 작업 설명에 실린다.
// 작업 첨부는 tasks/<tid>/uploads라 worker 자기 디렉터리 밖이다. 상속 작업과 발견 사항의 단계 출력에는
// tasks/<원본>/i<iid>/cmd-output 같은 큰 출력 저장 경로가 실린다. reporter·retester·auto는 여러 작업의
// 출력을 읽으므로 대화형은 tasks 전체를 읽는다. 에이전트가 스크립트·중간 파일을 임시 디렉터리에 쓰는
// 일이 흔해 임시 디렉터리는 읽기·쓰기를 둔다(링크를 푼 실제 경로로 판단하므로 그 안의 링크로 밖을 읽지는 못한다).
func fileScopeFor(role fileRole, in fileScopeInput) FileScope {
	tmp := os.TempDir()
	drafts := filepath.Join(in.DataDir, "drafts")
	sources := make([]string, 0, len(in.SourceTaskIDs))
	for _, id := range in.SourceTaskIDs {
		sources = append(sources, runDirPath(in.DataDir, id, 0))
	}
	switch role {
	case fileRoleTask:
		return FileScope{
			Writable: []string{runDirPath(in.DataDir, in.TaskID, 0), tmp},
			Readable: append([]string{drafts}, sources...),
		}
	case fileRoleIntent:
		return FileScope{
			Writable: []string{runDirPath(in.DataDir, in.TaskID, in.IntentID), tmp},
			Readable: append([]string{runDirPath(in.DataDir, in.TaskID, 0), drafts}, sources...),
		}
	case fileRoleConversation:
		return FileScope{
			Writable: []string{in.SessionDir, tmp},
			Readable: []string{filepath.Join(in.DataDir, "tasks"), drafts},
		}
	}
	return FileScope{}
}

// sourceTaskIDs 는 작업의 직접 상속 원본 작업 ID를 돌려준다. 읽지 못하면 원본 디렉터리를 허용하지
// 않는 쪽(더 좁은 쪽)으로 두고 기록만 한다. 에이전트 실행을 멈출 만한 실패가 아니어서다.
func sourceTaskIDs(ts *db.ExplorationStore) []int64 {
	if ts == nil {
		return nil
	}
	sources, err := ts.DirectSourceStores()
	if err != nil {
		log.Printf("[tools] 상속 원본 작업을 읽지 못해 파일 도구 읽기 허용에서 뺀다: %v", err)
		return nil
	}
	ids := make([]int64, 0, len(sources))
	for _, s := range sources {
		ids = append(ids, s.Task.TaskID)
	}
	return ids
}

type fileScopeKey struct{}

// withFileScope 는 이번 실행의 파일 도구 허용 경로를 ctx에 싣는다. AugmentTools가 읽는다.
func withFileScope(ctx context.Context, s FileScope) context.Context {
	return context.WithValue(ctx, fileScopeKey{}, s)
}

func fileScopeFrom(ctx context.Context) FileScope {
	s, _ := ctx.Value(fileScopeKey{}).(FileScope)
	return s
}

// fileToolAccess 는 감쌀 norma 파일 도구와 그 경로 입력의 성질이다.
type fileToolAccess struct {
	isDirTool  bool // LS/Glob/Grep: 경로가 비면 작업 디렉터리, 그것도 비면 "."(norma와 같다)
	isWrite    bool
	checkLinks bool // Grep: 순수 Go 대체 구현이 트리 안 파일 링크를 따라 읽으므로 링크 대상도 본다
}

var fileToolAccesses = map[string]fileToolAccess{
	"Read":      {},
	"Write":     {isWrite: true},
	"Edit":      {isWrite: true},
	"MultiEdit": {isWrite: true},
	"LS":        {isDirTool: true},
	"Glob":      {isDirTool: true},
	"Grep":      {isDirTool: true, checkLinks: true},
}

// confineFileTools 는 tools 가운데 norma 파일 도구를 scope 밖 경로를 거부하는 도구로 감싼다.
// 다른 도구는 그대로 둔다.
func confineFileTools(tools []actool.CoreTool, scope FileScope) []actool.CoreTool {
	for i, t := range tools {
		if access, ok := fileToolAccesses[t.Name()]; ok {
			tools[i] = &confinedFileTool{CoreTool: t, access: access, scope: scope}
		}
	}
	return tools
}

type confinedFileTool struct {
	actool.CoreTool
	access fileToolAccess
	scope  FileScope
}

func (c *confinedFileTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	// norma와 같은 디코더·같은 태그로 읽어야 대소문자가 다른 키("FILE_PATH")나 중복 키도 norma가 쓰는
	// 값과 똑같이 해석된다.
	var paths struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(in, &paths); err != nil {
		return actool.Errorf(fmt.Sprintf("Error: %s input is not a valid object: %v", c.Name(), err)), nil
	}
	raw := paths.FilePath
	if c.access.isDirTool {
		raw = paths.Path
	}
	target, err := realPath(toolTargetPath(tc, raw, c.access.isDirTool))
	if err != nil {
		// 오류 원문에는 링크 대상(허용 밖 실제 경로)이 들어갈 수 있어 싣지 않는다.
		return actool.Errorf(fmt.Sprintf("Error: %s refused: cannot resolve path %q (a parent is not a directory, or a symbolic link on the path is broken).", c.Name(), raw)), nil
	}
	if !c.scope.allows(target, c.access.isWrite) {
		return actool.Errorf(c.scope.refusal(c.Name(), raw, c.access.isWrite)), nil
	}
	if c.access.checkLinks {
		if link, found := c.scope.findEscapingLink(target); found {
			return actool.Errorf(fmt.Sprintf("Error: %s refused: %q contains a symbolic link (%s) that points outside the directories this agent may read. Remove the link or search a narrower path.", c.Name(), raw, link)), nil
		}
	}
	// 검사한 실제 경로를 norma에 넘긴다. 원래 입력을 넘기면 norma·커널이 다른 규칙으로 경로를 풀어
	// 검사하지 않은 파일을 열 수 있다(링크 뒤의 "..").
	rewritten, err := c.withPath(in, target)
	if err != nil {
		return actool.Errorf(fmt.Sprintf("Error: %s refused: cannot pass the checked path for %q: %v", c.Name(), raw, err)), nil
	}
	return c.CoreTool.Call(ctx, rewritten, tc)
}

// withPath 는 입력의 경로 키를 path로 바꾼 입력을 돌려준다. encoding/json은 대소문자가 다른 키도 같은
// 필드로 읽으므로 그런 키를 모두 지우고 정규 키 하나만 남긴다. 바꾼 입력을 norma와 같은 방식으로 다시
// 읽어 path가 나오는지 확인한다. 키 접기 규칙이 어긋나 다른 값이 남으면 넘기지 않는다.
func (c *confinedFileTool) withPath(in json.RawMessage, path string) (json.RawMessage, error) {
	key := "file_path"
	if c.access.isDirTool {
		key = "path"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(in, &fields); err != nil {
		return nil, fmt.Errorf("decode input: %w", err)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	for k := range fields {
		if strings.EqualFold(k, key) {
			delete(fields, k)
		}
	}
	encoded, err := json.Marshal(path)
	if err != nil {
		return nil, fmt.Errorf("encode path: %w", err)
	}
	fields[key] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode input: %w", err)
	}
	var check struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(out, &check); err != nil {
		return nil, fmt.Errorf("decode rewritten input: %w", err)
	}
	got := check.FilePath
	if c.access.isDirTool {
		got = check.Path
	}
	if got != path {
		return nil, errors.New("another key still sets the path")
	}
	return out, nil
}

// toolTargetPath 는 norma가 열 경로를 만든다. 작업 디렉터리·기본 디렉터리 규칙은 norma와 같다
// (tool/helpers.go의 resolvePath, ls.go·search.go의 기본 디렉터리). 다만 상대 경로를 filepath.Join으로
// 정리하지 않고 그대로 잇는다. ".."는 realPath가 링크를 푼 뒤 커널과 같은 순서로 적용한다.
func toolTargetPath(tc *actool.ToolContext, p string, isDirTool bool) string {
	workDir := ""
	if tc != nil {
		workDir = tc.WorkingDir
	}
	if isDirTool && p == "" {
		if workDir == "" {
			return "."
		}
		return workDir
	}
	if filepath.IsAbs(p) || workDir == "" {
		return p
	}
	return workDir + string(filepath.Separator) + p
}

// realPath 는 p를 커널이 여는 것과 같은 실제 경로로 바꾼다. 경로 요소를 앞에서부터 하나씩 보며 링크를
// 만나면 그 자리에서 풀고, ".."는 이미 푼 경로에 적용한다. filepath.Abs·Clean처럼 ".."를 글자 단위로
// 먼저 지우면 "<ws>/dirlink/../x"를 "<ws>/x"로 잘못 보고, 커널은 링크 대상의 상위를 연다.
//
// 아직 없는 요소를 만나면(새로 쓸 파일, 만들 디렉터리) 나머지 이름을 붙인다. 없는 요소는 링크일 수
// 없지만, 그 뒤의 ".."는 이미 있는 다른 링크로 되돌아갈 수 있어 오류로 둔다. 링크가 끊겼으면 오류다.
// 쓰기가 링크를 따라 밖에 파일을 만들 수 있어서다.
func realPath(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("working directory: %w", err)
		}
		p = wd + string(filepath.Separator) + p
	}
	vol := filepath.VolumeName(p)
	resolved := vol + string(filepath.Separator)
	elems := strings.FieldsFunc(p[len(vol):], func(r rune) bool { return os.IsPathSeparator(uint8(r)) })
	for i, elem := range elems {
		switch elem {
		case ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved) // resolved에는 링크가 없으므로 커널과 같다
			continue
		}
		next := filepath.Join(resolved, elem)
		info, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			rest := elems[i:]
			for _, r := range rest {
				if r == ".." {
					return "", fmt.Errorf("%q after a missing directory: %w", "..", fs.ErrNotExist)
				}
			}
			return filepath.Join(append([]string{resolved}, rest...)...), nil
		}
		if err != nil {
			return "", fmt.Errorf("stat: %w", err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// next의 상위는 이미 실제 경로이므로 EvalSymlinks는 링크 대상만 풀면 된다.
			if next, err = filepath.EvalSymlinks(next); err != nil {
				return "", fmt.Errorf("resolve symlinks: %w", err)
			}
		}
		resolved = next
	}
	return resolved, nil
}

// allows 는 실제 경로 target이 허용 디렉터리 안인지 본다. 허용 디렉터리도 실제 경로로 풀어 비교한다
// (macOS의 /tmp → /private/tmp 같은 링크). 아직 없는 허용 디렉터리는 건너뛴다.
func (s FileScope) allows(target string, isWrite bool) bool {
	for _, root := range s.roots(isWrite) {
		if isWithin(root, target) {
			return true
		}
	}
	return false
}

func (s FileScope) roots(isWrite bool) []string {
	dirs := s.Writable
	if !isWrite {
		dirs = append(append([]string{}, s.Writable...), s.Readable...)
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			continue
		}
		out = append(out, resolved)
	}
	return out
}

// isWithin 은 문자열 접두 비교가 아니라 filepath.Rel로 포함 여부를 본다("/a/bc"는 "/a/b" 안이 아니다).
func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// findEscapingLink 는 디렉터리 target 아래에서 읽기 허용 밖의 파일을 가리키는 심볼릭 링크를 찾는다.
// 디렉터리를 가리키는 링크는 Grep이 따라 들어가지 않으므로(ripgrep 기본값, Go WalkDir) 보지 않는다.
// 찾으면 target 기준 상대 경로를 돌려준다.
func (s FileScope) findEscapingLink(target string) (string, bool) {
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return "", false
	}
	var found string
	_ = filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 읽지 못하는 하위 디렉터리는 Grep도 건너뛴다
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil // 끊긴 링크는 Grep이 읽지 못한다
		}
		if st, err := os.Stat(resolved); err == nil && st.IsDir() {
			return nil
		}
		if s.allows(resolved, false) {
			return nil
		}
		found, _ = filepath.Rel(target, p)
		return filepath.SkipAll
	})
	return found, found != ""
}

// refusal 은 에이전트가 이유와 갈 곳을 알 수 있는 거부 메시지를 만든다. 입력 경로만 싣고, 링크를 푼
// 실제 경로나 파일 내용은 싣지 않는다.
func (s FileScope) refusal(tool, raw string, isWrite bool) string {
	verb := "read"
	if isWrite {
		verb = "write"
	}
	return fmt.Sprintf("Error: %s refused: path %q is outside the directories this agent may %s (symbolic links are resolved before checking). Writable: %s. Read-only: %s. Use a path inside these directories.",
		tool, raw, verb, listOrNone(s.Writable), listOrNone(s.Readable))
}

func listOrNone(dirs []string) string {
	var kept []string
	for _, d := range dirs {
		if d != "" {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return "(none)"
	}
	return strings.Join(kept, ", ")
}
