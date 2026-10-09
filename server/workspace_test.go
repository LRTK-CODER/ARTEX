package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const workspaceTestSecret = "outside-secret"

// workspaceFixture 는 심볼릭 링크가 섞인 작업 공간을 만든다.
// 작업 공간 루트(dataDir) 자체도 링크라서 루트를 해석한 뒤 비교하는지 함께 본다.
//
//	outside/secret.txt              작업 공간 밖 비밀
//	real/sub/ok.txt                 작업 공간 안 파일
//	real/out      -> outside        밖을 가리키는 링크
//	real/in       -> real/sub       안을 가리키는 정상 링크
//	real/nested   -> real/out       링크를 거쳐 밖으로 가는 중첩 링크
//	real/dangling -> outside/none   밖의 없는 경로를 가리키는 링크
//	data          -> real           작업 공간 루트로 쓰는 링크
type workspaceFixture struct {
	server  *Server
	outside string
	real    string
}

func newWorkspaceFixture(t *testing.T) workspaceFixture {
	t.Helper()
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	realDir := filepath.Join(tmp, "real")
	data := filepath.Join(tmp, "data")
	mustMkdir(t, outside)
	mustMkdir(t, filepath.Join(realDir, "sub"))
	mustWrite(t, filepath.Join(outside, "secret.txt"), workspaceTestSecret)
	mustWrite(t, filepath.Join(realDir, "sub", "ok.txt"), "inside")
	mustSymlink(t, outside, filepath.Join(realDir, "out"))
	mustSymlink(t, filepath.Join(realDir, "sub"), filepath.Join(realDir, "in"))
	mustSymlink(t, filepath.Join(realDir, "out"), filepath.Join(realDir, "nested"))
	mustSymlink(t, filepath.Join(outside, "none"), filepath.Join(realDir, "dangling"))
	mustSymlink(t, realDir, data)
	return workspaceFixture{server: &Server{m: &Manager{dir: data}}, outside: outside, real: realDir}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func serveWorkspace(handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func workspaceQuery(method, route, path string) *http.Request {
	return httptest.NewRequest(method, route+"?path="+url.QueryEscape(path), nil)
}

func workspaceJSON(route, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
}

func TestWorkspaceReadRoutesConfinedAfterSymlinks(t *testing.T) {
	f := newWorkspaceFixture(t)
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
		{name: "밖을 가리키는 링크 아래 파일", path: "out/secret.txt", routes: fileRoutes, wantStatus: http.StatusBadRequest},
		{name: "밖을 가리키는 링크 디렉터리", path: "out", routes: dirRoutes, wantStatus: http.StatusBadRequest},
		{name: "중첩 링크 아래 파일", path: "nested/secret.txt", routes: fileRoutes, wantStatus: http.StatusBadRequest},
		{name: "중첩 링크 디렉터리", path: "nested", routes: dirRoutes, wantStatus: http.StatusBadRequest},
		// ".." 는 루트에 묶여 작업 공간 안의 없는 경로가 된다.
		{name: "점점 경로", path: "../outside/secret.txt", routes: fileRoutes, wantStatus: http.StatusNotFound},
		{name: "안을 가리키는 링크 아래 파일", path: "in/ok.txt", routes: fileRoutes, wantStatus: http.StatusOK},
		{name: "안을 가리키는 링크 디렉터리", path: "in", routes: dirRoutes, wantStatus: http.StatusOK},
	}
	for _, tc := range cases {
		for _, route := range tc.routes {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				rec := serveWorkspace(handlers[route], workspaceQuery(http.MethodGet, route, tc.path))
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), workspaceTestSecret) {
					t.Fatalf("작업 공간 밖 내용이 응답에 있다: %s", rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), f.outside) {
					t.Fatalf("해석한 실제 경로가 응답에 있다: %s", rec.Body.String())
				}
			})
		}
	}
}

