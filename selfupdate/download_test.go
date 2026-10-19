package selfupdate

import (
	"archive/zip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixedBodyClient 는 어떤 요청에도 body를 200으로 돌려주는 가짜 클라이언트다.
func fixedBodyClient(body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
}

func TestDownloadStopsAtLimitAndRemovesTempFile(t *testing.T) {
	const url = "https://github.com/LRTK-CODER/ARTEX/releases/download/v1.0.0/a.zip"
	tests := []struct {
		name     string
		size     int64
		maxBytes int64
		body     string
		wantErr  bool
	}{
		{name: "자산 크기를 넘는 응답", size: 4, maxBytes: 100, body: "0123456789", wantErr: true},
		{name: "크기를 모를 때 고정 상한을 넘는 응답", size: 0, maxBytes: 4, body: "0123456789", wantErr: true},
		{name: "자산 크기가 고정 상한보다 큼", size: 10, maxBytes: 4, body: "0123456789", wantErr: true},
		{name: "자산 크기보다 짧은 응답", size: 20, maxBytes: 100, body: "0123456789", wantErr: true},
		{name: "자산 크기와 같은 응답", size: 10, maxBytes: 100, body: "0123456789"},
		{name: "크기를 모를 때 상한과 같은 응답", size: 0, maxBytes: 10, body: "0123456789"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "artex.new.zip.part")
			a := Asset{Name: "a.zip", URL: url, Size: tc.size}

			_, err := download(context.Background(), fixedBodyClient(tc.body), a, dst, tc.maxBytes, nil)

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("download: %v", err)
				}
				if got := readAll(t, dst); got != tc.body {
					t.Errorf("받은 내용 = %q, want %q", got, tc.body)
				}
				return
			}
			if err == nil {
				t.Fatal("상한을 벗어난 다운로드는 오류를 내야 함")
			}
			if _, statErr := os.Lstat(dst); !os.IsNotExist(statErr) {
				t.Errorf("실패한 다운로드의 임시 파일이 남아 있음: %v", statErr)
			}
		})
	}
}

// TestDownloadTempFileIsPrivateAndDoesNotFollowSymlink 는 다운로드 임시 파일이 0600이고, 고정 이름 자리에
// 미리 둔 심볼릭 링크를 따라가 다른 파일을 덮어쓰지 않는지 본다.
func TestDownloadTempFileIsPrivateAndDoesNotFollowSymlink(t *testing.T) {
	requireUnix(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "artex.new.zip.part")
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatal(err)
	}
	a := Asset{Name: "a.zip", URL: "https://github.com/x/a.zip", Size: 4}

	if _, err := download(context.Background(), fixedBodyClient("data"), a, dst, 100, func(Phase, int, string) {}); err != nil {
		t.Fatalf("download: %v", err)
	}

	assertPrivateRegularFile(t, dst)
	if got := readAll(t, victim); got != "keep" {
		t.Errorf("링크 대상 파일이 바뀌면 안 됨: %q", got)
	}
}

// TestWriteFilePrivateForSum 은 임시 저장본의 해시 파일을 쓰는 writeFilePrivate가 0600으로 쓰고
// 미리 둔 심볼릭 링크를 따라가지 않는지 본다.
func TestWriteFilePrivateForSum(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	victim := filepath.Join(p.Dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, p.Sum); err != nil {
		t.Fatal(err)
	}

	if err := writeFilePrivate(p.Sum, []byte("abc")); err != nil {
		t.Fatalf("writeFilePrivate: %v", err)
	}

	assertPrivateRegularFile(t, p.Sum)
	if got := readAll(t, p.Sum); got != "abc" {
		t.Errorf("해시 파일 내용 = %q", got)
	}
	if got := readAll(t, victim); got != "keep" {
		t.Errorf("링크 대상 파일이 바뀌면 안 됨: %q", got)
	}
}

// TestExtractBinaryDoesNotFollowSymlink 는 압축을 푼 바이너리 임시 파일도 미리 둔 심볼릭 링크를 따라가지 않는지 본다.
func TestExtractBinaryDoesNotFollowSymlink(t *testing.T) {
	requireUnix(t)
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "pkg.zip")
	writeTestZip(t, zipPath, map[string]string{"artex-1.0.0-linux-amd64/artex": "#!/bin/sh\n"})
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "artex.new.part")
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatal(err)
	}

	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}

	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o700 {
		t.Errorf("압축을 푼 바이너리 = %v, want 일반 파일 0700", fi.Mode())
	}
	if got := readAll(t, victim); got != "keep" {
		t.Errorf("링크 대상 파일이 바뀌면 안 됨: %q", got)
	}
}

func writeTestZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
