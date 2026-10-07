---
paths: ["**/*.go", "go.mod", "web/**/*.ts", "web/**/*.tsx"]
---

# 아키텍처 규칙

모듈을 설계하거나 Go 파일을 쓰기 전에 읽는다. 코드 한 줄 단위의 관례는 `coding-style.md`와 언어별 문서(`go-style.md`, `typescript-style.md`)를 따른다.

ARTEX는 Go 백엔드와 Next.js 프런트엔드(`web/`)로 된 AI 자율 침투 테스트 시스템이다. 이 문서는 백엔드(Go) 구조가 정본이고, 프런트엔드 구조는 마지막 절에서 따로 다룬다.

`.claude/` 아래 스크립트(시스템 python3, 표준 라이브러리만, 제품 코드가 아니다)에는 적용하지 않는다.

## 채택한 스타일

관례적인 계층형 Go를 쓴다. 헥사고날이나 포트·어댑터가 아니라, 구체 타입으로 된 패키지들을 의존 방향으로 쌓는다. 진입점(`cmd/artex`)은 얇고, 실제 조립은 `server` 패키지가 한다. 규칙의 기준은 패키지 이름이나 개수가 아니라 **의존 방향**이다.

적용했다고 말할 수 있는 기준:

- `cmd/artex`는 플래그 파싱과 수명 주기(시작·신호·재시작)만 하고, 조립은 `server`가 한다.
- 패키지 의존이 한 방향으로만 흐른다(순환 없음).

## 의존 방향

모듈은 `github.com/Autumn-27/artex`다. 패키지는 저장소 루트의 디렉터리 하나씩이다. 실제 의존 그래프(비테스트, 저장소 내부 import만):

    config, llmpool, mcphttp, selfupdate, notify, sidequestion   잎 패키지(저장소 내부 의존 없음)
    db            → config, notify, sidequestion          영속 계층의 중심(PostgreSQL)
    llmrec, enrich, intercept, report, traffic → db
    evidence      → db, traffic
    guard         → intercept
    agent         → db, guard, intercept, llmrec, sidequestion   (테스트에서만 evidence도 import한다)
    server        → 위의 거의 모두 + mcphttp, llmpool, notify, report, selfupdate
    cmd           → agent, config, selfupdate, server

규칙:

- **순환 import를 만들지 않는다.** 두 패키지가 서로를 필요로 하면 공통 타입을 잎 쪽(예: `config`)이나 양쪽이 의존하는 아래 패키지로 내린다. Go 컴파일러가 순환을 막지만, 억지로 피하려고 타입을 엉뚱한 곳에 두지 않는다.
- **`cmd/artex`는 얇게 둔다.** 조립·배선 코드는 `server`에 둔다(`server/manager.go`의 `NewManager`, `server/server.go`의 `New`, `server/assembly.go`). `cmd`에서 도메인 패키지(`db`, `agent` 등)를 직접 조립하지 않는다.
- **도메인 패키지는 `server`를 import하지 않는다.** `server`가 HTTP/API 계층이자 조립 루트다. 아래 계층이 위를 알면 조립을 건너뛰게 된다.
- **영속은 `db`에 모은다.** 다른 패키지는 `*db.DB`를 받아 쓴다. 새 SQL은 `db`에 둔다. traffic 프록시 저장소는 예외로 자기 SQLite를 가진다. `db` 밖에서 PostgreSQL SQL을 쓰는 기존 예외(`evidence/store.go`, `server/finding_retests.go`, `server/finding_workflow.go`, `server/finding_traffic.go`)가 남아 있다. 이를 본떠 `db` 밖에 SQL을 새로 두지 않는다.
- 새 기능 패키지는 자기가 쓰는 것만 import하고, 위 그래프의 방향을 지킨다.

## 저장소와 외부 엔진

- **주 저장소는 PostgreSQL이다**(`db` 패키지, `pgx` 드라이버). 과거 SQLite graph 단일 파일은 대체됐다. 코드나 주석에 "SQLite graph store"가 남아 있으면 옛 설명이다. SQLite는 traffic 프록시 저장소(`traffic/`) 한 곳에만 남는다.
- **LLM·에이전트 엔진 추상화는 `norma` 의존성**(`github.com/Autumn-27/norma`, 같은 저자)에 있다. `norma/llm`의 `Provider`·`Format`, `norma/agentcore`, `norma/tool`(Bash 도구), `norma/mcp`, `norma/permission`, `norma/hook` 등이다. ARTEX는 그 위에 배선만 한다. LLM 제공자 추상화를 ARTEX 안에 새로 만들지 않는다. `llm.Provider`를 감싸는 장식자(실패 전환 `llmpool`, 기록 `llmrec`)로 기능을 더한다.

