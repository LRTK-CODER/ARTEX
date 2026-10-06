#!/usr/bin/env python3
"""PM 보조: 역할 정의를 적용한 감독 워커를 하나 시작한다. 절차: .claude/skills/pm/SKILL.md

main 체크아웃에서 실행한다. 세 가지 방식이 있다.

새 worktree(기본):

    python3 .claude/scripts/start_worker.py --role implementer --type feat --issue 42 \\
        --spec "브리핑 파일을 읽고 그대로 수행한다: /private/tmp/artex-orchestration/briefs/x.md" --dry-run

1. 역할·브랜치 이름을 검사한다(이름 규칙은 .claude/hooks/branching_guard.py).
   조사자는 --type을 생략할 수 있다. 이때 type은 docs다. 조사 결과물이 문서이고,
   branch-types.txt에 research type이 없기 때문이다.
2. `git fetch origin main` 뒤 `orca worktree create --name <type>-<issue-N|slug> --base-branch origin/main`
   으로 에이전트 없이 worktree만 만든다.
3. 새 worktree에 `.claude/agents/<역할>.md`가 있는지 확인한다. 없으면 정리 방법을 출력하고 중단한다.
   새 worktree의 `.claude/settings.local.json`에 `"agent": "<역할>"`을 적는다.
   `claude`가 이 설정으로 `.claude/agents/<역할>.md`를 메인 세션 에이전트로 쓴다.
4. `git -C <worktree> branch -m <규칙 이름>`으로 Orca가 붙인 임시 이름을 바꾼다.
5. `orca orchestration worker-start --worktree path:<worktree> --agent claude --model --effort`.

기존 worktree(--worktree <경로 또는 브랜치>): 리뷰어를 PR 브랜치에 띄우거나, 실패한 구현을
effort high로 같은 worktree에서 다시 돌릴 때 쓴다. worktree를 만들지 않고 브랜치 이름도 바꾸지
않는다. settings.local.json의 역할만 바꿔 쓰고 새 터미널에 워커를 띄운다.

역할 파일은 worktree마다 하나다. 기록된 역할이 요청과 다르고 같은 worktree에 활성 워커가 있으면
멈춘다. --replace-role을 주면 덮어쓰고 경고한다. 활성 워커가 없으면 경고하고 덮어쓴다.
위험: 덮어쓴 뒤 원래 워커의 터미널을 재시작하면 그 워커는 바뀐 역할로 뜬다.
리뷰어 작업이 끝나면 `--worktree <같은 곳> --role implementer`로 역할을 되돌린다.

검증 워커(--verify-pr <PR 번호>): 병합 전 재채점을 맡는 리뷰어를 띄운다. origin/main에서
새 worktree를 만들고 브랜치 이름은 chore/verify-pr-<번호>, 역할은 reviewer로 고정한다.
구현 worktree의 역할 파일을 건드리지 않고, 역할 정의도 PR이 고쳤을 수 있는 사본이 아니라
main 사본을 쓴다. PR 코드는 pr_gate.py가 저장소 밖 임시 디렉터리에 꺼내 채점한다.
이 브랜치는 커밋·push하지 않는 작업용이다. 정리할 때 worktree와 함께 로컬 브랜치를 지운다.
같은 PR을 다시 검증할 때 브랜치가 남아 있으면 정리 명령을 출력하고 멈춘다.

후속 작업(--follow-up <dispatch id>): 끝난 워커의 터미널에 새 작업을 보낸다. 리뷰 지적을 같은
구현자에게 돌려보낼 때 쓴다. worker-show로 터미널 핸들과 worktree를 찾아
`worker-start --terminal <핸들> --worktree path:<worktree>`를 실행한다. 역할 파일은 건드리지 않는다.
Orca가 --terminal과 --model·--effort를 함께 받지 않으므로 둘 다 쓸 수 없다.

에이전트 frontmatter의 effort는 메인 세션에 적용되지 않는다. 그래서 역할별 기본 effort를
여기서 --effort로 넘긴다.
"""
from __future__ import annotations

import argparse
import json
import os
import shlex
import subprocess
import sys
from typing import Dict, List, Optional, Set, Tuple

sys.dont_write_bytecode = True

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(REPO_ROOT, ".claude", "hooks"))
import branching_guard  # noqa: E402

