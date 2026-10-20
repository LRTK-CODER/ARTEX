package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
)

// 작업 공간 파일 관리자다. 에이전트가 산출물을 쓰는 데이터 디렉터리(s.m.dir)를 둘러보고, 보고, 고치고,
// 내려받고, 올리고, 지운다. 모든 경로는 requireAuth 뒤에 있다(Handler()).
// 에이전트가 작업 공간에 만든 심볼릭 링크는 신뢰하지 않는다. 경로는 wsResolve가 링크를 푼 실제 경로로
// 판정하고, 실제 파일 연산은 요청마다 연 작업 단위(wsUnit) 루트의 os.Root 로만 한다. 판정 뒤 링크를 바꿔도
// os.Root가 커널 수준에서 단위 밖으로 나가는 경로를 막는다.
//
// 하드 링크는 막지 못한다. 서버 사용자 소유의 밖 파일을 같은 파일 시스템 안에서 작업 공간으로 하드 링크하면
// 경로 판정과 os.Root 모두 그 파일을 작업 공간 안의 정상 파일로 보므로 읽고 덮어쓸 수 있다. 링크 수(Nlink)로
// 거절하지 않는 이유는 패키지 관리자·백업 도구가 만든 정상 파일도 링크 수가 1보다 클 수 있어서다. 서버 키는
// 작업 공간 밖 디렉터리나 별도 볼륨에 두고(#53), 에이전트가 링크를 만드는 것 자체는 명령 실행 샌드박스가 막는다.

const (
	maxWorkspaceRead   = 2 << 20   // 2 MiB: files bigger than this aren't inlined for view/edit (download instead)
	maxWorkspaceUpload = 512 << 20 // 512 MiB per upload request
)

// wsUnitParents 는 에이전트가 쓰는 작업 단위 디렉터리(tasks/<tid>, sessions/<sid>, drafts/<id>)를 담는 최상위
// 디렉터리와, 그 아래 단위 이름이 서버가 만드는 형식인지 보는 함수다. 이 단위 안의 경로를 다루는 요청은 그 단위
// 디렉터리를 루트로 연다(#176). 데이터 디렉터리 전체를 루트로 열면, 에이전트가 자기 작업 공간에 만든 링크를 따라
// 다른 작업·세션 파일이나 대화 기록(transcripts)을 보여 주거나 덮어쓰게 된다. 링크가 같은 단위 안을 가리키면 그대로
// 따라간다.
//
// 단위 디렉터리와 그 부모(tasks/, sessions/, drafts/)는 에이전트가 바꾸지 못한다. 에이전트 uid 분리(#165)를 켜면
// 커널 권한이 막고, 끄면 에이전트가 서버와 같은 uid라 Bash로도 같은 파일에 닿는다. 그래서 단위 디렉터리를 연 뒤에는
// os.Root가 그 안에 묶는다.
var wsUnitParents = map[string]func(name string) bool{
	"tasks":    isCanonicalPositiveID,
	"sessions": isSessionUnitName,
	"drafts":   safeChatID.MatchString, // 첨부 업로드(chatUpload)가 받는 id 형식이다
}

