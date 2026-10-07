---
name: pm
description: ARTEX PM 절차. PM(main 체크아웃 세션)이 이슈를 만들고, 워커를 띄우고, 결과를 기다리고, 리뷰하고, 병합·정리할 때 쓴다. 워커 세션은 쓰지 않는다.
---

# PM 절차

main 체크아웃의 PM 세션에서 쓴다. 역할, 누가 정하나, 워커가 지킬 것은 `.claude/rules/orchestration.md`, 브랜치와 병합은 `branching.md`, 이슈 작성 원칙과 PR 형식은 `issues.md`를 따른다. 이 문서는 PM이 실제로 치는 명령과 순서만 다룬다.

브리핑·보고서는 `/private/tmp/artex-orchestration/briefs/`와 `reports/`에 둔다.

## 모델과 effort

Opus 5.5의 기본 effort는 medium이다. xhigh는 효과가 측정된 경우에만 쓴다.

| 역할 | 모델 · effort |
|---|---|
| PM | Opus 5.5 medium. 어려운 설계 판단만 high |
| 구현자(복잡) | Opus 5.5 medium으로 시작. 테스트가 실패하면 high로 다시 실행. xhigh는 30분 넘는 무인 장기 작업만 |
| 구현자(단순) | Opus 5.5 low~medium |
| 리뷰어 | Opus 5.5 medium. 중요한 변경만 high |
| 조사자 | Opus 5.5 low~medium. 단순 조회는 `start_worker.py --model haiku`로 Haiku 4.5 |

- 사용자 설정의 `effortLevel`은 Opus 5.5에 적용되지 않는다.
- 에이전트 frontmatter의 `model`과 도구 제한은 메인 세션(`claude --agent <역할>` 또는 설정의 `"agent"` 키)에도 적용된다.
- 메인 세션에서는 `--model`이 frontmatter의 `model`보다 우선한다. frontmatter의 `model`은 `--model`이 없을 때만 쓰인다.
- frontmatter의 `effort`는 서브에이전트로 쓸 때만 적용된다. 메인 세션에는 적용되지 않으므로 워커를 띄울 때 `--effort`로 넘긴다. `start_worker.py`가 역할별 기본값(구현자·리뷰어 medium, 조사자 low)을 넘긴다.

## 이슈 만들기

본문은 템플릿에서 앞머리(`---` 사이)를 떼고 채워서 파일로 넘긴다.

    awk 'f>=2; /^---$/{f++}' .github/ISSUE_TEMPLATE/work.md > /tmp/issue-body.md
    # /tmp/issue-body.md 를 채운다
    gh issue create --title "<한국어 제목>" --body-file /tmp/issue-body.md --label feature

라벨은 `--label`로 늘 직접 붙인다. `--template`(`-T`)은 마크다운 템플릿을 읽지만 편집기를 여는 대화형 경로라서 PM 자동화에는 `--body-file`을 쓴다.

부모와 sub-issue, 의존 관계:

    gh issue create --title "..." --body-file body.md --label feature --parent 100   # 100의 sub-issue로 만든다
    gh issue create --title "..." --body-file body.md --label feature --blocked-by 101
    gh issue edit 100 --add-sub-issue 123,124     # 이미 있는 이슈를 하위로 붙인다
    gh issue edit 123 --parent 100                # 같은 일을 하위 쪽에서
    gh issue edit 123 --remove-parent
    gh issue edit 124 --add-blocked-by 123        # 123이 끝나야 124를 시작한다

상태 라벨과 닫기:

    gh issue edit 123 --add-label needs-decision
    gh issue edit 123 --remove-label needs-decision
    gh issue close 123 --reason "not planned" --comment "<사유>"
    gh issue close 123 --duplicate-of 45 --comment "<사유>"

조사 이슈는 보고서 전문을 코멘트로 올린 뒤 소유자 승인을 받아 닫는다. 올릴 내용과 제외할 것은 `issues.md` "결과 공유 코멘트"를 따른다.

    gh issue comment 123 --body-file <보고서 사본>
    gh issue close 123 --reason completed

