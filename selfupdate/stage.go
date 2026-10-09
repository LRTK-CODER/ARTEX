package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset 은 release.yml 이 만드는 체크섬 목록이다. Release 의 zip 전부를 다룬다.
const sumsAsset = "SHA256SUMS"

// maxBinarySize 는 압축을 푼 바이너리 크기를 제한해 잘못된 zip 이 디스크를 가득 채우지 못하게 한다.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase 는 업데이트 과정의 단계다. SSE 이벤트의 phase 필드로 그대로 쓴다.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress 는 호출자가 넘겨 진행 상황을 프런트엔드로 보내는 데 쓴다. pct 는 다운로드 단계에서만 의미가 있고(0-100),
// 나머지 단계에서는 -1 을 넘긴다.
type Progress func(ph Phase, pct int, msg string)

// Stage 는 지정한 Release 의 현재 플랫폼용 배포 패키지를 내려받아 검증한 뒤 새 바이너리를 artex.new 로 임시 저장한다.
//
// 바이너리만이 아니라 zip 전체를 받는 이유는 두 가지다. 기존 Release 의 SHA256SUMS 는 원래
// zip 만 다루므로 zip 을 받으면 CI 를 고칠 필요가 없고 이미 배포한 이전 버전과도 호환된다. 또 zip 에는
// skills/ 가 들어 있어 나중에 내장 스킬을 동기화할 여지가 남는다. 대가는 skills 몇백 KB 를 더 받는 것뿐이다.
//
// 함수가 반환하면 임시 저장이 끝난 것이다. 호출자는 이어서 정상 종료하고 ExitRestart 로 종료한다.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("이 버전에는 %s/%s용 배포 패키지가 없습니다(%s 없음)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "체크섬 목록 받는 중…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s에 %s이(가) 없어 검증하지 않은 바이너리의 설치를 거부합니다", sumsAsset, name)
	}

	// 임시 파일은 모두 대상 디렉터리에 둔다. 마지막 rename 이 같은 파일 시스템 안의 원자적 동작이 되게 하기 위해서다
	// (다른 장치로의 rename 은 실패하고, /tmp 는 흔히 별도 마운트 지점이다).
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s(%s) 다운로드 중…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 검증 중…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256이 일치하지 않습니다(다운로드 손상 또는 변조): 기대값 %s, 실제값 %s", short(want), short(got))
	}

	prog(PhaseExtract, -1, "압축 해제와 스모크 테스트 중…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("새 버전이 현재 시스템에서 실행되지 않습니다: %w", err)
	}

	// 임시 저장본 자체의 sha256 을 따로 저장한다. 다음 시작 때 바이너리를 교체하기 전에 한 번 더 검증해서,
	// 임시 저장한 뒤 재시작하기 전 사이에 파일이 바뀌거나 망가지는 것을 막는다.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("새 바이너리 체크섬 계산: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("체크섬 쓰기: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("새 버전 임시 저장: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// 마커는 자동 롤백 기능에만 영향을 준다. 임시 저장본은 이미 준비됐으므로 이 때문에 업데이트를 멈추지 않는다.
		prog(PhaseStaged, -1, "경고: 업데이트 마커를 쓰지 못했습니다. 이번 업데이트는 자동 롤백으로 보호되지 않습니다")
	}

	prog(PhaseStaged, 100, "새 버전이 준비됐습니다. 재시작하는 중…")
	return nil
}

// fetchSums 는 SHA256SUMS 를 내려받아 파싱하고, 파일 이름 → 16진수 다이제스트를 돌려준다.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("이 Release에 %s이(가) 없어 무결성을 검증할 수 없으므로 업데이트를 거부합니다", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("%s 다운로드: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s 읽기: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s의 내용이 비어 있거나 형식을 알아볼 수 없습니다", sumsAsset)
	}
	return out, nil
}

// parseSums 는 sha256sum 형식의 목록을 파싱해 파일 이름 → 16진수 다이제스트를 돌려준다.
//
// 첫 필드가 64자리 16진수여야만 넣는다. "필드가 정확히 두 개"인지만 보면 부족하다.
// 두 단어로 된 설명 문장이 아무 줄에나 있으면 올바른 항목으로 취급돼 다이제스트 표에 쓰레기 값이 들어가고,
// 진짜 자산이 오히려 잘못된 다이제스트와 맞춰질 수 있다.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 형식은 "<sha256>  <filename>"이다(sha256sum 은 공백 두 칸을 쓰고, shasum 의 바이너리
		// 모드는 파일 이름 앞에 * 를 붙인다).
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download 는 자산을 dst 에 쓰면서 SHA256 을 계산하고 Content-Length 에 따라 진행 상황을 보고한다.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s 다운로드: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("임시 파일 만들기: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("다운로드 중단: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("디스크 쓰기 실패: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("다운로드가 완전하지 않습니다: 기대값 %d바이트, 실제값 %d바이트", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get 은 허용 목록 제약을 받는 GET 을 보내고 응답 본문을 돌려준다.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary 는 배포 패키지에서 artex 실행 파일을 꺼낸다.
//
// 패키지 안 구조는 artex-<버전>-<os>-<arch>/artex 이지만, 여기서는 전체 경로를 이어 붙이지 않고 **기본 이름**으로 찾는다.
// 버전 번호가 패키지 이름에 한 번 나오므로 한 글자만 틀려도 업데이트 전체가 실패한다. 기본 이름으로 찾는 편이 변경에 강하다.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("배포 패키지 열기: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s 읽기: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("새 바이너리 쓰기: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s 압축 해제: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("배포 패키지 안의 실행 파일이 %s을(를) 넘어 압축 해제를 거부합니다", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("배포 패키지 안의 %s이(가) 빈 파일입니다", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("배포 패키지에 %s이(가) 없습니다", want)
}

// checkWritable 은 디렉터리에 쓸 수 있는지 미리 확인한다. 이 단계가 없으면 root 가 아닌 사용자로 실행하거나 바이너리를 시스템
// 디렉터리에 둔 경우, 수십 MB 를 내려받은 뒤에야 바이너리를 교체하는 순간에 실패한다.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("프로그램 디렉터리 %s에 쓸 수 없어 자동 업데이트를 할 수 없습니다(권한을 확인하거나 수동으로 업데이트하세요): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter 는 쓴 바이트 수를 세고 보고 빈도를 제한한다. 32KiB 조각마다 SSE 를 하나씩 보내지 않기 위해서다.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("다운로드 중 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
