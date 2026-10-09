package selfupdate

import (
	"archive/zip"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths 는 격리된 업데이트 디렉터리를 만든다. ResolvePaths()를 바로 쓸 수 없다. 그러면 테스트
// 바이너리 자신을 가리켜, 돌리자마자 go test의 실행 파일 이름이 바뀐다.
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin 은 artex를 흉내 내는 실행 가능한 셸 스크립트를 쓴다. smokeTest는 -h로 띄워 종료 코드만 보므로
// 스크립트로 충분하고, 진짜 바이너리를 컴파일하는 것보다 훨씬 빠르다.
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("가짜 바이너리 %s 쓰기: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage 는 bin을 "임시 저장돼 바이너리 교체를 기다리는" 상태로 배치한다. artex.new와 그 체크섬을 써 둔다.
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("체크섬 계산: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("체크섬 쓰기: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s 읽기: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("가짜 바이너리가 sh 스크립트라 Windows 에서 실행할 수 없다")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh는 v를 떼고 tag는 v를 붙이므로 양쪽 모두 알아봐야 한다
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 사전순이 아니라 숫자로 비교한다
		{"1.0.0", "0.99.99", 1, true},
		// 개발 빌드는 비교할 수 없다고 판정해야 한다. 그러지 않으면 정식 버전이 커밋하지 않은 변경을 덮어쓴다.
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable = %v, 기대값 %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, 기대값 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 핵심 불변 조건: 업데이트 파일은 모두 실행 파일과 같은 디렉터리에 있다. CWD에 생기면 서비스로 실행할 때
	// (작업 디렉터리가 / 일 수 있다) 바이너리 교체가 완전히 무력해진다.
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s이(가) 실행 파일 디렉터리 밖에 있음: %s, 기대값 %s 아래", name, path, p.Dir)
		}
	}
	// Windows 에서는 .new/.old가 .exe를 유지해야 한다. 그러지 않으면 스모크 테스트와 바이너리 교체 뒤 실행이 모두 실패한다.
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows 에서 .new/.old는 .exe로 끝나야 함: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// 체크섬을 쓴 뒤 파일을 바꿔 다운로드 손상·바꿔치기를 흉내 낸다.
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 불일치로 거부돼야 하는데 통과했다")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 실행은 되지만 종료 코드가 0이 아니다

	if err := verifyStaged(p); err == nil {
		t.Fatal("스모크 테스트 실패로 거부돼야 하는데 통과했다")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("마커 쓰기: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("action = %v, 기대값 Restart", action)
	}
	if !st.Pending {
		t.Error("바이너리 교체 뒤 상태는 Pending 이어야 함")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex가 새 버전으로 바뀌어 있어야 함")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("이전 버전은 artex.old로 백업돼야 함")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("바이너리 교체 뒤 artex.new는 없어야 함")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("바이너리 교체 뒤 체크섬 파일은 정리돼야 함")
	}
	// 마커는 남아 있어야 한다. 다음 시작(새 버전으로 실행)이 이것으로 횟수를 세고 필요하면 롤백한다.
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("바이너리 교체 뒤 업데이트 마커는 남아 있어야 함")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // 체크섬을 깨뜨린다

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("검증 실패 시 action = %v, 기대값 Continue", action)
	}
	if !st.FailedStage {
		t.Error("상태는 FailedStage로 표시돼야 함")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("검증에 실패하면 현재 버전을 절대 건드리면 안 됨")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("검증에 실패한 임시 저장본은 정리돼야 함. 그러지 않으면 다음 시작에서 다시 시도한다")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 지난 업데이트가 남긴 백업
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3으로 바이너리가 교체돼야 함")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("백업은 방금 교체되어 나온 v2로 갱신돼야 함")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// 처음 maxAttempts 번의 시작은 횟수만 세서, 새 버전이 스스로 자리 잡을 기회를 준다.
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d번째 시도 action = %v, 기대값 Continue", i, action)
		}
		if !st.Pending {
			t.Errorf("%d번째 시도 상태는 Pending 이어야 함", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d번째 시도 뒤 attempts = %d(ok=%v), 기대값 %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// 한 번 더 죽으면 한도를 넘어, 자동으로 이전 버전을 되돌려 놓는다.
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("시도 한도 초과 시 action = %v, 기대값 Restart", action)
	}
	if !st.RolledBack {
		t.Error("상태는 RolledBack으로 표시돼야 함")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("이전 버전으로 롤백돼 있어야 함")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("롤백 뒤 마커는 지워져야 함. 그러지 않으면 롤백을 끝없이 되풀이한다")
	}
	// 시작하지 못한 버전은 바로 지우지 않고 원인 조사용으로 남긴다.
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("실패한 버전은 조사용으로 .failed에 남아야 함")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback()은 ResolvePaths()를 거치므로, 여기서는 아래 단계의 맞바꾸기 동작을 직접 테스트한다.
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("롤백 뒤 현재 버전은 v1 이어야 함")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("롤백 뒤 백업은 v2가 돼야 함. 그래야 다시 되돌릴 수 있다")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum 출력은 공백 두 칸으로 나뉜다. shasum -a 256은 바이너리 모드에서 파일 이름 앞에 * 를 붙인다.
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // 필드가 정확히 두 개지만 첫 필드가 다이제스트가 아니다
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // 다이제스트 길이가 맞지 않는다

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux 항목 파싱 오류: %v", out)
	}
	// 다이제스트를 소문자로 통일해야 비교할 때 대소문자 때문에 불일치로 잘못 판정하지 않는다.
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows 항목 오류(* 접두사를 떼고 다이제스트를 소문자로 바꿔야 함): %q", got)
	}
	if len(out) != 2 {
		t.Errorf("빈 줄, 다이제스트가 아닌 줄, 길이가 틀린 줄은 무시해야 함, 실제값 %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 에서는 패키지 안 기본 이름이 artex.exe 라서, 이 테스트 케이스는 Unix 이름으로 만든다")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 실제 배포 패키지의 구조: artex-<버전>-<os>-<arch>/artex에 방해용 파일 몇 개를 더한다.
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
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
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("압축을 푼 파일이 artex 실행 파일이 아님: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("압축을 푼 바이너리에는 실행 권한이 있어야 함")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("패키지 안에 실행 파일이 없으면 오류를 내야 함")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS 아님
		"https://evil.com/artex.zip",    // 도메인이 허용 목록에 없음
		"https://github.com.evil.com/x", // 접미사 위장
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q)은(는) 거부해야 함", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // 도메인은 대소문자를 구분하지 않는다
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q)은(는) 허용해야 하는데 오류를 냄: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh의 package_binary는 artex-<버전>-<os>-<arch>.zip을 쓰고, 버전 번호에서
	// v 접두사를 뗀다. 여기서 한 글자만 틀려도 모든 플랫폼의 원클릭 업데이트가 자산을 찾지 못한다.
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q 파싱: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("안정을 확인한 뒤에는 업데이트 마커를 반드시 지워야 함")
	}
	// 마커가 없으면 이후의 정상 재시작은 횟수를 세지 않고, 롤백을 잘못 일으키지도 않는다.
	if _, ok := readMarker(p.Marker); ok {
		t.Error("마커 읽기가 실패해야 함")
	}
	// 백업은 남겨야 한다. 사용자가 여전히 수동으로 롤백할 수 있다.
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("안정을 확인한 뒤에도 이전 버전 백업은 남아 있어야 함")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 일반 시작 경로. panic 하지도, 어떤 파일을 건드리지도 않아야 한다
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("마커가 없으면 settle이 어떤 파일에도 영향을 주면 안 됨")
	}
}

// roundTripFunc 는 네트워크 없이 요청을 받아 보는 가짜 RoundTripper 다.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestFetchLatestUsesForkReleases 는 업데이트 확인이 원 저자가 아니라 이 포크의
// 릴리스를 묻는지 본다. 출처가 원 저자로 돌아가면 검토하지 않은 바이너리가 들어온다.
func TestFetchLatestUsesForkReleases(t *testing.T) {
	const want = "https://api.github.com/repos/LRTK-CODER/ARTEX/releases/latest"
	var got string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.2.3"}`)),
			Header:     make(http.Header),
		}, nil
	})}

	rel, err := FetchLatest(context.Background(), client)
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if got != want {
		t.Errorf("요청 URL = %q, want %q", got, want)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", rel.TagName)
	}
}