## 브리핑

워커는 브리핑과 이슈만 보고 일한다. 브리핑에 최소한 다음을 적는다.

- 이슈 번호(`gh issue view <번호>`로 읽게 한다)
- 브랜치 이름(`<type>/issue-<번호>`)
- PR 제목과 본문(`issues.md`의 "PR")
- 보고서 경로(`/private/tmp/artex-orchestration/reports/<이름>.md`)
- 실험 디렉터리(`/private/tmp/artex-orchestration/scratch-<이름>/`)
- `orca orchestration run-create`를 하지 말 것(run은 PM이 만든다)

워커에게 PR을 템플릿으로 열게 할 때는 아래 명령을 적어 준다.

    cp .github/pull_request_template.md /tmp/pr-body.md
    # /tmp/pr-body.md 를 채운다(주석 줄은 지운다)
    gh pr create --title "<type>: <한국어 설명>" --body-file /tmp/pr-body.md

## 워커 띄우기

1. Run을 만든다(세션에 하나).

       orca orchestration run-create --objective "<목표 한 줄>" --json

2. 브리핑을 쓰고 워커를 띄운다. 먼저 `--dry-run`으로 명령을 확인하고, 빼면 실제로 실행한다.

       python3 .claude/scripts/start_worker.py --role implementer --type feat --issue 42 \
           --spec "브리핑 파일을 읽고 그대로 수행한다: /private/tmp/artex-orchestration/briefs/<이름>.md" --dry-run

   `--effort`, `--model`로 기본값을 덮어쓴다. `--base-branch`로 새 worktree의 기준을 바꾼다(기본 `origin/main`). 새 worktree에 `.claude/agents/<역할>.md`가 없으면(main에 병합되지 않았으면) worker-start 전에 멈추고 정리 명령을 출력한다. 스크립트가 하는 일:

       orca worktree create --name feat-issue-42 --json            # 에이전트 없이 worktree만
       # <worktree>/.claude/settings.local.json 에 {"agent": "implementer"} 기록
       git -C <worktree> branch -m feat/issue-42                   # Orca 임시 이름(LRTK-CODER/...)을 규칙 이름으로
       orca orchestration worker-start --spec "..." --worktree path:<worktree> \
           --agent claude --model claude-opus-5-5 --effort medium --json

   `worker-start --agent`는 `claude` 같은 에이전트 종류만 받는다. 역할은 `settings.local.json`의 `"agent"` 키로 정한다(이 파일은 `.gitignore` 처리돼 있다). 모든 옵션은 `python3 .claude/scripts/start_worker.py --help`로 본다.

### 경우별 명령

조사 워커. 조사 이슈에는 `research` 브랜치 type이 없다. `--type`을 생략하면 `docs`가 된다.

    python3 .claude/scripts/start_worker.py --role researcher --issue 50 --spec "..."   # 브랜치 docs/issue-50

리뷰어를 PR의 worktree에 띄운다. `--worktree`는 새로 만들지도 이름을 바꾸지도 않고 역할 파일만 바꾼다. 역할 파일은 worktree마다 하나라서, 같은 worktree에 활성 워커가 있으면 멈춘다. 덮어쓰려면 `--replace-role`을 붙인다.

    python3 .claude/scripts/start_worker.py --role reviewer --worktree feat/issue-42 --spec "..."

리뷰어가 끝나면 역할을 구현자로 되돌린다.

    python3 .claude/scripts/start_worker.py --role implementer --worktree feat/issue-42 --spec "..."

테스트가 실패해 같은 worktree에서 effort high로 다시 돌린다.

    python3 .claude/scripts/start_worker.py --role implementer --worktree feat/issue-42 --effort high --spec "..."