MODEL = "claude-opus-5-5"
ROLES = {"implementer": "medium", "reviewer": "medium", "researcher": "low"}  # 역할: 기본 effort
EFFORTS = ("low", "medium", "high", "xhigh", "max")
RESEARCH_TYPE = "docs"  # 조사자가 --type을 생략할 때 쓰는 브랜치 type
VERIFY_ROLE = "reviewer"
VERIFY_BRANCH = "chore/verify-pr-{}"  # 커밋·push하지 않는 검증 워커용 브랜치
BASE_BRANCH = "origin/main"
WORKTREE_PLACEHOLDER = "<새 worktree 경로>"

HELP_EPILOG = """\
방식:
  새 worktree      --type·--issue|--slug로 origin/main에서 worktree를 만든다(기본).
  --worktree       기존 worktree에 새 터미널로 워커를 띄운다. 만들지도 이름을 바꾸지도 않는다.
  --follow-up      끝난 워커의 터미널에 후속 작업을 보낸다. 역할 파일은 그대로다.
  --verify-pr      PR 검증 워커(리뷰어)를 origin/main의 새 worktree에 띄운다.
                   브랜치 chore/verify-pr-<번호>는 커밋·push하지 않는 작업용이다.
                   정리할 때 worktree와 로컬 브랜치를 지운다. 다시 검증하려면 먼저 정리한다.

조사자는 --type을 생략할 수 있다. 이때 브랜치 type은 docs다(research type은 없다).

--worktree의 역할 파일: worktree마다 하나다. 기록된 역할이 요청과 다르고 같은 worktree에
활성 워커가 있으면 멈춘다. --replace-role로 덮어쓴다(경고). 활성 워커가 없으면 경고하고 덮어쓴다.
덮어쓴 뒤 원래 워커 터미널을 재시작하면 바뀐 역할로 뜬다.
리뷰어가 끝나면 --worktree <같은 곳> --role implementer로 역할을 되돌린다.

예:
  --role implementer --type feat --issue 42 --spec "..."
  --role researcher --issue 50 --spec "..."                       # 브랜치 docs/issue-50
  --role reviewer --worktree feat/issue-42 --spec "..."           # PR 브랜치에 리뷰어
  --role implementer --worktree feat/issue-42 --effort high --spec "..."   # high로 다시
  --follow-up ctx_123 --spec "리뷰 지적을 반영한다: ..."
  --verify-pr 42 --spec "..."                                     # 브랜치 chore/verify-pr-42
"""


class UsageError(Exception):
    pass


class MissingRoleDefinition(RuntimeError):
    pass


class RoleConflict(RuntimeError):
    pass


def branch_name(btype: str, issue: Optional[int], slug: Optional[str]) -> str:
    if slug is not None and slug.startswith("issue-"):
        raise UsageError("slug는 issue-로 시작하지 않는다. 이슈가 있으면 --issue를 쓴다")
    name = "{}/issue-{}".format(btype, issue) if issue is not None else "{}/{}".format(btype, slug)
    problem = branching_guard.branch_name_problem(name)
    if problem:
        raise UsageError("브랜치 이름 '{}': {}".format(name, problem))
    return name


def orca_name(branch: str) -> str:
    """Orca worktree 이름. Orca는 이 이름에 `LRTK-CODER/`를 붙여 임시 브랜치를 만든다."""
    return branch.replace("/", "-")


def settings_path(worktree: str) -> str:
    return os.path.join(worktree, ".claude", "settings.local.json")


def read_agent_setting(worktree: str) -> Optional[str]:
    path = settings_path(worktree)
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as f:
        return json.load(f).get("agent")


def write_agent_setting(worktree: str, role: str) -> str:
    """worktree의 .claude/settings.local.json에 agent를 적는다. 다른 키는 그대로 둔다."""
    if role not in ROLES:
        raise UsageError("역할은 {} 중 하나다".format(", ".join(ROLES)))
    path = settings_path(worktree)
    data = {}
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
    data["agent"] = role
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
        f.write("\n")
    return path


