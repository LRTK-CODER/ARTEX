---
paths: ["**/*.go", "go.mod"]
---

# Go 코딩 규칙

Go 코드를 쓰거나 고칠 때 따른다. 언어와 무관한 원칙은 `coding-style.md`에 있고 여기서 되풀이하지 않는다. 이 문서는 그 원칙을 Go에서 어떻게 지키는지와 도구가 잡지 못하는 Go 관례만 적는다. 근거는 Effective Go, Go Code Review Comments, 표준 라이브러리 관례다.

`.claude/` 아래 스크립트(python3)에는 적용하지 않는다.

## 도구와 문서의 분담

- **형식은 `gofmt`가 정본이다.** 형식에 대한 리뷰 코멘트는 남기지 않는다. `gofmt -w`가 고친다. pre-commit과 PostToolUse hook이 돌린다.
- ARTEX에는 golangci-lint·staticcheck 같은 린터가 없다. 그래서 gofmt가 잡지 못하는 것(안 쓰는 코드, 의심스러운 구문, 네이밍)은 `go vet`과 **리뷰**로 본다. 커밋·PR 전에 `go vet ./...`를 스스로 돌린다.
- 새 린터를 들이는 것은 새 의존성이다(`orchestration.md` "누가 정하나").

## 문서 주석(godoc)

- 공개 식별자(대문자로 시작)에는 문서 주석을 단다. **주석은 식별자 이름으로 시작하고, 본문은 한국어로 쓴다.** godoc이 이름으로 시작하는 문장을 그 식별자의 설명으로 알아본다.
- 패키지마다 패키지 주석을 한 파일에 둔다(`// Package traffic 은 ...`).
- 타입 힌트(시그니처)에 있는 내용을 되풀이하지 않는다. 단위·범위·부작용·올리는 오류처럼 시그니처가 말하지 못하는 것을 적는다.
- 비공개라도 로직이 뻔하지 않으면 주석을 단다. "왜"를 적는다(`coding-style.md` "주석").

```go
// ParseConfig 는 설정 바이트를 읽어 검증한 Config 로 바꾼다.
// 값이 범위를 벗어나면 오류를 올린다. 빈 입력은 기본값 Config 를 돌려준다.
func ParseConfig(raw []byte) (Config, error) {
```

## 오류

- 오류는 마지막 반환값으로 돌려준다. 실패를 제로값으로 숨기지 않는다(`coding-style.md` "오류 처리").
- 감싸 올릴 때는 `fmt.Errorf("문맥: %w", err)`로 원인을 잇는다. 밖에서 분기할 오류만 `%w`로 감싸고, 분기할 일이 없으면 `%v`로 문맥만 더해도 된다.
- 밖에서 분기할 조건은 센티넬 오류(`var ErrNotFound = errors.New(...)`, `errors.Is`)나 오류 타입(`errors.As`)으로 가른다. 오류 메시지 문자열을 비교하지 않는다.
- 오류를 버리지 않는다. `_ = f()`로 삼킬 때는 왜 무시해도 되는지 주석을 단다(`go vet`과 리뷰가 본다).
- `panic`은 되돌릴 수 없는 프로그래밍 오류에만 쓴다. 보통의 실패는 오류로 돌려준다. 격리 지점(에이전트 도구 호출처럼)에서 `recover`로 가두는 것은 이유를 주석으로 단다.

## context

- 외부 경계(네트워크, 하위 프로세스, LLM, DB)를 넘는 함수는 첫 인자로 `ctx context.Context`를 받는다. 구조체 필드에 `context.Context`를 저장하지 않는다.
- `context.Context`를 타고 취소·마감을 지킨다. 외부 호출의 타임아웃은 `context.WithTimeout`으로 둔다(`coding-style.md` "자원과 값 표현").
- `context.Background()`·`context.TODO()`는 진입점(`main`, 테스트, 요청 처리 시작)에서만 만든다.

## 자원과 동시성

- 연 것은 `defer`로 닫는다. 쓰기 자원은 `Close`의 오류도 확인한다(버퍼가 늦게 비워질 수 있다).
- goroutine은 누가 언제 멈추는지가 분명할 때만 띄운다. `context`나 채널로 끝낼 수 있게 하고, 떠돌이 goroutine을 남기지 않는다.
- 공유 상태는 뮤텍스나 채널로 지킨다. 테스트와 CI 대신, 동시성 코드는 `go test -race`로 스스로 확인한다.

## 이름과 타입

- Go 네이밍을 따른다: `MixedCaps`(밑줄 없음), 머리글자는 대소문자를 일관되게(`ID`, `URL`, `HTTP`, `API` — `Id`·`Url`로 쓰지 않는다). 리시버 이름은 짧고 타입마다 같게 쓴다.
- "인터페이스를 받고 구조체를 돌려준다." 인터페이스는 쓰는 쪽에서 좁게 정의한다(`architecture.md` "인터페이스와 다형성").
- 제로값이 쓸모 있게 타입을 설계한다. 생성자가 꼭 필요할 때만 `New...`를 둔다.
- 긴 함수에서 이름 있는 반환값으로 암시적 `return`(naked return)을 쓰지 않는다. 짧은 함수의 `defer`로 오류를 손볼 때만 쓴다.
- 같은 타입의 인자가 여럿이면 뜻이 헷갈리지 않게 한다. 불리언 여러 개를 위치로 넘기지 말고 옵션 구조체로 묶는다.

```go
type CopyOptions struct {
	Overwrite      bool
	FollowSymlinks bool
}

// Copy 는 src 를 dst 로 복사하고, 새로 썼으면 true 를 돌려준다.
func Copy(src, dst string, opts CopyOptions) (bool, error) {
```

## 패키지 배치

- 패키지 이름은 짧은 소문자 한 단어다(`db`, `agent`, `traffic`). 밑줄·대문자·복수형을 쓰지 않는다.
- 패키지 밖에서는 공개 식별자만 쓴다. 내부 타입·함수는 소문자로 둔다.
- 한 파일이 커지면 같은 패키지 안에서 관심사별 파일로 나눈다(`db/assets.go`, `db/config.go`). 줄 수만 보고 나누지 않는다(`architecture.md` "설계 권고").
- 새 패키지는 의존 방향을 지킬 때만 만든다(`architecture.md` "의존 방향").

## 테스트와 제품 코드

- 제품 코드는 자기가 테스트 중인지 알아내 분기하지 않는다. `testing.Testing()`이나 테스트 전용 환경 변수를 제품 경로에서 보지 않는다.
- 제품 코드는 테스트를 통과시키려고 호출 횟수에 따라 다른 값을 돌려주거나, 늘 참을 돌려주는 비교 메서드를 두지 않는다. 테스트의 손으로 만든 가짜가 미리 정한 응답을 차례로 돌려주는 것은 대역이라 괜찮다.
- 테스트 작성 규칙은 `testing.md`에 있다.

## 억제와 생성 코드

- 빌드 태그는 맨 위에 `//go:build <태그>`로 둔다(ARTEX는 `embedui`/`!embedui`만 쓴다). 새 태그는 이유를 주석으로 단다.
- `go:generate`로 만든 코드에는 생성물임을 표시하고(`// Code generated ... DO NOT EDIT.`) 손으로 고치지 않는다. 생성물은 리뷰·형식 지적 대상이 아니다.