## 인터페이스와 다형성

- **기본은 구체 타입이다.** 구현이 하나뿐인데 인터페이스를 기계적으로 만들지 않는다. `db`에 리포지토리 인터페이스를 씌우지 않는다. 호출자는 `*db.DB`를 그대로 쓴다.
- 인터페이스는 두 경우에만 만든다.
  - **테스트 이음매(DI seam):** 외부 경계(LLM, SMTP, HTTP RoundTripper, 하위 프로세스)를 테스트에서 손으로 만든 가짜로 바꿔야 할 때. 예: `agent`의 `FindingRecorder`, `sidequestion`이 소비하는 `llm.Provider`, `server`의 `mcpClient`.
  - **실제 다형성:** 구현이 여럿인 것. 예: `notify`의 `Channel`(IM·이메일 채널 어댑터).
- 인터페이스는 **쓰는 쪽** 패키지에 좁게 정의한다. 메서드는 호출 목적 단위로 적게 둔다. 구현체가 인터페이스를 import해 상속처럼 쓰지 않는다(Go는 구조적 타이핑이라 명시 선언이 필요 없다).
- 패키지 밖에서는 공개 식별자만 쓴다. 비공개는 소문자로 둔다.

## 의존성 주입과 배선

- 의존성은 생성자나 함수 인자로 받는다. 시계·난수·LLM·HTTP 클라이언트도 인자로 받아 테스트에서 가짜를 넣을 수 있게 한다.
- **조립은 `server`가 한곳에서 손으로 한다**(`NewManager` → 저장소·traffic·enrich 개통, `New` → HTTP `Server` 구성, `assembly.go` → 에이전트·도구·도메인 레지스트리 배선). DI 프레임워크를 쓰지 않는다.
- 프로세스 전역 상태는 꼭 필요한 곳에만 두고 문서로 남긴다. 현재 예외는 `llmpool`의 회로 차단기 레지스트리(프로세스 전역, 상태를 PG에 미러링하고 부팅 때 복원)다. 새 전역 상태는 설계 승인을 받는다(`orchestration.md` "누가 정하나").

## LLM 경계

ARTEX는 자율 침투 테스트 도구라서 **LLM이 도구를 골라 명령을 실행하는 것이 설계의 핵심**이다. 그래서 LLM 출력을 막는 대신 피해를 제한하는 방식으로 설계한다. 구체 규칙과 확인 방법은 `secure-coding.md`가 정본이다. 여기서는 구조만 적는다.

- 도구 호출은 실행 전에 **guard PreToolUse 훅**(`guard` 패키지)을 지난다. 허용·차단은 하드코딩이 아니라 **DB에 든 가로채기 규칙**(`intercept`)으로 정한다. 내장 규칙은 `[내장]`으로 시드되고 사용자가 편집·삭제할 수 있다.
- 도구 호출 중 패닉은 복구(recover)로 가둔다(`agent`의 가드). 한 도구가 죽어도 에이전트 루프가 멈추지 않는다.
- LLM이 준 값으로 명령을 조립할 때는 셸 인용(`shellQuote`)으로 감싸고, DB 질의는 자리표(`$1`)로 파라미터화한다. 문자열 이어 붙이기로 질의·경로·명령을 만들지 않는다.
- LLM 요청·응답은 `llmrec` 장식자가 PG에 기록한다.

## 외부 도구와 프로세스

- **되도록 라이브러리로 프로세스 안에서 돈다.** MITM 프록시는 `go-mitmproxy`를 라이브러리로(`traffic`), DNS는 `dnsx`를 라이브러리로(`enrich`) 쓴다. 하위 프로세스를 띄우지 않는다.
- 에이전트가 외부 명령(nmap·curl·playwright 등)을 쓸 때는 **`norma`의 Bash 도구**를 지난다. 타임아웃·보안 기준선·프록시 환경 변수·출력 상한은 `norma`가 맡는다. ARTEX의 명령형 커스텀 도구도 이 Bash 바탕을 재사용한다.
- ARTEX Go 코드가 직접 `os/exec`를 쓰는 곳은 드물다(커스텀 script 도구의 `execPython`, `python` 탐지, selfupdate 바이너리 점검). 직접 쓸 때 지킬 것(타임아웃을 `context`로, 인자는 리스트로, 환경 변수 허용 목록)은 `secure-coding.md` "외부 도구 실행"에 있다.

## 설정