// isCanonicalPositiveID 는 name이 앞에 0이 없는 양의 십진수(작업 id)인지 본다.
func isCanonicalPositiveID(name string) bool {
	id, err := strconv.ParseInt(name, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == name
}

// isSessionUnitName 은 name이 sessions/ 아래에 서버가 만드는 이름(대화 세션 conv-<id>, 큰 출력 cmd-output)인지 본다.
func isSessionUnitName(name string) bool {
	if name == agent.ToolOutputDirName {
		return true
	}
	id, ok := strings.CutPrefix(name, "conv-")
	return ok && isCanonicalPositiveID(id)
}

// wsPath 는 작업 공간 안으로 판정한 경로다.
type wsPath struct {
	// abs 는 링크를 풀기 전 절대 경로다. 응답의 상대 경로(wsRel)가 사용자가 본 이름을 유지하게 한다.
	abs string
	// unit 은 요청을 가둘 작업 단위 디렉터리의 데이터 디렉터리 기준 상대 경로다. 비면 데이터 디렉터리 전체다.
	unit string
	// name 은 링크를 푼 실제 경로의 단위 루트 기준 상대 이름이다. os.Root 메서드에 넘긴다.
	// os.Root는 절대 경로 링크를 따라가지 않으므로, 안을 가리키는 절대 링크도 풀어 둔 이름으로 연다.
	name string
}

// wsUnit 은 abs가 작업 단위 디렉터리 안(단위 자신은 빼고)이면 그 단위의 상대 경로를, 아니면 ""를 돌려준다.
// 링크를 풀지 않은 글자 그대로의 경로로 정한다. 사용자가 고른 위치가 어느 단위인지가 기준이어서다.
//
// 단위는 첫 요소가 wsUnitParents의 이름과 글자 그대로 같고 둘째 요소가 그 형식에 맞을 때만 정한다. 첫 요소가 단위
// 부모와 글자는 다른데 같은 디렉터리일 수 있으면(대소문자, 유니코드 대소문자 접기, 정규화) ok=false다. 대소문자를
// 무시하는 파일 시스템(macOS, Docker Desktop의 바인드 마운트)에서 TASKS/1/...이 tasks/1/...과 같은 파일을 가리키는데
// 단위 밖으로 보면 데이터 디렉터리 전체 루트로 열려 링크를 따라가기 때문이다. 단위 부모 아래 둘째 요소가 형식에
// 맞지 않아도 ok=false다.
func (s *Server) wsUnit(abs string) (string, bool) {
	data := filepath.Clean(s.m.dir)
	rel, err := filepath.Rel(data, abs)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	isUnitName, isUnitParent := wsUnitParents[parts[0]]
	if !isUnitParent {
		return "", !s.aliasesUnitParent(parts[0])
	}
	if len(parts) < 3 {
		return "", true
	}
	if !isUnitName(parts[1]) {
		return "", false
	}
	return filepath.Join(parts[0], parts[1]), true
}

// aliasesUnitParent 는 데이터 디렉터리 바로 아래 이름 name이 글자는 달라도 단위 부모 디렉터리를 가리킬 수 있는지 본다.
// 대소문자 접기로 같은 이름이거나, 둘 다 있고 같은 파일이면 그렇다. 파일 시스템마다 같게 보는 이름 규칙(대소문자,
// 유니코드 정규화)이 달라 글자 비교만으로는 다 알 수 없으므로 실제 파일도 견준다.
func (s *Server) aliasesUnitParent(name string) bool {
	data := filepath.Clean(s.m.dir)
	info, statErr := os.Stat(filepath.Join(data, name))
	for parent := range wsUnitParents {
		if strings.EqualFold(name, parent) {
			return true
		}
		if statErr != nil {
			continue
		}
		if parentInfo, err := os.Stat(filepath.Join(data, parent)); err == nil && os.SameFile(info, parentInfo) {
			return true
		}
	}
	return false
}

// errWorkspaceUnitLink 는 작업 단위 디렉터리 자체가 심볼릭 링크라 열지 않았다는 뜻이다. 서버 오류가 아니라 요청 거부다.
var errWorkspaceUnitLink = errors.New("작업 단위 디렉터리가 심볼릭 링크입니다")

// wsOpenRoot 는 작업 단위 unit(비면 데이터 디렉터리 전체)을 os.Root로 연다. 요청마다 열어 닫으므로 작업
// 디렉터리가 바뀌어도 따라간다. create면 없는 단위 디렉터리를 만든다(쓰기·새 디렉터리·업로드).
// 단위 디렉터리 자체가 링크이면 열지 않는다.
func (s *Server) wsOpenRoot(unit string, create bool) (*os.Root, error) {
	data, err := os.OpenRoot(filepath.Clean(s.m.dir))
	if err != nil || unit == "" {
		return data, err
	}
	defer data.Close()
	if create {
		if err := data.MkdirAll(unit, 0o755); err != nil {
			return nil, err
		}
	}
	info, err := data.Lstat(unit)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: %w", unit, errWorkspaceUnitLink)
	}
	return data.OpenRoot(unit)
}

