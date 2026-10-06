"""브랜치 규칙 검사. 규칙 문서: .claude/rules/branching.md

막는 것:
- main 위에서 git commit / git push / git merge
- main을 향한 push, 모든 브랜치 push(--all, --branches)
- force push 전부(-f, --force, --force-with-lease, --force-if-includes, +refspec, --mirror)
- commit hook 건너뛰기(--no-verify, -n, -c core.hooksPath=...)
- 규칙에 맞지 않는 이름으로 브랜치 만들기·이름 바꾸기·push
"""
from __future__ import annotations

import os
import re
from typing import List, Optional, Tuple

import hooklib
from hooklib import Blocked

HOOK_DIR = os.path.dirname(os.path.abspath(__file__))
TYPES_FILE = os.path.join(HOOK_DIR, "branch-types.txt")
RULES_DOC = ".claude/rules/branching.md"

EXEMPT_NAMES = ("main",)
EXEMPT_PREFIXES = ("dependabot/", "backup/")
ISSUE_RE = re.compile(r"issue-[1-9][0-9]*")
SLUG_RE = re.compile(r"[a-z0-9]+(?:-[a-z0-9]+){1,4}")
NAME_FORMAT = "<type>/issue-<번호> 또는 <type>/<slug>"


# ---------------------------------------------------------------- 브랜치 이름

def load_types(path: str = TYPES_FILE) -> List[str]:
    with open(path, encoding="utf-8") as f:
        lines = [line.strip() for line in f]
    return [t for t in lines if t and not t.startswith("#")]


def branch_name_problem(name: str, types: Optional[List[str]] = None) -> Optional[str]:
    """이름이 규칙에 맞으면 None, 아니면 이유를 돌려준다."""
    if name in EXEMPT_NAMES or name.startswith(EXEMPT_PREFIXES):
        return None
    if types is None:
        types = load_types()
    btype, sep, rest = name.partition("/")
    if not sep or btype not in types:
        return "형식은 {}이고 type은 {} 중 하나다".format(NAME_FORMAT, " ".join(types))
    if ISSUE_RE.fullmatch(rest):
        return None
    if rest.startswith("issue-"):
        return "issue- 뒤에는 이슈 번호만 온다(예: {}/issue-42)".format(btype)
    if not SLUG_RE.fullmatch(rest):
        return "slug는 영문 소문자 kebab-case 2~5단어다(예: {}/parser-skeleton)".format(btype)
    return None


# ---------------------------------------------------------------- 차단 메시지

WHY_MAIN = "main은 PR의 squash merge로만 바뀐다. main에 직접 commit·push·merge하지 않는다"
INSTEAD_BRANCH = "작업 브랜치({})에서 커밋하고 `git push -u origin HEAD` 후 PR을 연다".format(NAME_FORMAT)
WHY_FORCE = "force push는 모든 브랜치에서 금지다(--force-with-lease 포함)"
INSTEAD_FORCE = "main을 따라잡을 때는 `git merge origin/main` 후 일반 push를 한다. rebase하지 않는다"
INSTEAD_RENAME = "`git branch -m <새 이름>`으로 규칙에 맞게 고친다(예: feat/issue-42, fix/login-timeout)"


def _blocked(what: str, why: str, instead: str) -> Blocked:
    return Blocked(what, why, instead, RULES_DOC)


def _name_blocked(name: str, problem: str, action: str) -> Blocked:
    return _blocked("규칙에 맞지 않는 브랜치 이름 '{}' ({})".format(name, action), problem, INSTEAD_RENAME)


def _strip_heads(ref: str) -> str:
    return ref[len("refs/heads/"):] if ref.startswith("refs/heads/") else ref


# ---------------------------------------------------------------- 진입

def check(event: dict) -> None:
    if event.get("tool_name") != "Bash":
        return
    command = str((event.get("tool_input") or {}).get("command") or "")
    _check_command(command, event.get("cwd") or os.getcwd(), 0)


def _check_command(command: str, cwd: str, depth: int) -> None:
    for cmd in hooklib.walk(command, cwd, depth):
        if cmd.prog == "git":
            _check_git(cmd)


