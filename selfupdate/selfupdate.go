// Package selfupdate 는 ARTEX 화면의 원클릭 업데이트를 구현한다. GitHub Release 에서 새 버전
// 바이너리를 받아 검증·임시 저장하고, 다음 시작 때 원자적으로 바이너리를 교체한다.
//
// 전체 분담(start.sh / start.bat 참고):
//
//	시작 스크립트 = 단순한 감시 루프. "프로세스가 종료되면 종료 코드에 따라 다시 띄울지" 정하는 일만 한다
//	이 패키지      = 틀리기 쉬운 로직 전부(다운로드 / SHA256 검증 / 스모크 테스트 / 바이너리 교체 / 실패 시 롤백)
//
// 바이너리 교체를 스크립트가 아니라 Go 에 둔 이유: sha256 검증과 스모크 테스트를 sh 와 bat 에
// 두 벌(sha256sum / shasum / certutil)로 써야 하는데, 바로 여기가 가장 틀리면 안 되는 단계다. 실행되지 않는
// 바이너리로 바꾸면 감시 프로세스가 충실하게 계속 다시 띄우고, 사용자는 서버에 들어가 손으로 고칠 수밖에 없다.
//
// 업데이트 한 번은 프로세스 시작을 세 번 거친다:
//
//	① 이전 버전 server 가 /api/update/apply 를 받음 → 다운로드·검증 → artex.new 로 임시 저장 → exit 75
//	② 스크립트가 이전 버전을 다시 띄움 → Bootstrap 이 artex.new 를 발견 → 검증+스모크 테스트 → 바이너리 교체 → exit 75
//	③ 스크립트가 다시 띄움, 이제 새 버전 → Bootstrap 이 시도를 한 번 기록 → 시작에 성공하면 마커를 지움
//
// 어느 단계든 실패하면 이전 버전으로 돌아간다. ②에서 검증을 통과하지 못하면 임시 저장본을 지우고 이전 버전으로 계속 실행한다. ③에서 3회 연속
// 마커를 지울 때까지 살아남지 못하면(시작하지 못하고 죽으면) artex.old 를 자동으로 되돌려 놓는다.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart 는 "감시 프로세스가 나를 다시 띄워 달라"는 종료 코드다(EX_TEMPFAIL). 시작 스크립트는 이 값을 보면
// 바로 다시 실행하고 충돌 백오프에 세지 않는다. 0 은 사용자가 정상적으로 멈춘 것(스크립트가 루프를 끝냄)이고, 나머지는 모두 충돌로 본다.
const ExitRestart = 75

// maxAttempts 는 바이너리 교체 뒤 허용하는 시작 시도 횟수다. 새 버전은 시작할 때마다 횟수를 1 늘리고,
// settleDelay 를 버티면 마커를 지운다. maxAttempts 번 연속으로 죽으면 새 버전이 아예 시작되지 않는다는 뜻이므로 자동 롤백한다.
const maxAttempts = 3

// Paths 는 업데이트 한 번에 관련된 모든 파일이다. 모두 **실행 파일이 있는 디렉터리** 아래에 둔다.
// 일부러 CWD 를 쓰지 않는다. 서비스로 실행하면 작업 디렉터리가 / 나 임의 경로일 수 있어, CWD 를 쓰면 임시 저장본이
// 엉뚱한 곳에 생기고 바이너리 교체 로직이 그대로 무력해진다.
type Paths struct {
	Dir     string // 실행 파일이 있는 디렉터리
	Current string // 현재 실행 중인 바이너리        artex      / artex.exe
	New     string // 임시 저장한 새 버전            artex.new  / artex.new.exe
	Sum     string // 새 버전의 sha256(hex)  artex.new.sha256 / artex.new.exe.sha256
	Old     string // 바이너리 교체 전에 백업한 이전 버전      artex.old  / artex.old.exe
	Marker  string // 업데이트 상태 마커            artex.upgrade.json
}

// ResolvePaths 는 현재 실행 파일을 기준으로 모든 업데이트 경로를 유도한다.
//
// Windows 에서는 .new/.old 도 .exe 확장자를 달아야 한다. 그러지 않으면 스모크 테스트와 바이너리 교체 뒤 실행이 모두 실패한다.
// 그래서 확장자를 먼저 떼고 이어 붙여야 두 플랫폼의 이름이 대칭이 된다.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 찾기: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows 에서는 ".exe", Unix 에서는 보통 비어 있다
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker 는 바이너리 교체 한 번의 진행 상황을 기록한다. 새 버전이 시작되지 않을 때 자동 롤백을 일으키는 데 쓴다.
type marker struct {
	From     string `json:"from"`     // 업데이트 전 버전
	To       string `json:"to"`       // 대상 버전
	Attempts int    `json:"attempts"` // 바이너리 교체 뒤 시작을 시도한 횟수
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged 는 임시 저장본을 지운다. 바이너리 교체 성공, 검증 실패, 사용자 취소가 모두 이 함수를 지난다. 남은
// artex.new 가 다음 시작 때 다시 시도되지 않게 하기 위해서다.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions 는 두 버전 번호를 비교해 -1/0/1(a<b / a==b / a>b)을 돌려준다.
// ok=false 는 적어도 한쪽이 비교할 수 있는 버전 번호가 아니라는 뜻이다(예: 로컬 개발 빌드의 "dev" 나
// git describe 가 만든 "0.3.7-2-gabc1234-dirty"). 이때 호출자는 원클릭 업데이트를 꺼야 한다.
// 그러지 않으면 개발 중인 빌드를 정식 버전으로 "업데이트"해 커밋하지 않은 변경을 덮어쓴다.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion 은 "v0.3.7" / "0.3.7" 형식의 버전 번호를 [3]int 로 파싱한다.
//
// 깔끔한 세 자리 형식만 받는다. build.sh 는 tag 가 아닌 빌드에서 git describe 로
// "0.3.7-2-gabc1234" 같은 접미사 붙은 버전을 만든다. 이런 버전은 0.3.7 로 취급하지 말고 비교할 수 없다고 판정해야 한다.
// 그러지 않으면 개발 빌드가 "이미 최신"으로 잘못 판정되거나 정식 버전에 덮어써진다.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker 는 프로세스가 컨테이너 안에서 실행 중인지 알려 준다. Docker 에서는 바이너리 교체가 컨테이너의 쓰기 가능 계층에 쓰므로,
// `docker compose up -d` 로 컨테이너를 다시 만들면 이미지에 든 버전으로 돌아간다. 이는 의도한 동작이지만
// (그때 사용자는 원래 새 이미지를 받는 중이다), 프런트엔드가 이를 근거로 사정을 분명히 알려 줄 수 있어야 한다.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
