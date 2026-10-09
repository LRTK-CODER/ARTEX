package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// maxChatUpload caps a single chat-attachment upload request (memory + spill).
const maxChatUpload = 128 << 20 // 128 MiB

// safeChatID guards the {id} path segment against traversal — task ids are numeric,
// session ids are alnum/_/- ; anything with "/" or ".." is rejected.
var safeChatID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// chatAttachment is one uploaded file as the frontend + agent see it. Path is relative
// to the chat's working dir (e.g. "uploads/report.txt"), which is the agent's CWD, so
// it can Read/Bash the file directly; Name/Size drive the UI card.
type chatAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Abs 는 저장한 파일의 절대 경로다(m.dir가 이미 절대 경로다). 작업을 만들기 전에 임시 저장할 때
	// (scope=staging) 프런트엔드가 이 값으로 프롬프트를 작업 설명에 써 넣는다. task·session은
	// composeAgentMessage가 백엔드에서 경로를 이어 붙이므로 이 필드에 기대지 않는다.
	Abs string `json:"abs,omitempty"`
}

// chatUpload 는 파일 첨부의 첫 번째 방식이다. 파일 하나 이상을 대화 작업 디렉터리의
// uploads/ 아래에 저장해, 에이전트가 기존 Read/Bash 도구로 열고 보낸 메시지에 그 경로가
// 실리게 한다. LLM 계층은 바꾸지 않고 멀티모달도 쓰지 않는다.
//
// POST /api/chat/upload?scope=task|session|staging&id=<id>, multipart 필드 "file"
// (여러 번 가능). {attachments:[{name,path,size,abs}]}를 돌려준다. 저장 디렉터리는
// 에이전트 CWD 구조를 따른다:
//
//	scope=task    → <workDir>/tasks/<id>/uploads/
//	scope=session → <workDir>/sessions/<id>/uploads/
//	scope=staging → <workDir>/drafts/<id>/uploads/   (작업을 만들기 전 임시 저장: 작업에 아직 ID가
//	                없어 파일을 먼저 여기 두고, 프런트엔드가 돌려받은 abs 절대 경로를 작업 설명에 써 넣는다)
func (s *Server) chatUpload(w http.ResponseWriter, r *http.Request) {
	var sub string
	taskScoped := false
	switch r.URL.Query().Get("scope") {
	case "task":
		sub = "tasks"
		taskScoped = true
	case "session":
		sub = "sessions"
	case "staging":
		sub = "drafts"
	default:
		writeErr(w, 400, "scope는 task, session, staging 중 하나여야 합니다")
		return
	}
	id := r.URL.Query().Get("id")
	if !safeChatID.MatchString(id) {
		writeErr(w, 400, "잘못된 id입니다")
		return
	}
	if taskScoped {
		if s.m.ResolveTask(id) == nil {
			writeErr(w, 404, "task not found")
			return
		}
		if !s.engine.beginTaskOperation(id) {
			writeErr(w, http.StatusConflict, "작업을 삭제하는 중이라 첨부 파일을 업로드할 수 없습니다")
			return
		}
		defer s.engine.decInflight(id)
	}
	// 에이전트가 작업 디렉터리 안에 심볼릭 링크를 만들 수 있으므로 만들기·쓰기는 작업 공간 루트의 os.Root 로만 한다(#97).
	root, err := s.wsOpenRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	rel := filepath.Join(sub, id, "uploads")
	if err := root.MkdirAll(rel, 0o755); err != nil {
		writeErr(w, 500, "업로드 디렉터리를 만들지 못했습니다: "+err.Error())
		return
	}
	dir := filepath.Join(s.m.dir, rel)
	r.Body = http.MaxBytesReader(w, r.Body, maxChatUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "업로드를 파싱하지 못했거나 크기 제한을 넘었습니다: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "업로드할 파일이 없습니다(폼 필드 file)")
		return
	}
	out := make([]chatAttachment, 0, len(files))
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		base, err := saveUniqueUpload(root, rel, hdr, name)
		if err != nil {
			writeErr(w, 500, "첨부 파일을 저장하지 못했습니다: "+err.Error())
			return
		}
		out = append(out, chatAttachment{Name: base, Path: "uploads/" + base, Size: hdr.Size, Abs: filepath.Join(dir, base)})
	}
	writeJSON(w, 200, map[string]any{"attachments": out})
}

// createUniqueUpload 는 root 안의 dir에 name, 이미 있으면 name-1, name-2… 순서로 새 파일을 만들고
// 고른 이름을 돌려준다. 같은 이름을 다시 올려도 앞 첨부를 덮어쓰지 않는다.
// O_EXCL로 만들므로 이름 고르기와 만들기 사이에 경쟁이 없고, 그 이름에 링크(끊긴 링크 포함)가 있으면
// 따라가지 않고 이미 있는 이름으로 본다.
func createUniqueUpload(root *os.Root, dir, name string) (*os.File, string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := root.OpenFile(filepath.Join(dir, cand), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, cand, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
}

// composeAgentMessage appends an attachment manifest to the user's message so the agent
// knows which files were uploaded and where to Read them. baseDir is the agent's working
// dir (its CWD); we emit ABSOLUTE paths (baseDir + relative) so the agent can Read/Bash
// them unambiguously regardless of how it interprets relative paths.
func composeAgentMessage(msg string, atts []chatAttachment, baseDir string) string {
	if len(atts) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\n[Files uploaded by the user] (absolute paths; open them with Read/Bash when needed):")
	for _, a := range atts {
		fmt.Fprintf(&b, "\n- %s (%s)", filepath.Join(baseDir, a.Path), humanBytes(a.Size))
	}
	return b.String()
}

// userActivityWithAttachments builds the persisted 'user' activity. With attachments,
// Detail holds JSON {text, attachments} so the transcript renders text + attachment
// cards; Summary stays the plain text (the list payload omits Detail, lazy-loaded).
func userActivityWithAttachments(worker, text string, atts []chatAttachment) db.Activity {
	a := db.Activity{Worker: worker, Kind: "user", Summary: text}
	if len(atts) > 0 {
		blob, _ := json.Marshal(map[string]any{"text": text, "attachments": atts})
		a.Detail = string(blob)
	}
	return a
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// saveUniqueUpload 는 채팅 첨부를 root 안의 dir에 겹치지 않는 이름으로 쓰고 그 이름을 돌려준다.
// 닫기 오류도 돌려준다(늦게 비워지는 쓰기 실패).
func saveUniqueUpload(root *os.Root, dir string, hdr *multipart.FileHeader, name string) (string, error) {
	src, err := hdr.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	out, base, err := createUniqueUpload(root, dir, name)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close() // 복사 오류가 원인이므로 닫기 오류는 덮어쓰지 않는다
		return "", err
	}
	return base, out.Close()
}
