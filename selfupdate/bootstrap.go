package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv 는 스모크 테스트로 띄운 하위 프로세스가 Bootstrap 을 바로 건너뛰게 한다.
//
// 엄밀히는 없어도 문제가 생기지 않는다. 하위 프로세스의 os.Executable() 은 artex.new 라서
// 거기서 유도한 경로가 모두 .new 접두사를 달고 실제 업데이트 파일에 닿지 않는다. 하지만 이런 우연에 기대는 것은 너무 취약하다.
// 명시적으로 끊으면 한눈에 보이고, 하위 프로세스의 쓸데없는 디스크 탐색도 한 번 줄인다.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action 은 Bootstrap 이 main 에 주는 지시다.
type Action int

const (
	// Continue: 평소대로 server 를 시작한다.
	Continue Action = iota
	// Restart: 바로 ExitRestart 로 종료해 감시 스크립트가 다시 띄우게 한다.
	Restart
)

// State 는 이번 시작 시점의 업데이트 상태를 나타낸다. /api/update/check 가 프런트엔드에
// "지난 업데이트가 성공했는지, 롤백됐는지"를 사실대로 알려 줄 때 쓴다.
type State struct {
	Pending     bool   // 바이너리 교체 뒤 아직 안정 여부를 확인하지 않음
	RolledBack  bool   // 이번 시작에서 방금 자동 롤백을 실행함
	FailedStage bool   // 임시 저장본이 검증·스모크 테스트를 통과하지 못해 버림
	Detail      string // 사용자에게 보여 줄 한 문장 설명
}

// Bootstrap 은 main 맨 앞에서 실행한다. 포트를 듣거나 데이터베이스를 열기 전에 호출해야 한다.
//
// 세 가지 경우:
//
//	① 임시 저장본 artex.new 가 있음 → 검증 + 스모크 테스트. 통과하면 바이너리를 교체하고 재시작을 요청하고, 통과하지 못하면 버리고 이전 버전으로 계속 실행
//	② 마커 파일만 남음          → 바이너리를 막 교체했다는 뜻. 시도를 한 번 센다. 연속 실패가 한도에 이르면 롤백
//	③ 아무것도 없음            → 정상 시작
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 부트스트랩 건너뜀: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged 는 "임시 저장본이 있는" 경우를 처리한다. 검증을 통과하면 바이너리를 교체하고, 실패하면 버린다.
//
// 업데이트 과정 전체에서 실행 파일을 덮어쓰는 곳은 여기뿐이고, 마지막 관문이기도 하다. 스모크 테스트가
// 다운로드 손상, 잘못 고른 아키텍처, 동적 링크 누락 같은 문제를 막는다. 실행되지 않는 바이너리를 한 번 통과시키면
// 감시 스크립트가 지치지 않고 계속 다시 띄우는데, Go 코드는 실행될 기회조차 없으니 자동 롤백도 할 수 없다.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 임시 저장한 새 버전 검증 실패, 버리고 현재 버전으로 계속 실행: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "새 버전이 검증을 통과하지 못해 버렸습니다: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 바이너리 교체 실패, 현재 버전으로 계속 실행: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "바이너리를 교체하지 못했습니다: " + err.Error()}
	}

	// 바이너리 교체 성공. 마커를 남겨 다음 시작(새 버전으로 실행)이 안정 여부를 확인하게 한다.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업데이트 마커 쓰기 실패(자동 롤백을 할 수 없음): %v", err)
	}
	log.Printf("[update] %s(으)로 바이너리 교체 완료, 재시작을 위해 종료(exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback 은 "바이너리 교체 뒤의 시작"을 처리한다. 시도 횟수를 세고, 한도를 넘으면 이전 버전으로 되돌린다.
//
// 횟수는 Go 코드가 실행된 뒤에야 늘어나므로, 이것이 다루는 것은 "실행은 되지만 초기화 중에 죽는"
// (설정 비호환, 포트 점유, DB 마이그레이션 실패) 종류의 장애다. "아예 exec 할 수 없는" 경우는 바이너리 교체 전의
// 스모크 테스트가 막는다. 둘을 합쳐야 완전하다.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// 롤백까지 실패하면 더 재시작하지 않는다. 그러지 않으면 무한 재시작에 빠진다. 마커를 지우고
			// 현재 상태대로 프로세스를 띄운다. 띄우지 못해도 사용자는 적어도 로그에서 원인을 볼 수 있다.
			log.Printf("[update] 새 버전 시작 %d회 연속 실패, 롤백도 실패: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "새 버전을 시작하지 못했고 롤백도 하지 못했습니다: " + err.Error()}
		}
		log.Printf("[update] 새 버전 시작 %d회 연속 실패, %s(으)로 롤백함, 재시작을 위해 종료(exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("새 버전을 시작하지 못해 %s(으)로 롤백했습니다", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] 업데이트 마커 갱신 실패: %v", err)
	}
	log.Printf("[update] 새 버전 시작 중(%d/%d번째 시도), 안정적으로 실행되면 업데이트를 확정함",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle 은 새 버전이 안정적으로 실행 중임을 확인하고 업데이트 마커를 지운다.
//
// main 이 HTTP 수신을 시작한 뒤 지연 호출한다. 이 시간을 버텨야 인정한다. 그러지 못하면 마커가 그대로 남아
// 다음 시작에서 시도 횟수를 계속 세다가 롤백이 일어난다.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // 업데이트 뒤의 시작이 아니면 할 일이 없다
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] 업데이트 마커 삭제 실패: %v", err)
		return
	}
	log.Printf("[update] 새 버전이 안정적으로 실행됨, 업데이트 완료(이전 버전은 %s에 보존)", p.Old)
}