리뷰 지적을 같은 구현자에게 돌려보낸다. `worker_done`은 dispatch당 한 번이고 워커는 PR 코멘트를 쓰지 않으므로, 끝난 워커의 터미널에 후속 작업을 보낸다. 역할 파일은 그대로다. `--effort`·`--model`은 함께 쓸 수 없다(바꾸려면 `--worktree`로 새로 띄운다).

    python3 .claude/scripts/start_worker.py --follow-up <dispatch_id> \
        --spec "리뷰 지적을 반영한다: /private/tmp/artex-orchestration/reports/<리뷰 보고서>.md"

## 기다리기

    python3 .claude/scripts/wait_workers.py            # 기본 30분. --timeout-ms, --run으로 바꾼다

- Bash 도구의 백그라운드 실행(`run_in_background`)으로 띄운다. 셸 `&`로 띄우거나 중간에 종료하면 orca에 대기 등록이 남아 다음 대기가 `waiter_exists`로 실패한다.
- 대기 명령(`wait_workers.py`, `orca orchestration check --wait`)은 단독 명령으로, 앞 대기가 끝난 뒤에 하나만 띄운다. 앞에는 `cd <경로> &&`만 붙일 수 있다. hook(`.claude/hooks/wait_guard.py`)이 `run_in_background` 없는 대기, 셸 `&`·`disown`·`nohup`·`setsid`, 다른 명령과 묶은 대기, 같은 Orca 창에서 대기 프로세스가 도는 중의 새 대기를 막는다. 다른 창(다른 저장소의 PM 등)의 대기는 막는 이유가 되지 않는다. Orca 대기는 run마다 하나다. `--status`는 막지 않는다.
- `waiter_exists`가 나면 스크립트가 원인과 대처를 출력한다. 남은 대기 프로세스를 `pgrep -fl 'orchestration check .*--wait'`로 찾는다. 이 목록에는 다른 창의 대기도 나오니 `ps -E -ww -o command= -p <PID>`의 `ORCA_PANE_KEY`가 이 세션의 값과 같은 것만 본다. kill해도 등록은 남으니 앞 대기가 끝나거나 타임아웃될 때까지 기다린다.
- worker_done·escalation·question을 기다려 요약하고, heartbeat는 워커별 한 줄로 줄인다. 종료 코드는 받음 0, 타임아웃 2, orca 오류 1이다.
- heartbeat도 대기 종류로 받는다. orca는 걸린 대기가 받는 종류를 PM 채팅 알림에서 빼므로, 이렇게 해야 heartbeat마다 채팅에 알림이 뜨지 않는다. 대기가 걸려 있지 않은 틈에 온 heartbeat는 여전히 알림으로 뜬다.
- 받은 묶음은 확인 처리(`check --ack`)한다. run에 묶인 `check`는 확인 처리 전까지 같은 묶음을 다시 주므로, 하지 않으면 처리한 `worker_done`이 새 완료처럼 또 나온다.
- 소유자에게 올릴 질문이 올 수 있으면 `--no-ack`로 받거나, 출력의 메시지 id를 적어 둔다. 확인 처리한 묶음은 다시 나오지 않는다.
- 워커 전체 상태는 `python3 .claude/scripts/wait_workers.py --status`로 본다. 한 줄 형식: `<dispatch_id>  <worktree 이름>  워커=  진행=  해제=`.

막혔다고 알린 워커(질문, escalation)를 실패로 다루지 않는다.

질문에는 이렇게 답한다. 소유자 승인이 필요한 질문이면 소유자에게 먼저 묻는다.

    orca orchestration reply --id <msg_id> --body "<답>"

## 리뷰와 병합

1. 구현 완료 보고를 받으면 PR이 무엇을 바꾸는지 본다(`gh pr diff <번호> --name-only`).
   - 코드·테스트·검사 설정(`go.mod`, `go.sum`, `web/package.json`, `web/package-lock.json`, `web/tsconfig.json`, `web/biome.json`, `.github/workflows/`, `.githooks/`, `.claude/`의 스크립트·hook)을 바꾸면 검증 워커를 띄운다.
   - 문서만 바꾸면 PM이 직접 본다.