// wsAbs 는 사용자가 준 상대 경로를 작업 공간 아래 절대 경로로 바꾼다. ".."는 루트에 묶어 접는다.
// 링크는 풀지 않으므로 이 결과만으로 작업 공간 안이라고 판정하지 않는다.
func (s *Server) wsAbs(rel string) string {
	base := filepath.Clean(s.m.dir)
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	clean := filepath.Clean("/" + rel) // e.g. "/a/../../etc" → "/etc" (still rooted at "/")
	return filepath.Clean(filepath.Join(base, clean))
}

// wsResolve 는 사용자가 준 상대 경로를 판정한다. 작업 단위(wsUnit) 밖이면 ok=false다.
// 심볼릭 링크는 풀어서 실제 경로가 단위 디렉터리(역시 링크를 푼 경로) 안일 때만 허용한다.
// 이 판정은 오류 응답을 가르는 용도다. 판정과 열기 사이의 경쟁은 열기를 단위의 os.Root로 해서 막는다.
func (s *Server) wsResolve(rel string) (wsPath, bool) {
	abs := s.wsAbs(rel)
	unit, ok := s.wsUnit(abs)
	if !ok {
		return wsPath{}, false
	}
	name, ok := s.wsRootName(unit, abs)
	if !ok {
		return wsPath{}, false
	}
	return wsPath{abs: abs, unit: unit, name: name}, true
}

// wsRootName 은 abs의 링크를 푼 실제 경로가 작업 단위 unit 안이면 단위 루트 기준 상대 이름을 돌려준다.
// 단위 자신은 "."다. 단위 디렉터리 자체가 링크이면(데이터 디렉터리를 푼 경로 뒤에 링크가 끼면) 거부한다.
// 아직 없는 단위는 만들 자리로 보고 판정한다.
func (s *Server) wsRootName(unit, abs string) (string, bool) {
	realData, err := filepath.EvalSymlinks(filepath.Clean(s.m.dir))
	if err != nil {
		return "", false
	}
	realBase := realData
	if unit != "" {
		resolvedUnit, err := evalWorkspacePath(filepath.Join(filepath.Clean(s.m.dir), unit))
		if err != nil || resolvedUnit != filepath.Join(realData, unit) {
			return "", false
		}
		realBase = resolvedUnit
	}
	resolved, err := evalWorkspacePath(abs)
	if err != nil {
		return "", false
	}
	inside, err := filepath.Rel(realBase, resolved)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return inside, true
}

// evalWorkspacePath 는 abs의 심볼릭 링크를 푼 실제 경로를 돌려준다. 아직 없는 경로(쓰기·새 디렉터리)는
// 실제로 있는 가장 깊은 조상을 풀어 남은 이름을 붙인다. 이름은 있는데 풀리지 않는 끊긴 링크는 오류다.
// 끊긴 링크를 없는 경로로 보고 조상만 판정하면, 그 경로에 쓸 때 링크가 가리키는 밖의 파일이 만들어진다.
func evalWorkspacePath(abs string) (string, error) {
	var missing []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(dir); lerr == nil {
			return "", fmt.Errorf("dangling symlink: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", err
		}
		missing = append([]string{filepath.Base(dir)}, missing...)
	}
}

// wsRel renders an absolute path back as a workspace-relative path (forward slashes).
func (s *Server) wsRel(abs string) string {
	base := filepath.Clean(s.m.dir)
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

type wsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix millis
}

// wsOpen 은 경로를 판정하고 그 경로의 작업 단위 루트를 연다. 실패하면 응답을 쓰고 ok=false를 돌려준다.
// ok=true이면 호출자가 root를 닫는다. create는 wsOpenRoot와 같다. 만들지 않는 요청에서 단위가 없으면 404다.
func (s *Server) wsOpen(w http.ResponseWriter, rel string, create bool) (*os.Root, wsPath, bool) {
	p, ok := s.wsResolve(rel)
	if !ok {
		writeErr(w, 400, "잘못된 경로입니다")
		return nil, wsPath{}, false
	}
	root, err := s.wsOpenRoot(p.unit, create)
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(w, 404, "경로가 없습니다")
		return nil, wsPath{}, false
	}
	if errors.Is(err, errWorkspaceUnitLink) {
		writeErr(w, 400, "잘못된 경로입니다")
		return nil, wsPath{}, false
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, wsPath{}, false
	}
	return root, p, true
}

