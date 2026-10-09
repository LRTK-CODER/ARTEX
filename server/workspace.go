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
	"strings"
	"unicode/utf8"
)

// Workspace file manager — browse / view / edit / download / upload / delete the
// shared work dir (s.m.dir), where all agents write their artifacts. All routes sit
// behind requireAuth (see Handler()).
// 에이전트가 작업 공간에 만든 심볼릭 링크는 신뢰하지 않는다. 경로는 wsResolve 가 링크를 푼 실제 경로로
// 판정하고, 실제 파일 연산은 요청마다 연 작업 공간 루트의 os.Root 로만 한다. 판정 뒤 링크를 바꿔도
// os.Root 가 커널 수준에서 루트 밖으로 나가는 경로를 막는다.
//
// 하드 링크는 막지 못한다. 서버 사용자 소유의 밖 파일을 같은 파일 시스템 안에서 작업 공간으로 하드 링크하면
// 경로 판정과 os.Root 모두 그 파일을 작업 공간 안의 정상 파일로 보므로 읽고 덮어쓸 수 있다. 링크 수(Nlink)로
// 거절하지 않는 이유는 패키지 관리자·백업 도구가 만든 정상 파일도 링크 수가 1보다 클 수 있어서다. 서버 키는
// 작업 공간 밖 디렉터리나 별도 볼륨에 두고(#53), 에이전트가 링크를 만드는 것 자체는 명령 실행 샌드박스가 막는다.

const (
	maxWorkspaceRead   = 2 << 20   // 2 MiB: files bigger than this aren't inlined for view/edit (download instead)
	maxWorkspaceUpload = 512 << 20 // 512 MiB per upload request
)

// wsPath 는 작업 공간 안으로 판정한 경로다.
type wsPath struct {
	// abs 는 링크를 풀기 전 절대 경로다. 응답의 상대 경로(wsRel)가 사용자가 본 이름을 유지하게 한다.
	abs string
	// name 은 링크를 푼 실제 경로의 루트 기준 상대 이름이다. os.Root 메서드에 넘긴다.
	// os.Root 는 절대 경로 링크를 따라가지 않으므로, 안을 가리키는 절대 링크도 풀어 둔 이름으로 연다.
	name string
}

// wsOpenRoot 는 작업 공간 루트를 os.Root 로 연다. 요청마다 열어 닫으므로 작업 디렉터리가 바뀌어도 따라간다.
func (s *Server) wsOpenRoot() (*os.Root, error) {
	return os.OpenRoot(filepath.Clean(s.m.dir))
}

// wsAbs 는 사용자가 준 상대 경로를 작업 공간 아래 절대 경로로 바꾼다. ".." 는 루트에 묶어 접는다.
// 링크는 풀지 않으므로 이 결과만으로 작업 공간 안이라고 판정하지 않는다.
func (s *Server) wsAbs(rel string) string {
	base := filepath.Clean(s.m.dir)
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	clean := filepath.Clean("/" + rel) // e.g. "/a/../../etc" → "/etc" (still rooted at "/")
	return filepath.Clean(filepath.Join(base, clean))
}

// wsResolve 는 사용자가 준 상대 경로를 판정한다. 작업 공간 밖이면 ok=false 다.
// 심볼릭 링크는 풀어서 실제 경로가 작업 공간(역시 링크를 푼 루트) 안일 때만 허용한다.
// 이 판정은 오류 응답을 가르는 용도다. 판정과 열기 사이의 경쟁은 열기를 os.Root 로 해서 막는다.
func (s *Server) wsResolve(rel string) (wsPath, bool) {
	abs := s.wsAbs(rel)
	name, ok := s.wsRootName(abs)
	if !ok {
		return wsPath{}, false
	}
	return wsPath{abs: abs, name: name}, true
}

// wsRootName 은 abs 의 링크를 푼 실제 경로가 작업 공간 안이면 루트 기준 상대 이름을 돌려준다. 루트 자신은 "." 다.
func (s *Server) wsRootName(abs string) (string, bool) {
	realBase, err := filepath.EvalSymlinks(filepath.Clean(s.m.dir))
	if err != nil {
		return "", false
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

// evalWorkspacePath 는 abs 의 심볼릭 링크를 푼 실제 경로를 돌려준다. 아직 없는 경로(쓰기·새 디렉터리)는
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

// wsOpen 은 경로를 판정하고 작업 공간 루트를 연다. 실패하면 응답을 쓰고 ok=false 를 돌려준다.
// ok=true 이면 호출자가 root 를 닫는다.
func (s *Server) wsOpen(w http.ResponseWriter, rel string) (*os.Root, wsPath, bool) {
	p, ok := s.wsResolve(rel)
	if !ok {
		writeErr(w, 400, "잘못된 경로입니다")
		return nil, wsPath{}, false
	}
	root, err := s.wsOpenRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, wsPath{}, false
	}
	return root, p, true
}

// GET /api/workspace/list?path=<rel>
func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	root, p, ok := s.wsOpen(w, r.URL.Query().Get("path"))
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
	root, p, ok := s.wsOpen(w, r.URL.Query().Get("path"))
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
	root, p, ok := s.wsOpen(w, req.Path)
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
	root, p, ok := s.wsOpen(w, req.Path)
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
	root, err := s.wsOpenRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	if parentName, ok := s.wsRootName(filepath.Dir(abs)); ok {
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
	root, p, ok := s.wsOpen(w, r.URL.Query().Get("path"))
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
	root, dir, ok := s.wsOpen(w, r.URL.Query().Get("path"))
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
		dest, okd := s.wsResolve(filepath.Join(s.wsRel(dir.abs), name))
		if !okd {
			continue
		}
		if err := saveUploadInRoot(root, hdr, dest.name); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		saved++
	}
	writeJSON(w, 200, map[string]any{"uploaded": saved})
}

// saveUploadInRoot 는 올린 파일을 root 안의 name 에 쓴다. 닫기 오류도 돌려준다(늦게 비워지는 쓰기 실패).
func saveUploadInRoot(root *os.Root, hdr *multipart.FileHeader, name string) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := root.Create(name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close() // 복사 오류가 원인이므로 닫기 오류는 덮어쓰지 않는다
		return err
	}
	return out.Close()
}

// sanitizeFilename strips characters unsafe for a Content-Disposition filename token.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	return name
}
