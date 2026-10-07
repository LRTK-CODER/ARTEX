# 브랜치 규칙

GitHub flow를 쓴다. `main`이 유일한 장기 브랜치다. 모든 변경은 main에서 딴 짧은 브랜치에서 만들고, PR을 거쳐 squash merge로 main에 들어간다.

**main에 직접 commit하거나 push하지 않는다.**

## 브랜치 만들기

- 최신 main에서 딴다.
- 이슈 하나에 브랜치 하나. 한 이슈에 동시에 열린 브랜치는 하나뿐이다.
- 처음부터 아래 규칙에 맞는 이름으로 만든다.

## 이름

| 경우 | 형식 | 예 |
|---|---|---|
| 이슈 있음 | `<type>/issue-<번호>` | `feat/issue-42` |
| 이슈 없음 | `<type>/<slug>` | `feat/parser-skeleton` |

- type 목록은 `.claude/hooks/branch-types.txt`에만 있다(한 줄에 하나). hook도 이 파일을 읽는다. type을 늘릴 때는 이 파일만 고친다.
- slug는 영문 소문자 kebab-case 2~5단어다. 숫자도 쓸 수 있다. `issue-`로 시작하지 않는다.
- 이슈 번호는 0으로 시작하지 않는다.
- 이름 검사 예외: `main`, `dependabot/*`, `backup/*`.

## 수명

- 같은 날 병합을 목표로 하고, 길어도 이틀을 넘기지 않는다.
- 커밋 없이 3일이 지난 브랜치는 닫거나 다시 배정한다.
- 병합된 브랜치는 다시 쓰지 않는다. 후속 작업은 새 브랜치에서 한다.

## 커밋

작업 브랜치의 개별 커밋 메시지는 형식이 자유다. squash merge로 PR 제목만 main의 커밋 제목이 되므로 형식은 PR 제목에서 지킨다(`issues.md` "PR").

## main 따라잡기

작업 브랜치를 최신 main에 맞출 때는 main을 브랜치로 merge한다.

    git fetch origin
    git merge origin/main

rebase한 뒤 force push하지 않는다. force push는 모든 브랜치에서 금지다(`--force-with-lease` 포함).

## PR과 병합

- `git push -u origin HEAD`로 올리고 PR을 연다. 제목·본문 형식은 `issues.md`의 "PR"을 따른다.
- 병합은 squash merge만 쓰고, 소유자 승인 후에 한다.

순차 병합이 기본이다. 앞 PR이 main에 들어간 뒤 새 브랜치를 main에서 딴다. stacked PR(다른 작업 브랜치를 base로 하는 PR)은 예외로만 쓴다. 쓸 때 주의점:

- `Closes #N`은 main을 향한 PR에서만 동작한다. 쌓인 PR이 병합돼도 이슈가 닫히지 않는다.
- squash 뒤 아래 브랜치의 커밋이 위 브랜치에 다시 섞인다. main을 merge해서 정리하고, force push는 하지 않는다.

## 릴리스

당분간 없다. 필요해지면 main에 태그를 단다.

## 강제 장치

GitHub 무료 플랜이라 서버 쪽 브랜치 보호(rulesets)와 필수 검사를 쓸 수 없다. 로컬 hook 두 겹으로 막는다.

### hook이 막는 것

Claude Code hook(`.claude/settings.json` → `.claude/hooks/guard.py`)은 Bash 명령을 실행 전에 검사한다.

- main 위에서 `git commit`, `git push`, `git merge` (main 위에서도 태그 push와 원격 브랜치 삭제는 된다)
- main으로 가는 push: `git push origin main`, `HEAD:main`, `:main` 등
- 모든 브랜치 push: `--all`, `--branches`
- force push 전부: `-f`, `--force`, `--force-with-lease`, `--force-if-includes`, `+<refspec>`, `--mirror`
- hook 건너뛰기: `git commit --no-verify`, `git commit -n`, `git -c core.hooksPath=...`(commit·push)
- 규칙에 맞지 않는 이름으로 브랜치 만들기·복사·이름 바꾸기: `git branch`, `git branch -c`·`-C`, `git branch -m`·`-M`, `git checkout -b`·`-B`·`--orphan`, `git switch -c`·`-C`·`--orphan`, `git worktree add -b`·`-B`
- 규칙에 맞지 않는 이름의 브랜치 push. 지금 브랜치 이름이 규칙에 어긋나면 `git branch -m`으로 먼저 고친다.

git hook(`.githooks/`)은 Claude Code 밖의 도구와 사람에게도 적용된다.

- pre-commit: main 위 커밋. 스테이징된 `*.go`의 gofmt 형식 검사와, 스테이징된 web 파일의 biome 검사
- pre-push: main으로 가는 push, fast-forward가 아닌 push, 규칙에 맞지 않는 브랜치 이름

git hook은 새 클론에서 한 번 켠다. worktree끼리 설정을 공유한다.

    git config core.hooksPath "$(git rev-parse --path-format=absolute --git-common-dir)/../.githooks"

main 체크아웃의 절대 경로를 쓰는 이유는 `orchestration.md`의 "hook은 main 체크아웃 사본으로 돈다"에 있다.

### hook이 막지 못하는 것

- git hook은 `git push --no-verify`, `git config core.hooksPath`로 경로 바꾸기, `git --config-env=core.hooksPath=<변수>`로 끌 수 있다. Claude Code hook도 이 셋은 막지 않는다.
- main 위의 `git cherry-pick`, `git revert`, `git pull`, `git reset`은 로컬에서 막지 않는다. main에 커밋이 생겨도 push 단계에서만 막힌다.
- `-b` 없는 `git worktree add <경로>`는 경로 이름으로 브랜치를 만들지만 이름 검사를 하지 않는다.

### 문서로만 지키는 것

- 이슈 하나에 브랜치 하나
- 수명: 이틀 안에 병합, 커밋 없이 3일이면 정리, 병합된 브랜치 재사용 금지
- main은 merge로 따라잡는다. rebase 자체는 막지 않지만, rebase한 브랜치는 force push 없이 올릴 수 없다.
- squash merge만 쓴다. PR 제목·본문 형식
- 병합은 소유자 승인 후, 검증 워커의 재채점(`pr_gate.py`) 결과와 로컬 검사 결과 확인 후

hook은 흔한 명령 형태만 해석한다. 막히지 않았다고 허용된 것은 아니다. 우회하지 말고 이 문서를 따른다.
