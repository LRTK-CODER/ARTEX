# 오케스트레이션 규칙

ARTEX 작업은 PM 한 명과 Orca 감독 워커 여럿이 나눠 한다. 브랜치·병합은 `branching.md`, 이슈·PR 형식은 `issues.md`를 따른다. 이 문서는 누가 무엇을 하고, 무엇을 누구에게 묻고, 워커가 무엇을 지키는지만 다룬다. PM이 워커를 띄우고, 기다리고, 병합·정리하는 절차는 PM skill(`.claude/skills/pm/SKILL.md`)에 있다.

## 역할

| 역할 | 누가 | 할 수 있는 것 | 못 하는 것 |
|---|---|---|---|
| PM | main 체크아웃의 메인 세션(소유자와 대화) | 계획, 이슈 생성·분할, 워커 시작·감독, 리뷰, 소유자 승인 후 병합 | 직접 구현(구현은 워커에게 맡긴다) |
| 구현자 | 자기 worktree의 워커 | 코드·테스트 작성, 커밋, push, PR 생성 | 병합, 자기 worktree 밖 수정, 소유자에게 직접 질문 |
| 리뷰어 | 워커 | PR/diff 읽기, 테스트 실행, 의견을 보고서로 제출 | 파일 수정, 커밋, PR 코멘트(밖에서 보이는 작업) |
| 조사자 | 워커 | 웹·코드 조사, 보고서 작성 | 저장소 파일 수정, 커밋 |

- 역할 정의: `.claude/agents/implementer.md`, `reviewer.md`, `researcher.md`.
- 구현 워커는 동시에 최대 3개다. 도구로 강제하지 않는다. PM이 띄우기 전에 센다.
- 워커는 소유자에게 직접 묻지 않는다. 항상 `orca orchestration ask`로 PM에게 묻고, PM이 아래 기준으로 소유자에게 올린다.

## 누가 정하나

기준: **되돌리기 어렵거나, 밖에서 보이거나, 규칙 자체를 바꾸는 일은 소유자 승인.**

소유자 승인이 필요한 것:
- PR 병합
- 밖에서 보이는 작업: 결과 공유가 아닌 이슈·PR 코멘트, 이슈 닫기, 저장소 설정, 태그·릴리스
- `.claude/` 아래 규칙·hook·에이전트 정의 변경
- 설계 방향: 브리핑에 없는 구조 선택, 새 의존성(외부 분석 도구 포함)
  - 승인 요청에 적을 것: 표준 라이브러리로 대신할 수 있는가, 유지보수 상태, 라이선스, 알려진 취약점. 외부 분석 도구라면 엔진과 규칙 세트 각각의 라이선스 제한(비공개 코드 분석, 자동 실행, 서비스 제공).
- 파괴적 작업: 데이터·이력 삭제

승인 없이 하는 것:
- PM의 이슈 생성, 이슈 분할, 워커 배정
- PM의 이슈 메타데이터 편집: 라벨, sub-issue, blocked-by, 본문 수정
- 워커의 PR 생성
- PM의 결과 공유 코멘트: 조사 보고서, 검증 보고서, 설계 선택, 병합 판단(`issues.md` "결과 공유 코멘트")
- 브리핑 범위 안의 구현 방법, 테스트 추가
- 리뷰 의견 반영 여부(규칙 위반이 아닌 한)

## 워커가 지킬 것

역할 정의(`.claude/agents/*.md`)는 워커 공통 행동을 이 절에 맡긴다. 역할마다 다른 것만 역할 정의에 적는다.

- 첫 단계에서 브랜치 이름을 확인한다(`git branch --show-current`). `LRTK-CODER/`로 시작하면 브리핑의 이름으로 `git branch -m`한다(Orca는 `--name`의 `/`를 `-`로 바꾸고 `LRTK-CODER/`를 붙인다).
- 파일은 자기 worktree와 "강제 장치"의 허용 경로에만 쓴다. 브리핑·보고서는 `/private/tmp/artex-orchestration/` 아래에 있다. 리뷰어·조사자는 저장소 파일을 고치지 않는다.
- 5분마다 heartbeat를 보내고, 파일을 새로 시작할 때·테스트를 돌린 뒤·끝내기 직전에 `orca orchestration check`로 PM 메시지를 확인한다.
- "누가 정하나"의 승인 목록에 해당하거나, 브리핑·이슈의 완료 기준과 범위 밖에 없는 선택은 추측하지 말고 `orca orchestration ask`로 묻는다.
- 검사를 통과시키려고 테스트를 지우거나, 건너뛰게 하거나, 규칙을 끄지 않는다. 검사가 틀렸다고 보면 고치지 말고 `orca orchestration ask`로 이유와 출력을 보내고 답을 기다린다.
- 테스트를 통과시키려고 특정 입력을 하드코딩하거나 테스트 전용 분기를 만들지 않는다. 일반 해법을 쓴다. 기대값 규칙은 `testing.md`에 있다.
- 같은 테스트가 같은 방식으로 세 번 실패하면 멈추고, 실패 출력과 가설을 `orca orchestration ask`로 보낸다.
- 결과를 주장하지 않고 증거(실행한 명령과 출력)를 낸다. 보고서와 PR 검증 섹션 모두 같다.
- 끝나면 보고서를 쓰고 `worker_done`을 정확히 한 번 보낸다(`--report-path`에 보고서). 못 끝냈으면 `--outcome failed`.
- 보고서는 `issues.md`의 이슈 작성 원칙대로 쓴다.