// GET /api/workspace/list?path=<rel>
func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	root, p, ok := s.wsOpen(w, r.URL.Query().Get("path"), false)
	if !ok {
		return
	}
	defer root.Close()
	dir, err := root.Open(p.name)
	if err != nil {
		writeErr(w, 404, "경로가 없습니다")
		return
	}
	defer dir.Close()
	fi, err := dir.Stat()
	if err != nil {
		writeErr(w, 404, "경로가 없습니다")
		return
	}
	if !fi.IsDir() {
		writeErr(w, 400, "디렉터리가 아닙니다")
		return
	}
	ents, err := dir.ReadDir(-1)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]wsEntry, 0, len(ents))
	for _, e := range ents {
		// 항목 정보도 루트를 거쳐 읽는다. 그 사이 항목이 사라졌으면 목록에서 뺀다.
		info, err := root.Lstat(filepath.Join(p.name, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, wsEntry{
			Name:  e.Name(),
			Path:  s.wsRel(filepath.Join(p.abs, e.Name())),
			Dir:   info.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		})
	}
	// 디렉터리를 앞에 두고, 각각 이름순으로 정렬한다.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, map[string]any{"path": s.wsRel(p.abs), "entries": out})
}

// GET /api/workspace/read?path=<rel> — inline text for view/edit. Binary or oversize
// files return {binary:true}/{too_large:true} with no content (use download instead).
func (s *Server) wsRead(w http.ResponseWriter, r *http.Request) {
	root, p, ok := s.wsOpen(w, r.URL.Query().Get("path"), false)
	if !ok {
		return
	}
	defer root.Close()
	f, err := root.Open(p.name)
	if err != nil {
		writeErr(w, 404, "파일이 없습니다")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, 404, "파일이 없습니다")
		return
	}
	if fi.IsDir() {
		writeErr(w, 400, "디렉터리라서 파일로 읽을 수 없습니다")
		return
	}
	if fi.Size() > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(p.abs), "size": fi.Size(), "too_large": true, "binary": true})
		return
	}
	// 연 뒤에 파일이 커져도 상한까지만 읽는다.
	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceRead))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		writeJSON(w, 200, map[string]any{"path": s.wsRel(p.abs), "size": fi.Size(), "binary": true})
		return
	}
	writeJSON(w, 200, map[string]any{"path": s.wsRel(p.abs), "size": fi.Size(), "binary": false, "content": string(data)})
}

// POST /api/workspace/write  {path, content} — create/overwrite a text file.
func (s *Server) wsWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.wsAbs(req.Path) == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "잘못된 경로입니다")
		return
	}
	root, p, ok := s.wsOpen(w, req.Path, true)
	if !ok {
		return
	}
	defer root.Close()
	if fi, err := root.Stat(p.name); err == nil && fi.IsDir() {
		writeErr(w, 400, "대상이 디렉터리입니다")
		return
	}
	if err := root.MkdirAll(filepath.Dir(p.name), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := root.WriteFile(p.name, []byte(req.Content), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(p.abs)})
}

// POST /api/workspace/mkdir  {path}
func (s *Server) wsMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.wsAbs(req.Path) == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "잘못된 경로입니다")
		return
	}
	root, p, ok := s.wsOpen(w, req.Path, true)
	if !ok {
		return
	}
	defer root.Close()
	if err := root.MkdirAll(p.name, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": s.wsRel(p.abs)})
}