def _check_git(cmd: hooklib.Command) -> None:
    call = hooklib.parse_git(cmd)
    if call is None:
        return
    if call.shell_alias is not None:
        _check_command(call.shell_alias, call.repo.cwd, cmd.depth + 1)
        return
    sub, args = call.sub, call.args
    if sub == "commit":
        _check_commit(args, call.overrides, call.repo)
    elif sub == "push":
        _check_push(args, call.overrides, call.repo)
    elif sub == "merge":
        _check_merge(args, call.repo)
    elif sub == "branch":
        _check_branch(args)
    elif sub == "checkout":
        _check_new_branch_option(args, "bB", ("--orphan",), "git checkout")
    elif sub == "switch":
        _check_new_branch_option(args, "cC", ("--create", "--force-create", "--orphan"), "git switch")
    elif sub == "worktree" and args[:1] == ["add"]:
        _check_new_branch_option(args[1:], "bB", (), "git worktree add")


# ---------------------------------------------------------------- commit·merge

def _hooks_override(overrides: List[str]) -> Optional[str]:
    for o in overrides:
        if o.split("=", 1)[0].strip().lower() == "core.hookspath":
            return o
    return None


def _block_hooks_override(o: str, action: str) -> Blocked:
    return _blocked(
        "git -c {} {}".format(o, action),
        "hook 경로를 바꾸면 .githooks 검사가 빠진다",
        "`-c core.hooksPath` 없이 실행한다. hook이 막으면 그 이유부터 해결한다")


_COMMIT_VALUE_LONG = ("--message", "--file", "--reuse-message", "--reedit-message", "--fixup",
                      "--squash", "--author", "--date", "--template", "--cleanup", "--trailer",
                      "--pathspec-from-file")


def _commit_skips_hooks(args: List[str]) -> Optional[str]:
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--":
            break
        if a.startswith("--"):
            opt, eq, _ = a.partition("=")
            if len(opt) >= len("--no-veri") and "--no-verify".startswith(opt):
                return a
            if opt in _COMMIT_VALUE_LONG and not eq:
                i += 1
        elif a.startswith("-") and len(a) > 1:
            for k, ch in enumerate(a[1:], 1):
                if ch == "n":
                    return a
                if ch in "mFcCt":
                    if k == len(a) - 1:
                        i += 1  # 값은 다음 인자
                    break
                if ch in "Su":
                    break  # 붙여 쓴 선택 값
        i += 1
    return None


def _check_commit(args: List[str], overrides: List[str], repo: hooklib.Repo) -> None:
    if repo.branch() == "main":
        raise _blocked("main 브랜치에서 git commit", WHY_MAIN, INSTEAD_BRANCH)
    o = _hooks_override(overrides)
    if o:
        raise _block_hooks_override(o, "commit")
    flag = _commit_skips_hooks(args)
    if flag:
        raise _blocked(
            "git commit {}".format(flag),
            "커밋 hook(.githooks)을 건너뛰면 규칙 검사가 빠진다",
            "옵션을 빼고 커밋한다. hook이 막으면 그 이유부터 해결한다")


def _check_merge(args: List[str], repo: hooklib.Repo) -> None:
    if repo.branch() == "main" and not {"--abort", "--quit"} & set(args):
        raise _blocked(
            "main 브랜치에서 git merge", WHY_MAIN,
            "main 갱신은 `git pull --ff-only`, 작업 브랜치 최신화는 작업 브랜치에서 `git merge origin/main`")


# ---------------------------------------------------------------- push

def _default_push_dst(repo: hooklib.Repo, branch: str) -> str:
    """refspec 없는 `git push`가 원격에서 갱신할 브랜치."""
    mode = repo.git(["config", "--get", "push.default"]) or "simple"
    if mode in ("upstream", "tracking"):
        merge = repo.git(["config", "--get", "branch.{}.merge".format(branch)])
        if merge:
            return _strip_heads(merge)
    return branch