명령 예시. `<...>` 값은 Orca가 워커 프롬프트 앞에 붙이는 안내문에 있다.

    # heartbeat(5분마다)
    orca orchestration send --from <내 터미널> --dispatch-capability <dcap> --type heartbeat \
      --subject alive --task-id <task> --dispatch-id <dispatch> --phase implementing
    # PM 메시지 확인
    orca orchestration check --terminal <내 터미널> --json
    # 끝(정확히 한 번). 못 끝냈으면 --outcome failed
    orca orchestration send --from <내 터미널> --dispatch-capability <dcap> --type worker_done \
      --subject "<짧은 상태>" --body "<한 일, 알아낸 것, 남은 일 세 문장>" \
      --task-id <task> --dispatch-id <dispatch> --outcome succeeded --report-path <보고서 경로>

## 강제 장치

`.claude/hooks/worker_guard.py`가 워커 세션에서만 막는다. 워커 판별: 세션 프로젝트 디렉터리(`CLAUDE_PROJECT_DIR`)의 `git rev-parse --git-dir`와 `--git-common-dir`가 다르면(연결 worktree) 워커다. main 체크아웃의 PM 세션은 막지 않는다.

- PR 병합: `gh pr merge`, `gh api .../merge`
- git 대상이 자기 worktree 밖: `cd`, `git -C`, `--git-dir`, `--work-tree`, `GIT_DIR=`
- Write·Edit·NotebookEdit 경로가 자기 worktree 밖

허용하는 곳: 자기 worktree, 자기 git 디렉터리, `/tmp`, `/private/tmp`, `/var/folders`, `$TMPDIR`, `~/.claude`.

`.claude/hooks/wait_guard.py`는 워커 판별 없이 PM·워커 세션 모두에서 대기 명령을 검사한다. 대기 명령은 `wait_workers.py`(`--status` 제외)와 `orca orchestration check --wait`다. 막는 것:

- Bash 도구의 `run_in_background` 없이 띄운 대기
- 셸 `&`, `disown`, `nohup`, `setsid`로 떼어 띄운 대기
- 다른 명령과 묶은 대기(`;`, `&&`, `||`, 파이프, 줄바꿈, 하위 셸, `bash -c`). 앞에 `cd <경로> &&` 하나만 붙일 수 있다
- 같은 사용자의 `orchestration check --wait` 프로세스가 이미 돌 때 새 대기. `pgrep`으로 확인하지 못하면 막지 않는다

이유와 올바른 형태는 PM skill "기다리기"에 있다.

리뷰어·조사자는 역할 정의에서 Write·Edit·NotebookEdit 도구가 빠진다. 리뷰어는 Agent 도구도 빠진다(서브에이전트는 파일을 고칠 수 있다). 조사자는 `tools` 허용 목록에 `Agent(Explore)`를 넣어 읽기 전용인 Explore 서브에이전트만 띄운다. 조사자는 `Skill`도 허용 목록에 있어 스킬을 부를 수 있다. 스킬 안의 서브에이전트도 Explore로만 띄운다. `Agent(<유형>)` 제한은 역할이 메인 세션으로 돌 때(`--agent`나 설정의 `"agent"` 키)만 적용된다. 워커는 메인 세션으로 돈다.

### hook은 main 체크아웃 사본으로 돈다

`settings.json`의 hook 명령은 공용 git 디렉터리의 상위(main 체크아웃)에 있는 `.claude/hooks/guard.py`를 실행한다. 작업 브랜치가 자기 hook 파일을 고쳐도 자기 검사는 약해지지 않는다. main 체크아웃에 `guard.py`가 없으면 worktree 사본을 쓴다. 따라서 hook 변경은 병합된 뒤에야 모든 세션에 적용된다.

git hook(`.githooks/`)도 같은 이유로 main 체크아웃의 절대 경로를 쓴다. 설정 명령은 `branching.md`의 "강제 장치"에 있다.

`settings.json`에는 PostToolUse hook(`.claude/hooks/format_code.py`)도 있다. Edit·Write로 바꾼 `*.go`에 `gofmt -w`를, web 소스(`web/` 아래 `*.ts`·`*.tsx`·`*.js`·`*.jsx`·`*.mjs`·`*.json`·`*.css`)에 `biome check --write`를 돌리고, 고칠 수 없는 위반이 남으면 그 출력을 세션에 보여 준다. 파일은 이미 바뀐 뒤라 막지는 않는다.

### 한계

워커가 자기 worktree의 `.claude/` 설정·hook 파일을 고치는 것은 막지 않는다. `.claude/` 변경은 병합 전에 소유자 승인을 받는다.

- hook 명령이 적힌 `settings.json`은 main 체크아웃이 아니라 세션 프로젝트 디렉터리(worktree)의 사본이 읽힌다. worktree에서 `settings.json`을 고치면 그 세션의 hook 구성이 바뀐다.
- GraphQL 경로의 병합(`gh api graphql`의 `mergePullRequest`)은 막지 못한다.
- 같은 명령 안에서 대입한 셸 변수는 해석하지 못한다. `D=<밖의 경로>; git -C $D ...`는 통과한다.
- hook 내부 오류(입력을 읽지 못함, 예외)는 경고만 남기고 통과시킨다.
- 브랜치 이름 검사(`branching_guard.py`)는 이 저장소가 아닌 git 저장소에도 적용된다(`git -C <다른 저장소> checkout -b ...`).

hook은 흔한 명령 형태만 해석한다. Bash의 `echo > 경로`, `sed -i`, `cp` 같은 git 밖의 파일 쓰기와 `git clone`의 대상 경로는 검사하지 않는다. 막히지 않았다고 허용된 것은 아니다. 우회하지 말고 이 문서를 따른다.