func TestWorkspaceWriteRoutesConfinedAfterSymlinks(t *testing.T) {
	cases := []struct {
		name       string
		request    func(f workspaceFixture) (http.HandlerFunc, *http.Request)
		wantStatus int
		// mustNotExist 는 요청 뒤에도 없어야 하는 작업 공간 밖 경로(outside 기준)다.
		mustNotExist string
		// mustExist 는 요청 뒤에도 남아 있어야 하는 작업 공간 밖 경로(outside 기준)다.
		mustExist string
		// goneInside 는 요청 뒤에 없어야 하는 작업 공간 안 경로(real 기준)다.
		goneInside string
		// keptInside 는 요청 뒤에도 남아 있어야 하는 작업 공간 안 경로(real 기준)다.
		keptInside string
	}{
		{
			name: "쓰기: 밖을 가리키는 링크 아래 새 파일",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"out/new.txt","content":"x"}`)
			},
			wantStatus: http.StatusBadRequest, mustNotExist: "new.txt",
		},
		{
			name: "쓰기: 밖을 가리키는 끊긴 링크",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"dangling","content":"x"}`)
			},
			wantStatus: http.StatusBadRequest, mustNotExist: "none",
		},
		{
			name: "쓰기: 중첩 링크 아래 없는 디렉터리의 새 파일",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"nested/a/b.txt","content":"x"}`)
			},
			wantStatus: http.StatusBadRequest, mustNotExist: "a",
		},
		{
			name: "새 디렉터리: 밖을 가리키는 링크 아래",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsMkdir, workspaceJSON("/api/workspace/mkdir", `{"path":"out/newdir"}`)
			},
			wantStatus: http.StatusBadRequest, mustNotExist: "newdir",
		},
		{
			name: "삭제: 밖을 가리키는 링크 아래 파일",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsDelete, workspaceQuery(http.MethodDelete, "/api/workspace/delete", "out/secret.txt")
			},
			wantStatus: http.StatusBadRequest, mustExist: "secret.txt",
		},
		{
			name: "삭제: 밖을 가리키는 링크 자체",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsDelete, workspaceQuery(http.MethodDelete, "/api/workspace/delete", "out")
			},
			wantStatus: http.StatusOK, mustExist: "secret.txt", goneInside: "out",
		},
		{
			name: "삭제: 안을 가리키는 링크 자체",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsDelete, workspaceQuery(http.MethodDelete, "/api/workspace/delete", "in")
			},
			wantStatus: http.StatusOK, goneInside: "in", keptInside: "sub/ok.txt",
		},
		{
			name: "올리기: 밖을 가리키는 링크 디렉터리",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsUpload, uploadRequest(t, "out", "up.txt")
			},
			wantStatus: http.StatusBadRequest, mustNotExist: "up.txt",
		},
		{
			name: "쓰기: 안을 가리키는 링크 아래 새 파일",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsWrite, workspaceJSON("/api/workspace/write", `{"path":"in/new.txt","content":"x"}`)
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "새 디렉터리: 안을 가리키는 링크 아래",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsMkdir, workspaceJSON("/api/workspace/mkdir", `{"path":"in/a/b"}`)
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "올리기: 안을 가리키는 링크 디렉터리",
			request: func(f workspaceFixture) (http.HandlerFunc, *http.Request) {
				return f.server.wsUpload, uploadRequest(t, "in", "up.txt")
			},
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			handler, req := tc.request(f)
			rec := serveWorkspace(handler, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.mustNotExist != "" {
				if _, err := os.Lstat(filepath.Join(f.outside, tc.mustNotExist)); err == nil {
					t.Fatalf("작업 공간 밖에 %s가 생겼다", tc.mustNotExist)
				}
			}
			if tc.mustExist != "" {
				if _, err := os.Lstat(filepath.Join(f.outside, tc.mustExist)); err != nil {
					t.Fatalf("작업 공간 밖 %s가 사라졌다: %v", tc.mustExist, err)
				}
			}
			if tc.goneInside != "" {
				if _, err := os.Lstat(filepath.Join(f.real, tc.goneInside)); err == nil {
					t.Fatalf("작업 공간 안 %s가 남아 있다", tc.goneInside)
				}
			}
			if tc.keptInside != "" {
				if _, err := os.Lstat(filepath.Join(f.real, tc.keptInside)); err != nil {
					t.Fatalf("작업 공간 안 %s가 사라졌다: %v", tc.keptInside, err)
				}
			}
		})
	}
}

func uploadRequest(t *testing.T, dir, name string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("uploaded")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workspace/upload?path="+url.QueryEscape(dir), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}
