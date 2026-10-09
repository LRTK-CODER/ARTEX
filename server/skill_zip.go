package server

import (
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Go의 archive/zip에는 Store(0)와 Deflate(8) 두 압축 해제기만 들어 있고, 다른 방식을 만나면
// "zip: unsupported compression algorithm"을 돌려준다. 압축 프로그램은 기본이 아닌 설정에서
// 다른 방식(7-Zip의 bzip2, WinZip의 zstd)을 자주 쓰므로, 순수 Go로 풀 수 있는 이 두 방식을
// 더한다. 정말 풀 수 없는 것(Deflate64 / LZMA / XZ / PPMd / 암호화된 압축 파일)은 압축을 풀기
// 전에 읽을 수 있는 안내로 알려 준다. 하위 계층 오류를 그대로 사용자에게 넘기지 않기 위해서다.
const (
	zipMethodStore     = 0
	zipMethodDeflate   = 8
	zipMethodDeflate64 = 9
	zipMethodBzip2     = 12
	zipMethodLZMA      = 14
	zipMethodZstdPKW   = 20 // PKWARE가 예전에 zstd에 배정한 번호
	zipMethodZstd      = 93
	zipMethodXZ        = 95
	zipMethodJPEG      = 96
	zipMethodWavPack   = 97
	zipMethodPPMd      = 98
	zipMethodAES       = 99
)

var zipMethodNames = map[uint16]string{
	zipMethodStore:     "Store",
	zipMethodDeflate:   "Deflate",
	zipMethodDeflate64: "Deflate64",
	zipMethodBzip2:     "bzip2",
	zipMethodLZMA:      "LZMA",
	zipMethodZstdPKW:   "Zstandard",
	zipMethodZstd:      "Zstandard",
	zipMethodXZ:        "XZ",
	zipMethodJPEG:      "JPEG",
	zipMethodWavPack:   "WavPack",
	zipMethodPPMd:      "PPMd",
	zipMethodAES:       "AES 암호화",
}

func zipMethodName(m uint16) string {
	if n, ok := zipMethodNames[m]; ok {
		return n
	}
	return "알 수 없음"
}

// newSkillZipReader parses an uploaded archive and registers the extra decompressors
// we can support beyond the stdlib's Store/Deflate.
func newSkillZipReader(buf []byte) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("압축 파일을 파싱하지 못했습니다(zip 형식이어야 합니다): %w", err)
	}
	zr.RegisterDecompressor(zipMethodBzip2, func(r io.Reader) io.ReadCloser {
		return io.NopCloser(bzip2.NewReader(r))
	})
	zdec := zstd.ZipDecompressor(zstd.WithDecoderConcurrency(1))
	zr.RegisterDecompressor(zipMethodZstd, zdec)
	zr.RegisterDecompressor(zipMethodZstdPKW, zdec)
	return zr, nil
}

// skillZipEntry pairs a zip entry with its decoded (UTF-8) name — f.Name may hold
// raw GBK bytes, see zipEntryName.
type skillZipEntry struct {
	f    *zip.File
	name string
}

// skillZipEntries lists the archive's real files (no directory entries, no archiver
// junk) with their names decoded to UTF-8.
func skillZipEntries(zr *zip.Reader) []skillZipEntry {
	out := make([]skillZipEntry, 0, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := zipEntryName(f)
		if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") ||
			path.Base(name) == ".DS_Store" {
			continue // macOS가 압축할 때 남기는 파일
		}
		out = append(out, skillZipEntry{f: f, name: name})
	}
	return out
}

// zipEntryName 은 항목 경로를 UTF-8로 돌려준다. Windows의 7-Zip / WinRAR / 파일 탐색기는
// UTF-8 플래그를 켜지 않으면 중국어 파일 이름을 GBK로 zip에 쓴다. Go는 이 바이트를 그대로
// 두므로 이름이 올바른 UTF-8도 아니고 경로 검사도 통과하지 못한다. 그래서 GBK로 대신 디코딩한다.
func zipEntryName(f *zip.File) string {
	if utf8.ValidString(f.Name) {
		return f.Name
	}
	if dec, err := simplifiedchinese.GBK.NewDecoder().String(f.Name); err == nil && utf8.ValidString(dec) {
		return dec
	}
	return f.Name
}

// checkSkillZipMethods rejects archives we cannot extract, naming the offending
// entry and method instead of letting f.Open() fail with an opaque English error.
func checkSkillZipMethods(entries []skillZipEntry) error {
	for _, e := range entries {
		if e.f.Flags&0x1 != 0 || e.f.Method == zipMethodAES {
			return fmt.Errorf("압축 파일이 암호화돼 있습니다(%s). 암호화하지 않은 zip을 업로드하세요", e.name)
		}
		switch e.f.Method {
		case zipMethodStore, zipMethodDeflate, zipMethodBzip2, zipMethodZstd, zipMethodZstdPKW:
		default:
			return fmt.Errorf("압축 파일이 지원하지 않는 압축 방식 %s(method %d)을(를) 씁니다: %s. "+
				"'저장(Store)'이나 'Deflate'로 다시 압축하세요(7-Zip/WinRAR에서는 압축 방식을 Deflate로 고르거나, "+
				"운영체제 기본 '압축(ZIP) 폴더' 기능이나 명령줄 zip -r을 쓰세요)",
				zipMethodName(e.f.Method), e.f.Method, e.name)
		}
	}
	return nil
}
