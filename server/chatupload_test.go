package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// chatUploadRequest 는 scope=session 채팅 첨부 요청을 만든다. 파일 이름은 report.txt 로 고정한다.
func chatUploadRequest(t *testing.T, id, content string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat/upload?scope=session&id="+id, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func decodeAttachments(t *testing.T, rec *httptest.ResponseRecorder) []chatAttachment {
	t.Helper()
	var got struct {
		Attachments []chatAttachment `json:"attachments"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got.Attachments
}

func TestChatUploadKeepsPathAndSuffixForPlainUploads(t *testing.T) {
	data := t.TempDir()
	s := &Server{m: &Manager{dir: data}}

	for i, want := range []string{"report.txt", "report-1.txt"} {
		rec := httptest.NewRecorder()
		s.chatUpload(rec, chatUploadRequest(t, "s1", "body"))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d: status=%d body=%s", i, rec.Code, rec.Body.String())
		}
		atts := decodeAttachments(t, rec)
		if len(atts) != 1 {
			t.Fatalf("upload %d: attachments=%v", i, atts)
		}
		wantAbs := filepath.Join(data, "sessions", "s1", "uploads", want)
		got := atts[0]
		if got.Name != want || got.Path != "uploads/"+want || got.Abs != wantAbs || got.Size != 4 {
			t.Fatalf("upload %d: got %+v, want name=%s abs=%s size=4", i, got, want, wantAbs)
		}
		if b, err := os.ReadFile(wantAbs); err != nil || string(b) != "body" {
			t.Fatalf("upload %d: saved content=%q err=%v", i, b, err)
		}
	}
}

// 에이전트가 작업 디렉터리 안에 만든 링크로 첨부를 작업 공간 밖에 쓰게 할 수 없어야 한다(#97).
func TestChatUploadDoesNotFollowLinksOutsideWorkspace(t *testing.T) {
	tests := []struct {
		name string
		// plant 는 data(작업 공간)와 outside 사이에 링크를 심는다.
		plant func(t *testing.T, data, outside string)
		id    string
		// wantSaved 가 비어 있으면 거절을 기대한다. 아니면 작업 공간 기준 저장 경로다.
		wantSaved string
	}{
		{
			name: "uploads 디렉터리가 밖을 가리킴",
			plant: func(t *testing.T, data, outside string) {
				mustMkdir(t, filepath.Join(data, "sessions", "s1"))
				mustSymlink(t, outside, filepath.Join(data, "sessions", "s1", "uploads"))
			},
			id: "s1",
		},
		{
			name: "같은 이름이 밖의 없는 파일을 가리키는 끊긴 링크",
			plant: func(t *testing.T, data, outside string) {
				dir := filepath.Join(data, "sessions", "s2", "uploads")
				mustMkdir(t, dir)
				mustSymlink(t, filepath.Join(outside, "planted.txt"), filepath.Join(dir, "report.txt"))
			},
			id:        "s2",
			wantSaved: filepath.Join("sessions", "s2", "uploads", "report-1.txt"),
		},
		{
			name: "sessions 디렉터리가 밖을 가리킴",
			plant: func(t *testing.T, data, outside string) {
				mustSymlink(t, outside, filepath.Join(data, "sessions"))
			},
			id: "s3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, outside := t.TempDir(), t.TempDir()
			tc.plant(t, data, outside)
			s := &Server{m: &Manager{dir: data}}

			rec := httptest.NewRecorder()
			s.chatUpload(rec, chatUploadRequest(t, tc.id, "user-attachment"))

			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("outside dir got entries %v (status=%d body=%s)", entries, rec.Code, rec.Body.String())
			}
			if tc.wantSaved == "" {
				if rec.Code == http.StatusOK {
					t.Fatalf("status=200 body=%s, want rejection", rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
			}
			atts := decodeAttachments(t, rec)
			wantAbs := filepath.Join(data, tc.wantSaved)
			if len(atts) != 1 || atts[0].Abs != wantAbs {
				t.Fatalf("attachments=%+v, want abs=%s", atts, wantAbs)
			}
			if b, err := os.ReadFile(wantAbs); err != nil || string(b) != "user-attachment" {
				t.Fatalf("saved content=%q err=%v", b, err)
			}
		})
	}
}
