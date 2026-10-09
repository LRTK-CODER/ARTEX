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
	// Abs 是落盘的绝对路径(m.dir 已是绝对)。建任务前暂存(scope=staging)时前端要用它把
	// 提示词写进描述;task/session 走 composeAgentMessage 在后端拼路径,不依赖此字段。
	Abs string `json:"abs,omitempty"`
}

// chatUpload implements method-1 file support: it saves one or more files into a chat's
// working dir under uploads/, so the agent opens them with its existing Read/Bash tools
// and the sent message carries their paths. No LLM-layer change, no multimodal.
//
// POST /api/chat/upload?scope=task|session|staging&id=<id>, multipart field "file"
// (repeatable). Returns {attachments:[{name,path,size,abs}]}. Target dir mirrors the
// agent CWD layout:
//
//	scope=task    → <workDir>/tasks/<id>/uploads/
//	scope=session → <workDir>/sessions/<id>/uploads/
//	scope=staging → <workDir>/drafts/<id>/uploads/   (建任务前暂存:任务尚无 ID,
//	                文件先落这里,前端按返回的 abs 绝对路径写进任务描述)
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
		writeErr(w, 400, "scope 必须是 task / session / staging")
		return
	}
	id := r.URL.Query().Get("id")
	if !safeChatID.MatchString(id) {
		writeErr(w, 400, "非法 id")
		return
	}
	if taskScoped {
		if s.m.ResolveTask(id) == nil {
			writeErr(w, 404, "task not found")
			return
		}
		if !s.engine.beginTaskOperation(id) {
			writeErr(w, http.StatusConflict, "任务正在删除，无法上传附件")
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
		writeErr(w, 500, "建目录失败: "+err.Error())
		return
	}
	dir := filepath.Join(s.m.dir, rel)
	r.Body = http.MaxBytesReader(w, r.Body, maxChatUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "解析上传失败或超出大小限制: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "缺少上传文件(表单字段 file)")
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
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}
		out = append(out, chatAttachment{Name: base, Path: "uploads/" + base, Size: hdr.Size, Abs: filepath.Join(dir, base)})
	}
	writeJSON(w, 200, map[string]any{"attachments": out})
}

// createUniqueUpload 는 root 안의 dir 에 name, 이미 있으면 name-1, name-2… 순서로 새 파일을 만들고
// 고른 이름을 돌려준다. 같은 이름을 다시 올려도 앞 첨부를 덮어쓰지 않는다.
// O_EXCL 로 만들므로 이름 고르기와 만들기 사이에 경쟁이 없고, 그 이름에 링크(끊긴 링크 포함)가 있으면
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
	b.WriteString("\n\n【用户上传的附件】(绝对路径，需要时用 Read/Bash 查看)：")
	for _, a := range atts {
		fmt.Fprintf(&b, "\n- %s（%s）", filepath.Join(baseDir, a.Path), humanBytes(a.Size))
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

// saveUniqueUpload 는 채팅 첨부를 root 안의 dir 에 겹치지 않는 이름으로 쓰고 그 이름을 돌려준다.
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