2. 검증 워커를 띄운다. 브리핑에 PR 번호와 보고서 경로를 적는다. 검증 워커는 구현 워커 3개 상한과 따로 세고, 여러 PR을 병렬로 검증해도 된다.

       python3 .claude/scripts/start_worker.py --verify-pr <번호> \
           --spec "브리핑 파일을 읽고 그대로 수행한다: /private/tmp/artex-orchestration/briefs/verify-pr-<번호>.md" --dry-run

   origin/main에서 새 worktree(브랜치 `chore/verify-pr-<번호>`)를 만들고 리뷰어로 띄운다. 구현 worktree의 역할 파일을 건드리지 않고, 역할 정의도 main 사본을 쓴다. PR이 `reviewer.md`를 고쳤어도 검증 워커는 main의 정의로 뜬다. PR 코드는 `pr_gate.py`가 저장소 밖 임시 디렉터리에 꺼내 채점하므로 worktree에 없어도 된다. 이 브랜치는 커밋·push하지 않는 작업용이다.

   검증 워커는 main 체크아웃의 `.claude/scripts/pr_gate.py <번호>`를 실행해 출력을 원문 그대로 보고서에 붙인다. 스크립트가 하는 일은 파일 첫머리 docstring에 있다. 요약: PR 코드를 main의 테스트 파일(`*_test.go`, `testdata/` 디렉터리, web 테스트 `*.test.*`·`*.spec.*`)과 검사 설정 파일(`go.mod`·`go.sum`과 web의 `package.json`·`package-lock.json`·`tsconfig.json`·`biome.json`. PR에만 있는 것은 어디에 있든 지운다)으로 다시 채점하고([1]), PR 트리 그대로 채점하고([2]), 기존 테스트 파일·검사 설정·테스트 대역 변경을 뽑고([3]), 테스트 함수 수·`t.Skip` 수·`-short`에서 빠지는 테스트 수를 비교한다([4]). 막지 않는다. 종료 코드는 [1] 실패면 1, 실행하지 못했으면 2, 그 밖은 0이다.

   PR이 갱신돼 다시 검증하려면 앞 검증 워커를 해제하고 worktree와 브랜치를 지운 뒤 다시 띄운다. 브랜치가 남아 있으면 `start_worker.py`가 정리 명령을 출력하고 멈춘다.
3. PR과 두 보고서를 리뷰한다(`issues.md`의 리뷰 체크리스트).
   검증 보고서와 구현 중 답한 설계 선택을 PR 코멘트로 올린다. 병합을 판단하면 근거와 남긴 지적의 처리 계획도 올린다. 내용 기준은 `issues.md` "결과 공유 코멘트".

       gh pr comment <번호> --body-file <파일>
4. 소유자에게 병합 승인을 요청한다. 검증을 거친 PR이면 요청에 검증 보고서 요약을 넣는다: 재채점 결과([1]·[2] 통과 여부와 실패한 검사), 표시된 항목([3]·[4])과 PR 본문의 이유.
5. squash merge로 병합하고 main 체크아웃을 갱신한다.

       gh pr merge <번호> --squash
       git pull --ff-only

## 정리

한 worktree에는 구현자·리뷰어·후속 작업의 dispatch가 여럿 있을 수 있다. 모두 해제한 뒤 worktree를 지운다.

    python3 .claude/scripts/wait_workers.py --status | grep <worktree 이름>   # dispatch id를 모은다
    orca orchestration worker-release --dispatch <dispatch_id> --json          # dispatch마다
    orca worktree rm --worktree path:<worktree>

검증 worktree도 같은 방법으로 해제·삭제한다. `orca worktree rm`이 로컬 브랜치도 지운다. 브랜치가 남아 있으면(`git branch --list chore/verify-pr-<번호>`) 그때만 지운다.

    git branch -D chore/verify-pr-<번호>

병합된 브랜치는 다시 쓰지 않는다. 후속 작업은 새 이슈·새 브랜치로 한다.
