package server

import (
	"archive/tar"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pgdb "github.com/Autumn-27/artex/db"
	"github.com/klauspost/compress/zstd"
)

const otherTaskSecret = "other-task-secret"

// unitFixture 는 작업 단위 디렉터리마다 루트를 가두는지 보는 데이터 디렉터리를 만든다(#176).
//
//	tasks/2/secret.txt                       다른 작업의 파일
//	transcripts/exp1-a.jsonl                 대화 기록
//	tasks/1/work/ok.txt                      자기 작업 파일
//	tasks/1/work/other   -> tasks/2          다른 작업 디렉터리를 가리키는 링크(데이터 디렉터리 안)
//	tasks/1/work/tx      -> transcripts/exp1-a.jsonl
//	tasks/1/work/abs     -> <data>/tasks/2/secret.txt (절대 경로 링크)
//	tasks/1/work/out     -> outside             데이터 디렉터리 밖을 가리키는 링크
//	tasks/1/work/self    -> tasks/1/work/sub    같은 작업 안을 가리키는 링크
type unitFixture struct {
	server  *Server
	data    string
	outside string
}

func newUnitFixture(t *testing.T) unitFixture {
	t.Helper()
	data, outside := t.TempDir(), t.TempDir()
	work := filepath.Join(data, "tasks", "1", "work")
	mustMkdir(t, filepath.Join(work, "sub"))
	mustMkdir(t, filepath.Join(data, "tasks", "2"))
	mustMkdir(t, filepath.Join(data, "transcripts"))
	mustWrite(t, filepath.Join(data, "tasks", "2", "secret.txt"), otherTaskSecret)
	mustWrite(t, filepath.Join(data, "transcripts", "exp1-a.jsonl"), otherTaskSecret)
	mustWrite(t, filepath.Join(outside, "secret.txt"), otherTaskSecret)
	mustWrite(t, filepath.Join(work, "ok.txt"), "inside")
	mustWrite(t, filepath.Join(work, "sub", "in.txt"), "inside")
	mustSymlink(t, filepath.Join("..", "..", "2"), filepath.Join(work, "other"))
	mustSymlink(t, filepath.Join("..", "..", "..", "transcripts", "exp1-a.jsonl"), filepath.Join(work, "tx"))
	mustSymlink(t, filepath.Join(data, "tasks", "2", "secret.txt"), filepath.Join(work, "abs"))
	mustSymlink(t, outside, filepath.Join(work, "out"))
	mustSymlink(t, "sub", filepath.Join(work, "self"))
	return unitFixture{server: &Server{m: &Manager{dir: data}}, data: data, outside: outside}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWorkspaceReadRoutesStayInTaskUnit(t *testing.T) {
	f := newUnitFixture(t)
	handlers := map[string]http.HandlerFunc{
		"/api/workspace/download": f.server.wsDownload,
		"/api/workspace/read":     f.server.wsRead,
		"/api/workspace/list":     f.server.wsList,
	}
	fileRoutes := []string{"/api/workspace/download", "/api/workspace/read"}
	dirRoutes := []string{"/api/workspace/list"}
	cases := []struct {
		name       string
		path       string
		routes     []string
		wantStatus int
	}{
		{name: "다른 작업 디렉터리 링크 아래 파일", path: "tasks/1/work/other/secret.txt", routes: fileRoutes, wantStatus: http.StatusBadRequest},
		{name: "다른 작업 디렉터리 링크", path: "tasks/1/work/other", routes: dirRoutes, wantStatus: http.StatusBadRequest},
		{name: "대화 기록 파일 링크", path: "tasks/1/work/tx", routes: fileRoutes, wantStatus: http.StatusBadRequest},
		{name: "다른 작업 파일 절대 경로 링크", path: "tasks/1/work/abs", routes: fileRoutes, wantStatus: http.StatusBadRequest},
		{name: "데이터 디렉터리 밖 링크 아래 파일", path: "tasks/1/work/out/secret.txt", routes: fileRoutes, wantStatus: http.StatusBadRequest},
		// ".."는 글자 그대로 접혀 다른 작업의 경로가 된다. 사용자가 고른 위치라 그 작업의 단위로 연다.
		{name: "점점 경로", path: "tasks/1/work/../../2/secret.txt", routes: fileRoutes, wantStatus: http.StatusOK},
		{name: "같은 작업 안 링크 아래 파일", path: "tasks/1/work/self/in.txt", routes: fileRoutes, wantStatus: http.StatusOK},
		{name: "정상 파일", path: "tasks/1/work/ok.txt", routes: fileRoutes, wantStatus: http.StatusOK},
		{name: "정상 디렉터리", path: "tasks/1/work", routes: dirRoutes, wantStatus: http.StatusOK},
		{name: "작업 디렉터리 자체", path: "tasks/1", routes: dirRoutes, wantStatus: http.StatusOK},
		{name: "없는 작업", path: "tasks/9/x.txt", routes: fileRoutes, wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		for _, route := range tc.routes {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				rec := serveWorkspace(handlers[route], workspaceQuery(http.MethodGet, route, tc.path))
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if tc.wantStatus != http.StatusOK && strings.Contains(rec.Body.String(), otherTaskSecret) {
					t.Fatalf("작업 단위 밖 내용이 응답에 있다: %s", rec.Body.String())
				}
			})
		}
	}
}