def _check_push(args: List[str], overrides: List[str], repo: hooklib.Repo) -> None:
    o = _hooks_override(overrides)
    if o:
        raise _block_hooks_override(o, "push")
    pos: List[str] = []
    force: Optional[str] = None
    everything: Optional[str] = None
    delete = tags = False
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--":
            pos.extend(args[i + 1:])
            break
        if a.startswith("--"):
            opt, eq, _ = a.partition("=")
            name = opt[2:]
            if name.startswith("for") or (name and "mirror".startswith(name)):
                force = force or a
            elif name in ("all", "branches"):
                everything = a
            elif len(name) >= 3 and "delete".startswith(name):
                delete = True
            elif name == "tags":
                tags = True
            if name in ("repo", "receive-pack", "exec", "push-option") and not eq:
                i += 1
        elif a.startswith("-") and len(a) > 1:
            for k, ch in enumerate(a[1:], 1):
                if ch == "f":
                    force = force or a
                elif ch == "d":
                    delete = True
                elif ch == "o":
                    if k == len(a) - 1:
                        i += 1
                    break
        else:
            pos.append(a)
        i += 1

    refspecs = pos[1:]
    plus = next((r for r in refspecs if r.startswith("+")), None)
    if force or plus:
        raise _blocked("force push ({})".format(force or plus), WHY_FORCE, INSTEAD_FORCE)
    if everything:
        raise _blocked("모든 브랜치 push ({})".format(everything),
                       "main까지 함께 push된다. " + WHY_MAIN,
                       "`git push -u origin HEAD`처럼 작업 브랜치 하나만 push한다")

    branch = repo.branch()
    main_push = _blocked("main 브랜치에서 git push", WHY_MAIN, INSTEAD_BRANCH)

    if not refspecs:
        if tags:
            return
        if branch == "main":
            raise main_push
        if branch:
            if _default_push_dst(repo, branch) == "main":
                raise _blocked("upstream이 main인 브랜치의 git push", WHY_MAIN,
                               "`git push -u origin HEAD`로 같은 이름의 원격 브랜치에 push한다")
            problem = branch_name_problem(branch)
            if problem:
                raise _name_blocked(branch, problem, "git push")
        return

    def norm(ref: str) -> str:
        return (branch or ref) if ref in ("HEAD", "@") else _strip_heads(ref)

    for r in refspecs:
        src, sep, dst = r.partition(":")
        if not sep:
            dst = src
        if delete:
            src = ""
        if norm(dst) == "main":
            raise _blocked("main으로 가는 git push ('{}')".format(r), WHY_MAIN, INSTEAD_BRANCH)
        if not src:
            continue  # 원격 브랜치 삭제
        if src.startswith("refs/tags/") or dst.startswith("refs/tags/") or (
                "/" not in src and repo.has_ref("refs/tags/" + src)
                and not repo.has_ref("refs/heads/" + src)):
            continue  # 태그
        if branch == "main":
            raise main_push
        names = [norm(dst)]
        if norm(src) != names[0] and (src in ("HEAD", "@") or repo.has_ref("refs/heads/" + norm(src))):
            names.append(norm(src))
        for name in names:
            if name.startswith("refs/") or name in ("HEAD", "@"):
                continue
            problem = branch_name_problem(name)
            if problem:
                raise _name_blocked(name, problem, "git push")


# ---------------------------------------------------------------- 브랜치 만들기·이름 바꾸기

_BRANCH_LIST_LONG = ("--delete", "--list", "--all", "--remotes", "--verbose", "--contains",
                     "--no-contains", "--merged", "--no-merged", "--points-at", "--show-current",
                     "--set-upstream-to", "--unset-upstream", "--edit-description", "--format",
                     "--sort", "--column", "--no-column", "--ignore-case", "--omit-empty")
_BRANCH_VALUE_LONG = ("--set-upstream-to", "--contains", "--no-contains", "--merged",
                      "--no-merged", "--points-at", "--sort", "--format")


def _check_branch(args: List[str]) -> None:
    move = listing = False
    pos: List[str] = []
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--":
            pos.extend(args[i + 1:])
            break
        if a.startswith("--"):
            opt, eq, _ = a.partition("=")
            if opt in ("--move", "--copy"):
                move = True
            elif opt in _BRANCH_LIST_LONG:
                listing = True
            if opt in _BRANCH_VALUE_LONG and not eq:
                i += 1
        elif a.startswith("-") and len(a) > 1:
            for k, ch in enumerate(a[1:], 1):
                if ch in "mMcC":
                    move = True
                elif ch in "dDlarvi":
                    listing = True
                elif ch == "u":
                    listing = True
                    if k == len(a) - 1:
                        i += 1
                    break
        else:
            pos.append(a)
        i += 1
    if move and pos:
        _check_new_name(pos[-1], "git branch -m")
    elif not move and not listing and pos:
        _check_new_name(pos[0], "git branch")


def _check_new_branch_option(args: List[str], letters: str, long_opts: Tuple[str, ...],
                             action: str) -> None:
    for i, a in enumerate(args):
        nxt = args[i + 1] if i + 1 < len(args) else None
        if a == "--":
            break
        if a.startswith("--"):
            opt, eq, val = a.partition("=")
            if opt in long_opts:
                _check_new_name(val if eq else nxt, action)
        elif a.startswith("-") and len(a) > 1:
            for k, ch in enumerate(a[1:], 1):
                if ch in letters:
                    _check_new_name(a[k + 1:] or nxt, action)
                    break


def _check_new_name(name: Optional[str], action: str) -> None:
    if not name:
        return
    name = _strip_heads(name)
    problem = branch_name_problem(name)
    if problem:
        raise _name_blocked(name, problem, action)