// DELETE /api/workspace/delete?path=<rel> — removes a file or a directory tree
// (confined to the work dir; the root itself can't be deleted).
// 마지막 요소가 심볼릭 링크이면 대상이 아니라 링크만 지운다. 그래서 밖을 가리키는 링크도 지울 수 있고,
// 안을 가리키는 링크를 지울 때 대상 디렉터리가 지워지지 않는다.
func (s *Server) wsDelete(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs := s.wsAbs(rel)
	if abs == filepath.Clean(s.m.dir) {
		writeErr(w, 400, "작업 공간 루트 디렉터리는 삭제할 수 없습니다")
		return
	}
	unit, ok := s.wsUnit(abs)
	if !ok {
		writeErr(w, 400, "잘못된 경로입니다")
		return
	}
	root, err := s.wsOpenRoot(unit, false)
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(w, 404, "경로가 없습니다")
		return
	}
	if errors.Is(err, errWorkspaceUnitLink) {
		writeErr(w, 400, "잘못된 경로입니다")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	// 단위 안의 경로는 단위보다 깊으므로(wsUnit) 부모도 같은 단위 안이다.
	if parentName, ok := s.wsRootName(unit, filepath.Dir(abs)); ok {
		linkName := filepath.Join(parentName, filepath.Base(abs))
		if fi, err := root.Lstat(linkName); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			if err := root.Remove(linkName); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	p, ok := s.wsResolve(rel)
	if !ok {
		writeErr(w, 400, "잘못된 경로입니다")
		return
	}
	if _, err := root.Lstat(p.name); err != nil {
		writeErr(w, 404, "경로가 없습니다")
		return
	}
	if err := root.RemoveAll(p.name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/workspace/download?path=<rel> — stream a file as an attachment.
func (s *Server) wsDownload(w http.ResponseWriter, r *http.Request) {
	root, p, ok := s.wsOpen(w, r.URL.Query().Get("path"), false)
	if !ok {
		return
	}
	defer root.Close()
	f, err := root.Open(p.name)
	if err != nil {
		writeErr(w, 404, "파일이 없습니다")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeErr(w, 404, "파일이 없습니다")
		return
	}
	name := filepath.Base(p.abs)
	// RFC 5987 filename* keeps non-ASCII names intact; plain filename is the fallback.
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFilename(name)+"\"; filename*=UTF-8''"+url.PathEscape(name))
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// POST /api/workspace/upload?path=<dir> — multipart form field "file" (one or more).
func (s *Server) wsUpload(w http.ResponseWriter, r *http.Request) {
	root, dir, ok := s.wsOpen(w, r.URL.Query().Get("path"), false)
	if !ok {
		return
	}
	defer root.Close()
	if fi, err := root.Stat(dir.name); err != nil || !fi.IsDir() {
		writeErr(w, 400, "대상 디렉터리가 없습니다")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "업로드를 파싱하지 못했거나 크기 제한을 넘었습니다: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "업로드 파일이 없습니다(폼 필드 file)")
		return
	}
	saved := 0
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." {
			continue
		}
		if err := saveUploadInRoot(root, hdr, filepath.Join(dir.name, name)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		saved++
	}
	writeJSON(w, 200, map[string]any{"uploaded": saved})
}

// saveUploadInRoot 는 올린 파일을 root 안의 name에 쓴다. 같은 디렉터리의 새 임시 파일에 다 쓴 뒤 name으로
// 이름을 바꾼다. name에 이미 링크가 있으면 링크가 가리키는 파일을 덮어쓰지 않고 링크를 일반 파일로 바꾼다
// (rename은 마지막 요소의 링크를 따라가지 않는다). name이 디렉터리면 실패한다.
func saveUploadInRoot(root *os.Root, hdr *multipart.FileHeader, name string) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	// 권한은 umask에 맡긴다. 에이전트 uid 분리(#165)를 켜면 서버 umask가 002라 0664가 되어, 사용자가 올린 파일을
	// 에이전트가 그 자리에서 고칠 수 있다. 같은 작업 디렉터리에 에이전트·서버가 만드는 다른 파일과 같다.
	out, tmp, err := createUniqueUpload(root, filepath.Dir(name), "."+filepath.Base(name)+".upload", 0o666)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, src)
	if err := errors.Join(copyErr, out.Close()); err != nil {
		_ = root.Remove(filepath.Join(filepath.Dir(name), tmp)) // 쓰다 만 임시 파일이다. 지우지 못해도 원래 오류가 원인이다
		return err
	}
	if err := root.Rename(filepath.Join(filepath.Dir(name), tmp), name); err != nil {
		_ = root.Remove(filepath.Join(filepath.Dir(name), tmp)) // 위와 같다
		return err
	}
	return nil
}

// sanitizeFilename strips characters unsafe for a Content-Disposition filename token.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	return name
}