func TestWorkspaceWriteRoutesStayInTaskUnit(t *testing.T) {
	cases := []struct {
		name       string
		request    func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request)
		wantStatus int
	}{
		{
			name: "쓰기: 다른 작업 디렉터리 링크 아래 파일",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"tasks/1/work/other/secret.txt","content":"x"}`)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "쓰기: 대화 기록 파일 링크",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"tasks/1/work/tx","content":"x"}`)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "새 디렉터리: 다른 작업 디렉터리 링크 아래",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsMkdir, workspaceJSON("/api/workspace/mkdir", `{"path":"tasks/1/work/other/newdir"}`)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "삭제: 다른 작업 디렉터리 링크 아래 파일",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsDelete, workspaceQuery(http.MethodDelete, "/api/workspace/delete", "tasks/1/work/other/secret.txt")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "삭제: 다른 작업 디렉터리 링크 자체",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsDelete, workspaceQuery(http.MethodDelete, "/api/workspace/delete", "tasks/1/work/other")
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "올리기: 다른 작업 디렉터리 링크 디렉터리",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsUpload, uploadRequest(t, "tasks/1/work/other", "secret.txt")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "올리기: 같은 이름이 대화 기록을 가리키는 링크",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsUpload, uploadRequest(t, "tasks/1/work", "tx")
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "올리기: 같은 이름이 다른 작업 파일을 가리키는 절대 경로 링크",
			request: func(t *testing.T, f unitFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsUpload, uploadRequest(t, "tasks/1/work", "abs")
			},
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnitFixture(t)
			handler, req := tc.request(t, f)
			rec := serveWorkspace(handler, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			for _, p := range []string{
				filepath.Join(f.data, "tasks", "2", "secret.txt"),
				filepath.Join(f.data, "transcripts", "exp1-a.jsonl"),
				filepath.Join(f.outside, "secret.txt"),
			} {
				if got := readFileString(t, p); got != otherTaskSecret {
					t.Fatalf("작업 단위 밖 파일 %s가 바뀌었다: %q", p, got)
				}
			}
			for _, dir := range []string{filepath.Join(f.data, "tasks", "2"), f.outside} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Fatalf("작업 단위 밖 %s에 항목이 생겼다: %v", dir, entries)
				}
			}
		})
	}
}