- `config` 패키지는 `database`와 `skill_dir`만 다룬다. LLM 등 나머지는 환경 변수나 앱 안 설정(PG의 `llm_profiles`)으로 받는다.
- 우선순위: PostgreSQL DSN은 `ARTEX_PG_DSN` > 설정 파일 `database.dsn` > 분리 필드. 설정 파일 경로는 `ARTEX_CONFIG` > 작업 디렉터리의 `config.json` > 실행 파일 옆 `config.json`.
- 비밀값(DB 비밀번호, LLM 키, 채널 자격 증명)은 로그·UI·`--json` 출력에서 가린다. 정본은 `secure-coding.md` "설정과 비밀값".

## 오류와 로깅

- 오류는 Go 관례대로 `fmt.Errorf("...: %w", err)`로 원인을 이어 올린다. 오류 프레임워크를 쓰지 않는다. 밖에서 분기할 오류는 센티넬(`errors.Is`) 또는 타입(`errors.As`)으로 가른다.
- 로깅은 표준 라이브러리 `log`를 쓴다(`slog`를 새로 들이지 않는다). 메시지 앞에 하위 시스템 태그를 붙인다: `log.Printf("[traffic] ...")`, `[engine]`, `[llmpool]`, `[notify]`, `[mcp]` 등.
- 백엔드 로그는 `server.StartLogCapture()`가 메모리 링에도 복제해 `/logs` 페이지에 보인다. 로그는 stderr에도 그대로 나간다.

## 설계 권고

아래는 권고다. 도구로 검사하지 않고 리뷰에서 본다.

- 함수와 패키지의 책임은 "동작 하나"가 아니라 "변경 이유 하나"로 본다.
- 이름과 계약에 드러나지 않는 상태 변경을 하지 않는다. 조회 함수는 데이터를 바꾸지 않는다.
- 타입은 상태와 그 상태를 지키는 동작을 함께 묶을 이유가 있을 때만 만든다. 상태 없는 동작은 패키지 함수로 둔다.
- 같은 업무 규칙은 한곳에 둔다. 겉모양이 비슷해도 변경 이유가 다른 코드는 합치지 않는다.
- 추상화는 외부 의존성 격리, 테스트, 확인된 확장 요구처럼 구체적인 이유가 있을 때만 만든다.
- 나중을 위한 뼈대(쓰이지 않는 인터페이스, 빈 확장 지점)를 미리 만들지 않는다.
- 바뀌지 않는 값을 설정으로 빼지 않는다. 줄 수를 맞추려고 함수나 파일을 나누지 않는다.

## 프런트엔드(`web/`) 구조

- Next.js 16 App Router, React 19, TypeScript(strict), Tailwind 4, shadcn/ui. 한 줄 관례는 `typescript-style.md`를 따른다.
- 라우트는 `src/app/` 아래 라우트 그룹(`(auth)`, `(main)`, `(external)`)으로 나눈다. 공용 컴포넌트는 `src/components/`, 생성물(`ui/`, `calendar/`)은 건드리지 않는다(biome 검사에서도 제외된다).
- 백엔드 호출은 dev에서 `/api/*`가 Go 백엔드(`:8787`)로 리라이트된다(`next.config.mjs`). API 기본 경로를 컴포넌트에 하드코딩하지 않는다.
- import 순환을 만들지 않는다(biome `noImportCycles`가 막는다).

## 도구로 강제하는 것

ARTEX에는 Go용 린터·타입 검사·PR CI가 없다. 강제 수단이 적으므로 대부분은 리뷰와 아래 hook·pr_gate로 지킨다.

| 규칙 | 수단 | 시점 |
|---|---|---|
| Go 형식 | gofmt | pre-commit(스테이징된 `*.go`), PostToolUse hook(`format_code.py`). CI에는 없다 |
| 프런트 형식·린트 | biome | pre-commit(스테이징된 web 파일), PostToolUse hook. web import 순환은 `noImportCycles` |
| 의존 방향, 순환 없음, 인터페이스가 꼭 필요한지, 조립이 `server`에 모였는지 | 리뷰 | 늘 |
| 병합 전 테스트·형식 재채점 | `.claude/scripts/pr_gate.py` | 검증 워커가 실행(`pm/SKILL.md`) |

- Go 린터(golangci-lint 등)나 PR CI를 들이는 것은 새 의존성·설정이므로 `orchestration.md` "누가 정하나"를 따른다.
- 도구가 없으므로 `go vet`과 테스트는 커밋·PR 전에 스스로 돌린다. 명령은 `AGENTS.md` "명령"에 있다.
