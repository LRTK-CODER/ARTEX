# ARTEX 저장소 안내

이 저장소에서 일하는 모든 세션이 처음 읽는 문서다. Claude Code와 Codex가 같은 내용을 읽는다.

## 현재 상태

- ARTEX는 AI 자율 보안 테스트 시스템이다. Go 백엔드와 Next.js 프런트엔드(`web/`)로 되어 있고, Autumn-27/ARTEX를 포크한 저장소다.
- 백엔드: 진입점은 `cmd/artex`(얇다)이고 실제 조립은 `server` 패키지가 한다. 주 저장소는 PostgreSQL(`db` 패키지, `pgx`)이다. LLM·에이전트 엔진 추상화는 `norma` 의존성에 있고 ARTEX가 그 위에 배선한다. 패키지 구조와 의존 방향은 `.claude/rules/architecture.md`가 정본이다.
- 프런트엔드: `web/`의 Next.js 16(App Router)·React 19·TypeScript·Tailwind·shadcn/ui. 빌드 시 정적 export가 백엔드 바이너리에 내장된다.

## 언어

- 문서, 커밋 본문, PR, 이슈, 코드 주석: 한국어.
- 식별자, 파일명, 브랜치 이름, 패키지 이름: 영어.
- 커밋·PR 제목의 Conventional Commits type(`feat`, `docs` 등): 영어. 설명은 한국어.
- 기존 코드에 영어 주석이 남아 있다. 그 파일을 고칠 때 손대는 부분의 주석을 한국어로 바꾸고, 건드리지 않는 파일을 한꺼번에 번역하지는 않는다.

## 규칙 문서

작업 규칙의 정본이다. Claude Code는 자동으로 읽는다. Codex처럼 `.claude/rules/`를 자동으로 읽지 않는 도구는 해당 작업 전에 직접 연다.

- `.claude/rules/branching.md`: 브랜치를 만들거나, 커밋·push하거나, PR을 열 때.
- `.claude/rules/orchestration.md`: 워커로 일하거나, 역할과 무엇을 소유자에게 물어야 할지 정할 때.
- `.claude/rules/issues.md`: 이슈나 PR 제목·본문을 쓰거나 리뷰할 때.
- `.claude/rules/coding-style.md`: 코드를 쓰거나 고치기 전에.
- `.claude/rules/go-style.md`: Go 파일을 쓰거나 고치기 전에.
- `.claude/rules/typescript-style.md`: 프런트엔드(`web/`) 파일을 쓰거나 고치기 전에.
- `.claude/rules/architecture.md`: 패키지·모듈을 설계하거나 Go 파일을 쓰기 전에.
- `.claude/rules/testing.md`: 테스트를 쓰거나 고치거나 리뷰하기 전에.
- `.claude/rules/secure-coding.md`: LLM·외부 도구·분석 대상을 다루는 코드를 쓰거나 리뷰하기 전에.

`paths`가 있는 규칙은 해당 파일을 읽을 때만 로드된다. 새 파일을 만들거나 설계·리뷰할 때는 `coding-style.md`, `architecture.md`와 언어별 문서(`go-style.md`·`typescript-style.md`)를 직접 연다. 테스트를 쓰거나 리뷰할 때는 `testing.md`도 연다.

## 역할

main 체크아웃의 세션은 PM이고, worktree의 세션은 워커다. 자세한 것은 `.claude/rules/orchestration.md`. PM 절차(워커 띄우기·기다리기·병합·정리, 이슈 명령)는 PM skill `.claude/skills/pm/SKILL.md`에 있다.

## 꼭 지킬 것

hook이 막는 것은 일부뿐이다. main 커밋·push와 force push는 Claude Code hook과 git hook이 막는다. PR 병합은 워커 세션에서만 Claude Code hook이 막는다. squash merge, 소유자 승인, 비밀값은 어떤 hook도 막지 않으니 스스로 지킨다. Codex에는 Claude Code hook이 적용되지 않고 git hook만 적용된다.

