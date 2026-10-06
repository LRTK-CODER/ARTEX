"""워커 격리 검사. 규칙 문서: .claude/rules/orchestration.md

워커는 연결 worktree에서 도는 세션이다. 프로젝트 디렉터리(CLAUDE_PROJECT_DIR, 없으면 cwd)의
`git rev-parse --git-dir`와 `--git-common-dir`가 다르면 워커로 본다. main 체크아웃의 PM
세션에는 아무것도 하지 않는다.

워커에게만 막는 것:
- PR 병합: `gh pr merge`, `gh api .../merge`
- git 대상(cd, -C, --git-dir, --work-tree, GIT_DIR=)이 자기 worktree 밖
- Write·Edit·NotebookEdit 경로가 자기 worktree 밖

허용하는 곳: 자기 worktree, 자기 git 디렉터리, 임시 디렉터리(/tmp, /private/tmp,
/var/folders, $TMPDIR), ~/.claude
"""
from __future__ import annotations

import os
from collections import namedtuple
from typing import List, Optional

import hooklib
from hooklib import Blocked

RULES_DOC = ".claude/rules/orchestration.md"
FILE_TOOLS = {"Write": "file_path", "Edit": "file_path", "NotebookEdit": "notebook_path"}
TEMP_DIRS = ("/tmp", "/private/tmp", "/var/folders")

Worker = namedtuple("Worker", "worktree git_dir allowed")


def _real(path: str) -> str:
    """없는 경로도 존재하는 상위 디렉터리까지는 심볼릭 링크를 풀어 절대 경로로 만든다."""
    path = os.path.abspath(path)
    base = hooklib.nearest_dir(path)
    rest = os.path.relpath(path, base)
    real = os.path.realpath(base)
    return real if rest == "." else os.path.normpath(os.path.join(real, rest))


def _inside(path: str, roots: List[str]) -> bool:
    return any(path == r or path.startswith(r.rstrip("/") + "/") for r in roots)


def worker_context(event: dict) -> Optional[Worker]:
    """워커 세션이면 Worker, 아니면(main 체크아웃, 저장소 밖) None."""
    project = os.environ.get("CLAUDE_PROJECT_DIR") or event.get("cwd") or os.getcwd()
    out = hooklib.run_git(["-C", project, "rev-parse", "--path-format=absolute",
                           "--git-dir", "--git-common-dir", "--show-toplevel"])
    if not out:
        return None
    lines = out.splitlines()
    if len(lines) != 3:
        return None
    git_dir, common_dir, toplevel = (_real(p) for p in lines)
    if git_dir == common_dir:
        return None
    allowed = [toplevel, git_dir, os.path.realpath(os.path.expanduser("~/.claude"))]
    allowed += [os.path.realpath(d) for d in TEMP_DIRS]
    if os.environ.get("TMPDIR"):
        allowed.append(_real(os.environ["TMPDIR"]))
    return Worker(toplevel, git_dir, allowed)


def _blocked(what: str, why: str, instead: str) -> Blocked:
    return Blocked(what, why, instead, RULES_DOC)


WHY_OUTSIDE = "워커는 자기 worktree 안에서만 작업한다. 다른 worktree와 main 체크아웃은 다른 세션의 것이다"


# ---------------------------------------------------------------- 진입

def check(event: dict) -> None:
    tool = event.get("tool_name")
    if tool != "Bash" and tool not in FILE_TOOLS:
        return
    worker = worker_context(event)
    if worker is None:
        return
    tool_input = event.get("tool_input") or {}
    cwd = event.get("cwd") or os.getcwd()
    if tool in FILE_TOOLS:
        _check_file(tool, str(tool_input.get(FILE_TOOLS[tool]) or ""), cwd, worker)
    else:
        _check_command(str(tool_input.get("command") or ""), cwd, 0, worker)


def _check_file(tool: str, path: str, cwd: str, worker: Worker) -> None:
    if not path:
        return
    target = _real(hooklib.resolve(path, cwd))
    if not _inside(target, worker.allowed):
        raise _blocked("worktree 밖 파일 수정 ({} {})".format(tool, target), WHY_OUTSIDE,
                       "자기 worktree({}) 안의 파일만 고친다. 밖의 변경이 필요하면 PM에게 ask로 요청한다"
                       .format(worker.worktree))


def _check_command(command: str, cwd: str, depth: int, worker: Worker) -> None:
    for cmd in hooklib.walk(command, cwd, depth):
        if cmd.prog == "gh":
            _check_gh(cmd.argv)
        elif cmd.prog == "git":
            _check_git(cmd, worker)


# ---------------------------------------------------------------- gh

_GH_VALUE_OPTS = ("-R", "--repo", "--hostname")


def _gh_words(argv: List[str]) -> List[str]:
    """gh의 옵션을 뺀 위치 인자."""
    words: List[str] = []
    i = 1
    while i < len(argv):
        a = argv[i]
        if a in _GH_VALUE_OPTS:
            i += 2
            continue
        if not a.startswith("-"):
            words.append(a)
        i += 1
    return words


def _check_gh(argv: List[str]) -> None:
    words = _gh_words(argv)
    merge = words[:2] == ["pr", "merge"] or (
        words[:1] == ["api"] and any(w.rstrip("/").endswith("/merge") for w in words[1:]))
    if merge:
        raise _blocked("워커의 PR 병합 ({})".format(" ".join(argv[:4])),
                       "병합은 소유자 승인 뒤 PM이 한다",
                       "PR을 연 채로 두고 worker_done 보고에 PR 링크를 적는다")


# ---------------------------------------------------------------- git

def _check_git(cmd: hooklib.Command, worker: Worker) -> None:
    call = hooklib.parse_git(cmd)
    if call is None:
        return
    repo = call.repo
    targets = [("작업 디렉터리", repo.cwd)]
    if repo.git_dir:
        targets.append(("--git-dir/GIT_DIR", repo.git_dir))
    if repo.work_tree:
        targets.append(("--work-tree/GIT_WORK_TREE", repo.work_tree))
    for label, path in targets:
        target = _real(path)
        if not _inside(target, worker.allowed):
            raise _blocked("worktree 밖을 대상으로 한 git {} ({}: {})".format(call.sub or "", label, target),
                           WHY_OUTSIDE,
                           "자기 worktree({})에서 git을 실행한다. main 내용은 `git fetch` 후 origin/main으로 본다"
                           .format(worker.worktree))
    if call.shell_alias is not None:
        _check_command(call.shell_alias, repo.cwd, cmd.depth + 1, worker)