// 올린 파일 이름에 링크가 있으면 링크 대상이 아니라 링크 자리에 일반 파일을 둔다.
func TestWorkspaceUploadReplacesLinkWithFile(t *testing.T) {
	f := newUnitFixture(t)
	rec := serveWorkspace(f.server.wsUpload, uploadRequest(t, "tasks/1/work", "tx"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	saved := filepath.Join(f.data, "tasks", "1", "work", "tx")
	info, err := os.Lstat(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("tx mode = %v, want regular file", info.Mode())
	}
	if got := readFileString(t, saved); got != "uploaded" {
		t.Fatalf("tx = %q, want uploaded", got)
	}
	entries, err := os.ReadDir(filepath.Join(f.data, "tasks", "1", "work"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".upload") {
			t.Fatalf("임시 파일 %s가 남았다", e.Name())
		}
	}
}

// 에이전트가 자기 세션의 uploads를 데이터 디렉터리 안의 다른 세션으로 돌려놓아도 첨부가 그쪽에 쓰이지 않는다(#176).
func TestChatUploadStaysInOwnSession(t *testing.T) {
	tests := []struct {
		name  string
		plant func(t *testing.T, data string)
		// wantSaved 가 비어 있으면 거절을 기대한다. 아니면 작업 공간 기준 저장 경로다.
		wantSaved string
	}{
		{
			name: "uploads 디렉터리가 다른 세션의 uploads를 가리킴",
			plant: func(t *testing.T, data string) {
				mustMkdir(t, filepath.Join(data, "sessions", "s1"))
				mustSymlink(t, filepath.Join("..", "s2", "uploads"), filepath.Join(data, "sessions", "s1", "uploads"))
			},
		},
		{
			name: "uploads 디렉터리가 같은 세션 안을 가리킴",
			plant: func(t *testing.T, data string) {
				mustMkdir(t, filepath.Join(data, "sessions", "s1", "work"))
				mustSymlink(t, "work", filepath.Join(data, "sessions", "s1", "uploads"))
			},
		},
		{
			name: "같은 이름이 다른 세션의 파일을 가리키는 링크",
			plant: func(t *testing.T, data string) {
				dir := filepath.Join(data, "sessions", "s1", "uploads")
				mustMkdir(t, dir)
				mustSymlink(t, filepath.Join("..", "..", "s2", "uploads", "report.txt"), filepath.Join(dir, "report.txt"))
			},
			wantSaved: filepath.Join("sessions", "s1", "uploads", "report-1.txt"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			other := filepath.Join(data, "sessions", "s2", "uploads")
			mustMkdir(t, other)
			mustWrite(t, filepath.Join(other, "report.txt"), otherTaskSecret)
			tc.plant(t, data)
			s := &Server{m: &Manager{dir: data}}

			rec := serveWorkspace(s.chatUpload, chatUploadRequest(t, "s1", "user-attachment"))

			entries, err := os.ReadDir(other)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || readFileString(t, filepath.Join(other, "report.txt")) != otherTaskSecret {
				t.Fatalf("다른 세션 uploads가 바뀌었다: %v (status=%d body=%s)", entries, rec.Code, rec.Body.String())
			}
			if tc.wantSaved == "" {
				if rec.Code != http.StatusConflict {
					t.Fatalf("status=%d body=%s, want 409", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
			}
			if got := readFileString(t, filepath.Join(data, tc.wantSaved)); got != "user-attachment" {
				t.Fatalf("saved content=%q", got)
			}
		})
	}
}

// 단위 이름을 글자가 다르게 써도(대소문자, 유니코드 대소문자 접기, 0을 앞에 붙인 id) 단위 판정을 건너뛰지 않는다.
// 대소문자를 무시하는 파일 시스템(macOS APFS, Docker Desktop 바인드 마운트)에서는 TASKS/1이 tasks/1과 같은
// 디렉터리라, 이를 단위 밖으로 보면 데이터 디렉터리 전체 루트로 열려 다른 작업의 링크를 따라간다(#176).
// 대소문자를 구분하는 파일 시스템에서도 같은 거부가 나야 어디서 돌려도 이 판정을 지킨다.
func TestWorkspaceRejectsUnitNameVariants(t *testing.T) {
	paths := []string{
		"TASKS/1/work/other/secret.txt",
		"Tasks/1/work/tx",
		"taſks/1/work/tx", // U+017F는 유니코드 대소문자 접기로 s와 같다
		"tasks/01/work/ok.txt",
		"tasks/1x/work/ok.txt",
		"sessions/CONV-1/other/secret.txt",
		"SESSIONS/conv-1/other/secret.txt",
		"tasks/3/secret.txt", // 단위 디렉터리 자체가 다른 작업을 가리키는 링크
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			f := newUnitFixture(t)
			mustMkdir(t, filepath.Join(f.data, "sessions", "conv-1"))
			mustSymlink(t, filepath.Join("..", "..", "tasks", "2"), filepath.Join(f.data, "sessions", "conv-1", "other"))
			mustSymlink(t, "2", filepath.Join(f.data, "tasks", "3"))
			requests := map[string]func() (http.HandlerFunc, *http.Request){
				"내려받기": func() (http.HandlerFunc, *http.Request) {
					return f.server.wsDownload, workspaceQuery(http.MethodGet, "/api/workspace/download", path)
				},
				"읽기": func() (http.HandlerFunc, *http.Request) {
					return f.server.wsRead, workspaceQuery(http.MethodGet, "/api/workspace/read", path)
				},
				"쓰기": func() (http.HandlerFunc, *http.Request) {
					return f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"`+path+`","content":"x"}`)
				},
				"삭제": func() (http.HandlerFunc, *http.Request) {
					return f.server.wsDelete, workspaceQuery(http.MethodDelete, "/api/workspace/delete", path)
				},
			}
			for name, request := range requests {
				handler, req := request()
				rec := serveWorkspace(handler, req)
				if rec.Code != http.StatusBadRequest {
					t.Errorf("%s: status = %d, want 400; body = %s", name, rec.Code, rec.Body.String())
				}
			}
			if got := readFileString(t, filepath.Join(f.data, "tasks", "2", "secret.txt")); got != otherTaskSecret {
				t.Fatalf("다른 작업 파일이 바뀌었다: %q", got)
			}
		})
	}
	// 정규 이름은 그대로 받는다.
	f := newUnitFixture(t)
	mustMkdir(t, filepath.Join(f.data, "sessions", "conv-1"))
	mustWrite(t, filepath.Join(f.data, "sessions", "conv-1", "note.txt"), "inside")
	for _, path := range []string{"tasks/1/work/ok.txt", "sessions/conv-1/note.txt"} {
		rec := serveWorkspace(f.server.wsRead, workspaceQuery(http.MethodGet, "/api/workspace/read", path))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body = %s", path, rec.Code, rec.Body.String())
		}
	}
}

// 보관본에는 링크 대상의 내용이 들어가지 않는다. 파일·디렉터리 링크 모두 항목으로도 남지 않는다.
func TestTaskArchivePackageDoesNotFollowLinks(t *testing.T) {
	dataDir, outside := t.TempDir(), t.TempDir()
	payload := filepath.Join(dataDir, "payload")
	work := filepath.Join(payload, "files", "tasks", "1", "work")
	mustMkdir(t, work)
	mustMkdir(t, filepath.Join(outside, "dir"))
	mustWrite(t, filepath.Join(outside, "secret.txt"), otherTaskSecret)
	mustWrite(t, filepath.Join(outside, "dir", "inner.txt"), otherTaskSecret)
	mustWrite(t, filepath.Join(work, "keep.txt"), "keep me")
	mustSymlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(work, "file-link"))
	mustSymlink(t, filepath.Join(outside, "dir"), filepath.Join(work, "dir-link"))
	mustSymlink(t, filepath.Join("..", "..", "..", "..", "..", "..", filepath.Base(outside), "secret.txt"), filepath.Join(work, "rel-link"))

	archivePath := filepath.Join(dataDir, "archive.tar.zst")
	snapshot := &pgdb.TaskArchiveSnapshot{FormatVersion: pgdb.TaskArchiveFormatVersion, TaskID: 1}
	if _, _, _, err := writeTaskArchivePackage(archivePath, payload, snapshot); err != nil {
		t.Fatalf("링크가 있어도 보관은 성공해야 한다: %v", err)
	}
	names, contents := readArchiveEntries(t, archivePath)
	if strings.Contains(contents, otherTaskSecret) {
		t.Fatalf("링크 대상 내용이 보관본에 들어갔다: entries=%v", names)
	}
	for _, name := range names {
		if strings.Contains(name, "link") {
			t.Fatalf("링크 항목 %s가 보관본에 들어갔다", name)
		}
	}
	if !strings.Contains(contents, "keep me") {
		t.Fatalf("일반 파일이 보관본에 없다: entries=%v", names)
	}
}

func readArchiveEntries(t *testing.T, path string) ([]string, string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder, err := zstd.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	reader := tar.NewReader(decoder)
	var names []string
	var contents strings.Builder
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if _, err := io.Copy(&contents, reader); err != nil {
			t.Fatal(err)
		}
	}
	return names, contents.String()
}