// SettleDelay 는 "새 버전이 살아남았다"고 판정하는 데 필요한 실행 시간이다.
const SettleDelay = 30 * time.Second

// verifyStaged 는 임시 저장본을 검증한다. 먼저 SHA256 을 비교하고, 실제로 한 번 띄워 실행해 본다.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("체크섬 읽기: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("체크섬 계산: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256이 일치하지 않음(다운로드 손상 또는 변조)")
	}
	return smokeTest(p.New)
}

// smokeTest 는 -h 로 새 바이너리를 띄워 현재 시스템에서 정말 실행되는지 확인한다.
// 다운로드가 잘림, 잘못 고른 아키텍처(exec format error), 의존성 누락 같은 문제를 크게 막는다.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("실행 권한 부여: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("스모크 테스트 시간 초과(새 바이너리가 응답하지 않음)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("스모크 테스트 실패: %v: %s", err, snippet)
	}
	return nil
}

// swap 은 현재 바이너리를 임시 저장한 새 버전으로 바꾼다.
//
// Unix 와 Windows 모두 실행 중인 실행 파일의 rename 을 허용한다(Windows 가 막는 것은 삭제와
// 덮어쓰기이고 rename 은 해당하지 않는다). 그래서 여기서는 플랫폼별로 나누거나 자신을 먼저 멈출 필요가 없다.
func swap(p Paths) error {
	// Windows 의 rename 은 이미 있는 대상을 덮어쓰지 않으므로, 지난 업데이트가 남긴 .old 를 먼저 지워야 한다.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("이전 백업 %s 정리: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("현재 버전 백업: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 교체에 실패했는데 현재 버전은 이미 옮겨졌다. 원래대로 돌려놓아야 한다. 그러지 않으면 다음 시작에 실행 파일이 없다.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("새 버전 설치 실패(%v), 현재 버전 복구도 실패: %w", err, rerr)
		}
		return fmt.Errorf("새 버전 설치: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback 은 swap 이 백업한 이전 버전을 되돌려 놓는다.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("롤백할 백업 %s이(가) 없음: %w", p.Old, err)
	}
	// 실행되지 않는 새 버전은 바로 지우지 않고 .failed 로 옮겨 원인 조사용으로 남긴다.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("실패한 버전 옮기기: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("이전 버전 복구: %w", err)
	}
	return nil
}

// Rollback 은 /api/update/rollback 의 구현이다. 직전 버전으로 직접 되돌린다.
// 바이너리 교체만 하고, 재시작은 마찬가지로 감시 스크립트에 맡긴다(호출자가 이어서 ExitRestart 로 종료한다).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("롤백할 이전 버전이 없습니다(" + p.Old + " 없음)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("이전 버전을 실행할 수 없어 롤백을 거부함: %w", err)
	}
	// 현재 버전과 백업을 맞바꾼다. 롤백한 뒤에도 다시 되돌릴 수 있다.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("현재 버전 옮기기: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("이전 버전 설치: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] 롤백 뒤 백업 정리 실패(실행에는 영향 없음): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup 은 롤백할 이전 버전이 있는지 알려 준다. 프런트엔드가 롤백 버튼을 보일지 정할 때 쓴다.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "알 수 없는 버전"
	}
	return s
}
