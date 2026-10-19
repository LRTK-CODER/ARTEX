package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixedBodyClient 는 어떤 요청에도 body를 200으로 돌려주는 가짜 클라이언트다.
func fixedBodyClient(body string) *http.Client {
	return readerClient(strings.NewReader(body))
}

// readerClient 는 어떤 요청에도 r을 본문으로 200을 돌려주는 가짜 클라이언트다.
func readerClient(r io.Reader) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(r),
			Header:     make(http.Header),
		}, nil
	})}
}

// countingReader 는 본문에서 실제로 읽힌 바이트 수를 센다. "다 받은 뒤"가 아니라 "받는 도중에" 멈췄는지 보려고 쓴다.
type countingReader struct {
	r     io.Reader
	count int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.count += int64(n)
	return n, err
}

// zeroReader 는 0바이트를 끝없이 돌려준다.
type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
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

	// 끝없는 응답은 상한+1바이트까지만 읽고 멈춰야 한다. 다 받은 뒤에 크기를 보는 검사만으로는 디스크가 먼저 찬다.
	// 상한 없이 읽는 회귀가 테스트를 끝없이 붙잡지 않게 본문은 16 MiB에서 끝낸다.
	t.Run("끝없는 응답은 받는 도중에 멈춤", func(t *testing.T) {
		const maxBytes = 4
		dst := filepath.Join(t.TempDir(), "artex.new.zip.part")
		body := &countingReader{r: io.LimitReader(zeroReader{}, 16<<20)}
		a := Asset{Name: "a.zip", URL: url}

		_, err := download(context.Background(), readerClient(body), a, dst, maxBytes, nil)

		if err == nil {
			t.Fatal("상한을 넘는 응답은 오류를 내야 함")
		}
		if body.count > maxBytes+1 {
			t.Errorf("읽은 양 = %d바이트, want ≤ %d(상한+1)", body.count, maxBytes+1)
		}
		if _, statErr := os.Lstat(dst); !os.IsNotExist(statErr) {
			t.Errorf("실패한 다운로드의 임시 파일이 남아 있음: %v", statErr)
		}
	})
}

// TestStageWritesPrivateSumFile 은 Stage의 성공 경로가 임시 저장본과 0600 해시 파일을 남기는지 본다.
// writeFilePrivate만이 아니라 Stage가 해시 파일을 그것으로 쓰는지(호출 지점)를 잡는다.
func TestStageWritesPrivateSumFile(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	const tag = "v1.0.0"
	name := AssetName(tag, runtime.GOOS, runtime.GOARCH)
	binBody := "#!/bin/sh\nexit 0\n"

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, err := zw.Create("artex-1.0.0/artex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(binBody)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipSum := sha256.Sum256(zipBuf.Bytes())
	sums := hex.EncodeToString(zipSum[:]) + "  " + name + "\n"

	base := "https://github.com/LRTK-CODER/ARTEX/releases/download/" + tag + "/"
	bodies := map[string][]byte{base + name: zipBuf.Bytes(), base + sumsAsset: []byte(sums)}
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, ok := bodies[r.URL.String()]
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	rel := &Release{TagName: tag, Assets: []Asset{
		{Name: name, URL: base + name, Size: int64(zipBuf.Len())},
		{Name: sumsAsset, URL: base + sumsAsset, Size: int64(len(sums))},
	}}

	if err := stageAt(context.Background(), c, rel, p, "0.9.0", nil); err != nil {
		t.Fatalf("stageAt: %v", err)
	}

	if got := readAll(t, p.New); got != binBody {
		t.Errorf("임시 저장본 내용 = %q, want %q", got, binBody)
	}
	assertPrivateRegularFile(t, p.Sum)
	binSum := sha256.Sum256([]byte(binBody))
	if got := readAll(t, p.Sum); got != hex.EncodeToString(binSum[:]) {
		t.Errorf("해시 파일 내용 = %q, want 임시 저장본의 sha256", got)
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