- main에 직접 커밋하거나 push하지 않는다. main에서 딴 브랜치에서 PR로 올린다.
- force push를 하지 않는다. `--force-with-lease`도 포함이다.
- PR 병합은 소유자 승인 뒤 squash merge로만 한다.
- 비밀값(DB 비밀번호, LLM API 키, 채널 자격 증명)은 커밋하지 않는다. 환경 변수나 로컬 설정(`config.json`, `.env`, 앱 안 설정)으로 넣는다. 정본은 `.claude/rules/secure-coding.md`.
- 막히지 않았다고 허용된 것은 아니다. 우회하지 말고 규칙 문서를 따른다.

## 명령

백엔드(Go):

```
go build ./...                                   # 전체 빌드
go vet ./...                                     # 정적 검사(커밋·PR 전에 돌린다)
gofmt -l .                                       # 형식 어긋난 파일 목록(빈 출력이면 통과)
gofmt -w .                                       # 형식 고치기
go test ./...                                    # 전체 테스트
go test -short ./...                             # testing.Short()로 거른 느린 테스트 빼고(지금은 그런 테스트가 없어 go test ./...와 같다)
go test -race ./...                              # 경쟁 상태 검사
go test ./server/ -run TestName                 # 특정 패키지의 특정 테스트
ARTEX_PG_DSN=postgres://... go test ./db/...     # 실제 PostgreSQL이 필요한 테스트
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788   # 백엔드만 실행
./dev.sh                                         # 백엔드(:8787)+프록시(:8788)+프런트 dev(:5173) 함께
./build.sh                                       # 현재 플랫폼 단일 바이너리(프런트 내장)
./build.sh --release                             # 전체 플랫폼 교차 컴파일+패키징
```

프런트엔드(`web/`에서):

```
npm install                                      # 의존성 설치
npm run dev                                      # next dev 핫리로드
npm run build                                    # next build
npm run build:static                             # 정적 export(백엔드 내장용 web/out)
npm run check                                    # biome 형식·린트 검사
npm run check:fix                                # biome 자동 수정
npx tsc --noEmit                                 # 타입 검사(strict)
```

hook 테스트:

```
python3 -m unittest discover -s .claude/hooks/tests   # .claude hook·스크립트 테스트
```

## 실행과 설정

- 데이터베이스는 PostgreSQL이 필요하다. 연결은 `config.json`의 `database`나 환경 변수 `ARTEX_PG_DSN`으로 준다(`ARTEX_PG_DSN`이 우선). `config.json`에는 `database`와 `skill_dir`만 있고, 그 밖의 설정은 환경 변수나 앱 안 설정으로 받는다. 예시는 `config.example.json`.
- 탐색에는 LLM이 필요하다. 키는 환경 변수 `ANTHROPIC_API_KEY`·`OPENAI_API_KEY`(또는 `ARTEX_LLM_*`)로 주거나 UI에서 설정한다. Docker 배포 변수 예시는 `.env.example`.
- 설치·배포 방법(일괄 스크립트, Docker, 단일 바이너리)은 `README.md`와 `install.sh`에 있다.

## 검사와 CI

- 커밋할 때 git hook(`.githooks/pre-commit`)이 스테이징된 `*.go`에 gofmt 형식 검사를, 스테이징된 web 파일에 biome 검사를 돌린다. Edit·Write에는 PostToolUse hook(`.claude/hooks/format_code.py`)이 gofmt·biome로 자동 정리한다.
- GitHub 무료 플랜이라 서버 쪽 브랜치 보호와 PR 필수 검사가 없다. CI는 태그(`v*`)를 밀 때 도는 릴리스 워크플로(`.github/workflows/release.yml`, 교차 컴파일·릴리스·Docker)뿐이다. **PR·push에 도는 CI는 없다.** 그래서 `go vet ./...`, `go test ./...`, `npm run check`는 커밋·PR 전에 스스로 돌린다. 병합 전 재채점은 검증 워커가 `.claude/scripts/pr_gate.py`로 한다(`.claude/skills/pm/SKILL.md`).