def parse_args(argv: List[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description="역할 정의를 적용한 감독 워커를 시작한다",
                                epilog=HELP_EPILOG,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--role", choices=sorted(ROLES), help="워커 역할. --follow-up에서는 쓰지 않는다")
    p.add_argument("--type", help="브랜치 type(.claude/hooks/branch-types.txt)."
                   " 조사자는 생략하면 {}".format(RESEARCH_TYPE))
    name = p.add_mutually_exclusive_group()
    name.add_argument("--issue", type=int, help="이슈 번호")
    name.add_argument("--slug", help="이슈가 없을 때 영문 소문자 kebab-case 2~5단어")
    target = p.add_mutually_exclusive_group()
    target.add_argument("--worktree", help="기존 worktree의 경로 또는 브랜치 이름."
                        " 새로 만들지 않고 역할 파일만 바꿔 쓴다")
    target.add_argument("--follow-up", metavar="DISPATCH_ID",
                        help="끝난 워커의 dispatch id. 그 터미널에 후속 작업을 보낸다")
    target.add_argument("--verify-pr", type=int, metavar="PR",
                        help="PR 검증 워커를 origin/main의 새 worktree(chore/verify-pr-<번호>)에"
                        " 리뷰어로 띄운다")
    work = p.add_mutually_exclusive_group(required=True)
    work.add_argument("--spec", help="워커에게 줄 작업 설명(보통 브리핑 파일 경로를 가리킨다)")
    work.add_argument("--task", help="이미 만든 orchestration task id")
    p.add_argument("--effort", choices=EFFORTS, help="역할 기본값을 덮어쓴다. --follow-up에서는 못 쓴다")
    p.add_argument("--model", help="기본 {}. --follow-up에서는 못 쓴다".format(MODEL))
    p.add_argument("--base-branch", help="새 worktree의 기준. 기본 {}".format(BASE_BRANCH))
    p.add_argument("--replace-role", action="store_true",
                   help="--worktree에서 활성 워커가 있어도 기록된 역할을 덮어쓴다")
    p.add_argument("--run", help="orchestration run id(생략하면 현재 run)")
    p.add_argument("--repo", help="orca repo 선택자(생략하면 현재 저장소)")
    p.add_argument("--dry-run", action="store_true", help="실행할 명령만 차례대로 출력한다")
    return p.parse_args(argv)


def mode(args: argparse.Namespace) -> str:
    if args.verify_pr is not None:
        return "verify"
    if args.follow_up:
        return "follow-up"
    if args.worktree:
        return "existing"
    return "new"


def validate(args: argparse.Namespace) -> None:
    """방식별로 함께 쓸 수 없는 옵션을 거부한다."""
    m = mode(args)
    if m == "verify":
        if args.role not in (None, VERIFY_ROLE):
            raise UsageError("--verify-pr의 역할은 {}다".format(VERIFY_ROLE))
        # 검증 worktree는 역할 정의를 main 사본으로 읽어야 하므로 기준을 바꾸지 못하게 한다
        given = [flag for flag, value in (("--type", args.type), ("--issue", args.issue),
                                          ("--slug", args.slug), ("--base-branch", args.base_branch))
                 if value is not None]
        if given or args.replace_role:
            raise UsageError("--verify-pr에는 {}를 쓰지 않는다. 브랜치와 기준은 정해져 있다: {}, {}"
                             .format(", ".join(given + (["--replace-role"] if args.replace_role
                                                        else [])),
                                     VERIFY_BRANCH.format(args.verify_pr), BASE_BRANCH))
        # plan()이 역할로 역할 파일과 기본 effort를 정하므로 여기서 채운다
        args.role = VERIFY_ROLE
        return
    if m == "new":
        if args.role is None:
            raise UsageError("--role이 필요하다")
        if args.issue is None and args.slug is None:
            raise UsageError("새 worktree에는 --issue나 --slug가 필요하다")
        if args.type is None and args.role != "researcher":
            raise UsageError("--type이 필요하다(조사자만 생략할 수 있다)")
        if args.replace_role:
            raise UsageError("--replace-role은 --worktree에서만 쓴다")
        return
    opt = "--follow-up" if m == "follow-up" else "--worktree"
    given = [flag for flag, value in (("--type", args.type), ("--issue", args.issue),
                                      ("--slug", args.slug), ("--base-branch", args.base_branch),
                                      ("--repo", args.repo)) if value is not None]
    if given:
        raise UsageError("{}에는 {}를 쓰지 않는다. 기존 worktree와 브랜치를 그대로 쓴다"
                         .format(opt, ", ".join(given)))
    if m == "existing":
        if args.role is None:
            raise UsageError("--role이 필요하다")
        return
    if args.role is not None:
        raise UsageError("--follow-up은 기존 터미널의 역할을 그대로 쓴다. --role을 뺀다")
    if args.effort is not None or args.model is not None or args.replace_role:
        raise UsageError("--follow-up에는 --effort, --model, --replace-role을 쓰지 않는다."
                         " Orca가 --terminal과 함께 받지 않는다."
                         " effort를 바꾸려면 --worktree로 새 터미널에 띄운다")


def new_branch(args: argparse.Namespace) -> str:
    if args.verify_pr is not None:
        name = VERIFY_BRANCH.format(args.verify_pr)
        problem = branching_guard.branch_name_problem(name)
        if problem:
            raise UsageError("브랜치 이름 '{}': {}".format(name, problem))
        return name
    return branch_name(args.type or RESEARCH_TYPE, args.issue, args.slug)


def start_command(args: argparse.Namespace, worktree: str,
                  terminal: Optional[str] = None) -> List[str]:
    start = ["orca", "orchestration", "worker-start"]
    start += ["--spec", args.spec] if args.spec else ["--task", args.task]
    start += ["--worktree", "path:" + worktree]
    if terminal:
        start += ["--terminal", terminal, "--json"]
    else:
        start += ["--agent", "claude", "--model", args.model or MODEL,
                  "--effort", args.effort or ROLES[args.role], "--json"]
    if args.run:
        start += ["--run", args.run]
    return start


def plan(args: argparse.Namespace, worktree: str = WORKTREE_PLACEHOLDER) -> List[List[str]]:
    """새 worktree 방식의 명령 목록. settings.local.json 기록은 ["write-agent-setting", 경로, 역할]."""
    branch = new_branch(args)
    create = ["orca", "worktree", "create", "--name", orca_name(branch),
              "--base-branch", args.base_branch or BASE_BRANCH, "--json"]
    if args.repo:
        create[3:3] = ["--repo", args.repo]
    return [
        ["git", "-C", REPO_ROOT, "fetch", "origin", "main"],
        create,
        ["write-agent-setting", worktree, args.role],
        ["git", "-C", worktree, "branch", "-m", branch],
        start_command(args, worktree),
    ]


def plan_existing(args: argparse.Namespace, worktree: str) -> List[List[str]]:
    return [["write-agent-setting", worktree, args.role], start_command(args, worktree)]


def plan_follow_up(args: argparse.Namespace, terminal: str, worktree: str) -> List[List[str]]:
    return [start_command(args, worktree, terminal)]


def format_step(step: List[str]) -> str:
    if step[0] == "write-agent-setting":
        return "# {}/.claude/settings.local.json 에 {{\"agent\": \"{}\"}} 기록".format(step[1], step[2])
    return " ".join(shlex.quote(a) for a in step)


def orca_json(cmd: List[str]) -> Dict:
    out = subprocess.run(cmd, check=True, capture_output=True, text=True).stdout
    return json.loads(out)["result"]


def find_worktree(name: str) -> str:
    """orca worktree list에서 방금 만든 worktree 경로를 찾는다."""
    for w in orca_json(["orca", "worktree", "list", "--json"])["worktrees"]:
        if w.get("displayName") == name or os.path.basename(w.get("path", "")) == name:
            return w["path"]
    raise RuntimeError("만든 worktree '{}'를 orca worktree list에서 찾지 못했다".format(name))


def git_worktrees() -> List[Tuple[str, Optional[str]]]:
    """이 저장소의 worktree (경로, 브랜치) 목록. 첫 항목이 main 체크아웃이다."""
    out = subprocess.run(["git", "-C", REPO_ROOT, "worktree", "list", "--porcelain"],
                         check=True, capture_output=True, text=True).stdout
    result: List[Tuple[str, Optional[str]]] = []
    for block in out.strip().split("\n\n"):
        fields = dict(line.split(" ", 1) for line in block.splitlines() if " " in line)
        if "worktree" in fields:
            branch = fields.get("branch", "")
            result.append((fields["worktree"],
                           branch[len("refs/heads/"):] if branch.startswith("refs/heads/") else None))
    return result


def resolve_worktree(selector: str, worktrees: List[Tuple[str, Optional[str]]]) -> str:
    """경로 또는 브랜치 이름으로 이 저장소의 연결 worktree 경로를 찾는다. main 체크아웃은 거부한다."""
    real = os.path.realpath(selector) if os.path.isdir(selector) else None
    for i, (path, branch) in enumerate(worktrees):
        if (real is not None and os.path.realpath(path) == real) or branch == selector:
            if i == 0:
                raise UsageError("main 체크아웃에는 워커를 띄우지 않는다")
            return path
    raise UsageError("'{}'에 해당하는 worktree가 없다(git worktree list)".format(selector))


def active_worker_paths(run: Optional[str]) -> Set[str]:
    """worker-list에서 터미널이 살아 있는 워커의 worktree 경로."""
    cmd = ["orca", "orchestration", "worker-list", "--terminal-state", "active", "--json"]
    if run:
        cmd += ["--run", run]
    paths = set()
    for w in orca_json(cmd)["workers"]:
        if w.get("terminalState") != "active":
            continue
        wid = (w.get("resource") or {}).get("worktreeId") or ""
        if "::" in wid:
            paths.add(os.path.realpath(wid.split("::", 1)[1]))
    return paths


def check_role_change(worktree: str, role: str, active: Set[str], replace: bool) -> Optional[str]:
    """역할 파일을 바꿔 써도 되는지 본다. 경고 문장을 돌려주거나 RoleConflict를 던진다."""
    recorded = read_agent_setting(worktree)
    if recorded is None or recorded == role:
        return None
    risk = "{} 워커의 터미널을 재시작하면 {} 역할로 뜬다".format(recorded, role)
    if os.path.realpath(worktree) in active and not replace:
        raise RoleConflict("\n".join([
            "{}에 {} 역할이 기록돼 있고 활성 워커가 있다. 역할을 {}로 바꾼 뒤 {}.".format(
                worktree, recorded, role, risk),
            "워커는 시작하지 않았다. 그래도 띄우려면 --replace-role을 준다.",
            "끝난 뒤 되돌리기: --worktree {} --role {}".format(shlex.quote(worktree), recorded),
        ]))
    return "경고: {}의 역할을 {}에서 {}로 바꾼다. {}. 되돌리기: --worktree {} --role {}".format(
        worktree, recorded, role, risk, shlex.quote(worktree), recorded)


def dispatch_target(dispatch_id: str) -> Tuple[str, str]:
    """worker-show로 dispatch의 (터미널 핸들, worktree 경로)를 찾는다."""
    worker = orca_json(["orca", "orchestration", "worker-show", "--dispatch", dispatch_id,
                        "--json"]).get("worker") or {}
    handle = worker.get("agentTerminalHandle")
    wid = worker.get("worktreeId") or ""
    if not handle or "::" not in wid:
        raise RuntimeError("dispatch '{}'의 터미널 핸들이나 worktree를 찾지 못했다".format(dispatch_id))
    return handle, wid.split("::", 1)[1]


def role_definition(worktree: str, role: str) -> str:
    return os.path.join(worktree, ".claude", "agents", role + ".md")


def check_role_definition(worktree: str, role: str, created: bool = True) -> None:
    """worktree에 역할 정의가 없으면 MissingRoleDefinition을 던진다.

    정의 파일이 없으면 claude는 "agent" 설정을 무시하고 역할 없이 뜬다.
    created가 참이면(방금 만든 worktree) 정리 방법을 함께 적는다.
    """
    path = role_definition(worktree, role)
    if os.path.isfile(path):
        return
    lines = ["worktree에 역할 정의가 없다: {}".format(path)]
    if not created:
        lines.append("이 브랜치에 .claude/agents/{}.md가 있어야 한다. main을 merge해 따라잡는다."
                     " 워커는 시작하지 않았다.".format(role))
        raise MissingRoleDefinition("\n".join(lines))
    branch = subprocess.run(["git", "-C", worktree, "symbolic-ref", "--short", "-q", "HEAD"],
                            capture_output=True, text=True).stdout.strip() or "<worktree 브랜치>"
    lines[0] = "새 " + lines[0]
    raise MissingRoleDefinition("\n".join(lines + [
        "worktree는 main에서 만들어지므로 .claude/agents/{}.md가 main에 병합돼 있어야 한다."
        " 워커는 시작하지 않았다.".format(role),
        "만든 worktree 정리:",
        "  orca worktree rm --worktree {} --force --json".format(shlex.quote("path:" + worktree)),
        "  git branch -D {}".format(shlex.quote(branch)),
    ]))


def execute(steps: List[List[str]]) -> None:
    for step in steps:
        print("$ " + format_step(step), file=sys.stderr)
        if step[0] == "write-agent-setting":
            write_agent_setting(step[1], step[2])
        else:
            subprocess.run(step, check=True)


def emit(steps: List[List[str]]) -> None:
    for step in steps:
        print(format_step(step))


def run_new(args: argparse.Namespace) -> int:
    branch = new_branch(args)
    if subprocess.run(["git", "-C", REPO_ROOT, "show-ref", "--verify", "--quiet",
                       "refs/heads/" + branch]).returncode == 0:
        if args.verify_pr is not None:
            raise UsageError(verify_branch_exists(branch))
        raise UsageError("브랜치 '{}'가 이미 있다. 이슈 하나에 브랜치 하나다."
                         " 기존 worktree에 띄우려면 --worktree {}".format(branch, branch))
    if args.dry_run:
        if subprocess.run(["git", "-C", REPO_ROOT, "cat-file", "-e",
                           "main:.claude/agents/{}.md".format(args.role)],
                          capture_output=True).returncode != 0:
            print("경고: main에 .claude/agents/{}.md가 없다. 실제 실행은 worker-start 전에 중단된다"
                  .format(args.role), file=sys.stderr)
        emit(plan(args))
        return 0
    fetch, create = plan(args)[:2]
    for step in (fetch, create):
        print("$ " + format_step(step), file=sys.stderr)
        subprocess.run(step, check=True, stdout=subprocess.DEVNULL)
    worktree = find_worktree(orca_name(branch))
    check_role_definition(worktree, args.role)
    execute(plan(args, worktree)[2:])
    return 0


def verify_branch_exists(branch: str) -> str:
    """검증 브랜치가 이미 있을 때 정리 방법을 적은 문장."""
    path = next((p for p, b in git_worktrees() if b == branch), None)
    lines = ["검증 브랜치 '{}'가 이미 있다. PR이 갱신돼 다시 검증하려면 앞 검증을 정리하고"
             " 다시 실행한다.".format(branch),
             "  python3 .claude/scripts/wait_workers.py --status | grep {}   # dispatch id를 모은다"
             .format(orca_name(branch)),
             "  orca orchestration worker-release --dispatch <dispatch_id> --json"]
    if path:
        lines.append("  orca worktree rm --worktree {}".format(shlex.quote("path:" + path)))
    lines.append("  git branch -D {}".format(shlex.quote(branch)))
    return "\n".join(lines)


def run_existing(args: argparse.Namespace) -> int:
    worktree = resolve_worktree(args.worktree, git_worktrees())
    check_role_definition(worktree, args.role, created=False)
    warning = check_role_change(worktree, args.role, active_worker_paths(args.run),
                                args.replace_role)
    if warning:
        print(warning, file=sys.stderr)
    steps = plan_existing(args, worktree)
    if args.dry_run:
        emit(steps)
    else:
        execute(steps)
    return 0


def run_follow_up(args: argparse.Namespace) -> int:
    terminal, worktree = dispatch_target(args.follow_up)
    steps = plan_follow_up(args, terminal, worktree)
    if args.dry_run:
        emit(steps)
    else:
        execute(steps)
    return 0


def run(args: argparse.Namespace) -> int:
    validate(args)
    runners = {"new": run_new, "verify": run_new, "existing": run_existing,
               "follow-up": run_follow_up}
    return runners[mode(args)](args)


def main(argv: List[str]) -> int:
    args = parse_args(argv)
    try:
        return run(args)
    except UsageError as e:
        print("오류: {}".format(e), file=sys.stderr)
        return 2
    except (MissingRoleDefinition, RoleConflict) as e:
        print("중단: {}".format(e), file=sys.stderr)
        return 1
    except (subprocess.CalledProcessError, RuntimeError) as e:
        print("실패: {}. 새 worktree를 이미 만들었으면 정리한다(보고서의 정리 절차)".format(e),
              file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
