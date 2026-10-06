"""검사 모듈이 함께 쓰는 도구: 차단 예외, 셸 명령 분해, git 호출 해석.

완전한 셸·git 파서가 아니다. 흔한 형태를 막는 것이 목표다.
"""
from __future__ import annotations

import os
import re
import shlex
import subprocess
from collections import namedtuple
from typing import Dict, Iterator, List, Optional, Tuple

MAX_DEPTH = 5
SHELLS = ("sh", "bash", "zsh", "dash", "ksh")


class Blocked(Exception):
    """검사 모듈이 도구 호출을 막을 때 던진다. 메시지는 한 줄씩: 무엇을, 왜, 대신 어떻게."""

    def __init__(self, what: str, why: str, instead: str, doc: Optional[str] = None):
        super().__init__(what)
        self.what, self.why, self.instead, self.doc = what, why, instead, doc

    def message(self) -> str:
        instead = self.instead + (" (규칙: {})".format(self.doc) if self.doc else "")
        return "차단: {}\n이유: {}\n대신: {}\n".format(self.what, self.why, instead)


# ---------------------------------------------------------------- 경로·git

def run_git(args: List[str]) -> Optional[str]:
    """git을 실행해 성공하면 표준 출력을, 실패하면 None을 돌려준다."""
    env = {k: v for k, v in os.environ.items()
           if k not in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR")}
    try:
        p = subprocess.run(["git"] + args, capture_output=True, text=True, timeout=10, env=env)
    except (OSError, subprocess.SubprocessError):
        return None
    return p.stdout.strip() if p.returncode == 0 else None


def resolve(path: str, base: str) -> str:
    path = os.path.expanduser(os.path.expandvars(path))
    return os.path.normpath(os.path.join(base, path))


def nearest_dir(path: str) -> str:
    """경로가 없으면 가장 가까운 존재하는 상위 디렉터리를 돌려준다."""
    while path and not os.path.isdir(path):
        parent = os.path.dirname(path)
        if parent == path:
            break
        path = parent
    return path or "/"


# ---------------------------------------------------------------- 셸 명령 분해

_SEP = ";&|()\n"
_HEREDOC_RE = re.compile(r"<<-?[ \t]*(?:'([^'\n]*)'|\"([^\"\n]*)\"|\\?([^\s;&|()<>]+))")


class _Splitter:
    """셸 명령 문자열을 단순 명령 조각으로 나눈다.

    `;` `&&` `||` `|` `&` 줄바꿈 괄호로 나누고, 따옴표·주석·heredoc 본문·리다이렉션을
    걸러 낸다. 명령 치환($(...), `...`)의 안쪽은 별도 조각으로 꺼내고 바깥 인자 자리에는
    '$()'를 남긴다. 단어 나누기는 shlex가 한다.
    """

    def __init__(self, text: str):
        self.s = text
        self.n = len(text)
        self.segments: List[str] = []

    def argvs(self) -> List[List[str]]:
        self._scan(0, False)
        result = []
        for seg in self.segments:
            try:
                argv = shlex.split(seg)
            except ValueError:
                argv = seg.split()
            if argv:
                result.append(argv)
        return result

    def _flush(self, buf: List[str]) -> None:
        self.segments.append("".join(buf))
        buf.clear()

    def _scan(self, i: int, in_subst: bool) -> int:
        s, n = self.s, self.n
        buf: List[str] = []
        heredocs: List[str] = []
        depth = 0
        while i < n:
            c = s[i]
            if c == "\\":
                if not s.startswith("\\\n", i):
                    buf.append(s[i:i + 2])
                i += 2
            elif c == "'":
                j = s.find("'", i + 1)
                j = n if j < 0 else j + 1
                buf.append(s[i:j])
                i = j
            elif c == '"':
                i = self._dquote(i + 1, buf)
            elif c == "`":
                i = self._backtick(i + 1, buf)
            elif s.startswith("$(", i) or (c in "<>" and s.startswith("(", i + 1)):
                i = self._subst(i, buf)
            elif c == "#" and (not buf or buf[-1][-1] in " \t"):
                j = s.find("\n", i)
                i = n if j < 0 else j
            elif s.startswith("<<", i) and not s.startswith("<<<", i) and _HEREDOC_RE.match(s, i):
                m = _HEREDOC_RE.match(s, i)
                heredocs.append(next(g for g in m.groups() if g is not None))
                i = m.end()
            elif c in "<>" or s.startswith("&>", i):
                i = self._redirect(i, buf)
            elif c in _SEP:
                self._flush(buf)
                i += 1
                if c == "(":
                    depth += 1
                elif c == ")":
                    if depth:
                        depth -= 1
                    elif in_subst:
                        return i
                elif c == "\n" and heredocs:
                    i = self._skip_heredocs(i, heredocs)
                    heredocs = []
            else:
                buf.append(c)
                i += 1
        self._flush(buf)
        return i

    def _dquote(self, i: int, buf: List[str]) -> int:
        s, n = self.s, self.n
        buf.append('"')
        while i < n:
            c = s[i]
            if c == "\\":
                buf.append(s[i:i + 2])
                i += 2
            elif c == '"':
                buf.append('"')
                return i + 1
            elif c == "`":
                i = self._backtick(i + 1, buf)
            elif s.startswith("$(", i):
                i = self._subst(i, buf)
            else:
                buf.append(c)
                i += 1
        return i

    def _backtick(self, i: int, buf: List[str]) -> int:
        j = i
        while j < self.n and self.s[j] != "`":
            j += 2 if self.s[j] == "\\" else 1
        inner = _Splitter(self.s[i:j])
        inner._scan(0, False)
        self.segments.extend(inner.segments)
        buf.append("$()")
        return j + 1

    def _subst(self, i: int, buf: List[str]) -> int:
        if self.s.startswith("$((", i):
            j = self.s.find("))", i + 3)  # 산술 확장: 안쪽은 명령이 아니다
            i = self.n if j < 0 else j + 2
        else:
            i = self._scan(i + 2, True)
        buf.append("$()")
        return i

    def _redirect(self, i: int, buf: List[str]) -> int:
        s, n = self.s, self.n
        cur = "".join(buf)
        k = len(cur)
        while k and cur[k - 1].isdigit():
            k -= 1
        if k < len(cur) and (k == 0 or cur[k - 1] in " \t"):
            buf[:] = [cur[:k]]  # 2>&1 의 fd 번호
        while i < n and s[i] in "<>&|":
            i += 1
        while i < n and s[i] in " \t":
            i += 1
        while i < n and s[i] not in " \t" and s[i] not in _SEP and s[i] not in "<>":
            c = s[i]
            if c == "\\":
                i += 2
            elif c == "'":
                j = s.find("'", i + 1)
                i = n if j < 0 else j + 1
            elif c == '"':
                i = self._dquote(i + 1, [])
            elif s.startswith("$(", i):
                i = self._subst(i, [])
            else:
                i += 1
        return i

    def _skip_heredocs(self, i: int, delims: List[str]) -> int:
        s, n = self.s, self.n
        for delim in delims:
            while i < n:
                j = s.find("\n", i)
                line = s[i:] if j < 0 else s[i:j]
                i = n if j < 0 else j + 1
                if line.strip() == delim:
                    break
        return i


def split_command(command: str) -> List[List[str]]:
    """셸 명령 문자열을 단순 명령의 argv 목록으로 나눈다."""
    return _Splitter(command).argvs()


_ASSIGN_RE = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)=(.*)", re.S)
_KEYWORDS = ("{", "}", "!", "if", "then", "elif", "else", "do", "while", "until")
_WRAPPERS = ("sudo", "command", "builtin", "exec", "nohup", "time", "nice", "env", "xargs", "timeout")


def strip_prefix(argv: List[str]) -> Tuple[List[str], Dict[str, str]]:
    """앞에 붙은 변수 대입, 셸 키워드, env·sudo 같은 감싸기 명령을 떼어 낸다."""
    env: Dict[str, str] = {}
    i = 0
    while i < len(argv):
        a = argv[i]
        m = _ASSIGN_RE.fullmatch(a)
        if m:
            env[m.group(1)] = m.group(2)
            i += 1
        elif a in _KEYWORDS:
            i += 1
        elif os.path.basename(a) in _WRAPPERS:
            wrapper = os.path.basename(a)
            i += 1
            while i < len(argv) and argv[i].startswith("-"):
                i += 1
            if wrapper == "timeout" and i < len(argv):
                i += 1  # 제한 시간
        else:
            break
    return argv[i:], env


def _shell_c_arg(argv: List[str]) -> Optional[str]:
    has_c = False
    for a in argv[1:]:
        if a.startswith("--"):
            continue
        if a.startswith("-"):
            has_c = has_c or "c" in a[1:]
            continue
        return a if has_c else None
    return None


Command = namedtuple("Command", "prog argv env cwd depth")


def walk(command: str, cwd: str, depth: int = 0) -> Iterator[Command]:
    """단순 명령을 차례로 돌려준다. `cd`로 바뀐 작업 디렉터리를 따라가고
    `sh -c`, `bash -c`, `eval`의 안쪽 문자열도 펼친다."""
    if depth > MAX_DEPTH:
        return
    for argv in split_command(command):
        argv, env = strip_prefix(argv)
        if not argv:
            continue
        prog = os.path.basename(argv[0])
        if prog in ("cd", "pushd"):
            dirs = [a for a in argv[1:] if not a.startswith("-")]
            if dirs:
                cwd = resolve(dirs[0], cwd)
            elif len(argv) == 1:
                cwd = os.path.expanduser("~")
        elif prog in SHELLS and _shell_c_arg(argv) is not None:
            yield from walk(_shell_c_arg(argv), cwd, depth + 1)
        elif prog == "eval":
            yield from walk(" ".join(argv[1:]), cwd, depth + 1)
        else:
            yield Command(prog, argv, env, cwd, depth)


# ---------------------------------------------------------------- git 호출 해석

class Repo:
    """git 명령 하나가 가리키는 저장소(-C, --git-dir, --work-tree, GIT_DIR 반영)."""

    def __init__(self, cwd: str, git_dir: Optional[str] = None, work_tree: Optional[str] = None):
        self.cwd = cwd
        self.git_dir = resolve(git_dir, cwd) if git_dir else None
        self.work_tree = resolve(work_tree, cwd) if work_tree else None
        self._branch: Optional[str] = None
        self._branch_known = False

    def git(self, args: List[str]) -> Optional[str]:
        prefix = ["-C", nearest_dir(self.cwd)]
        if self.git_dir:
            prefix += ["--git-dir", self.git_dir]
        if self.work_tree:
            prefix += ["--work-tree", self.work_tree]
        return run_git(prefix + args)

    def branch(self) -> Optional[str]:
        """현재 브랜치 이름. detached HEAD이거나 저장소가 아니면 None."""
        if not self._branch_known:
            self._branch_known = True
            self._branch = self.git(["symbolic-ref", "--short", "-q", "HEAD"]) or None
        return self._branch

    def has_ref(self, ref: str) -> bool:
        return self.git(["show-ref", "--verify", "--quiet", ref]) is not None


GitCall = namedtuple("GitCall", "repo sub args overrides shell_alias")

_GIT_BUILTINS = set("""
add am annotate apply archive bisect blame branch bundle cat-file check-ignore checkout
cherry cherry-pick clean clone commit commit-tree config count-objects describe diff
diff-files diff-index diff-tree difftool fetch for-each-ref format-patch fsck gc grep
hash-object help init log ls-files ls-remote ls-tree maintenance merge merge-base
mergetool mv name-rev notes pull push range-diff read-tree rebase reflog remote repack
replace reset restore rev-list rev-parse revert rm shortlog show show-branch show-ref
sparse-checkout stash status submodule switch symbolic-ref tag update-index update-ref
var verify-commit version whatchanged worktree write-tree
""".split())


def parse_git(cmd: Command) -> Optional[GitCall]:
    """`git [전역 옵션] <하위 명령> [인자]`를 해석한다. 별칭은 한 번 펼친다.

    셸 별칭(`!`로 시작)이면 sub는 None이고 shell_alias에 실행될 셸 문자열이 담긴다.
    """
    argv = cmd.argv
    target = cmd.cwd
    git_dir = cmd.env.get("GIT_DIR")
    work_tree = cmd.env.get("GIT_WORK_TREE")
    overrides: List[str] = []
    i = 1
    while i < len(argv):
        a = argv[i]
        opt, eq, val = a.partition("=")
        if a == "-C" and i + 1 < len(argv):
            target = resolve(argv[i + 1], target)
            i += 2
        elif a == "-c" and i + 1 < len(argv):
            overrides.append(argv[i + 1])
            i += 2
        elif opt in ("--git-dir", "--work-tree"):
            if not eq:
                i += 1
                val = argv[i] if i < len(argv) else ""
            if opt == "--git-dir":
                git_dir = val
            else:
                work_tree = val
            i += 1
        elif opt in ("--namespace", "--config-env", "--super-prefix") and not eq:
            i += 2
        elif a.startswith("-"):
            i += 1
        else:
            break
    if i >= len(argv):
        return None
    repo = Repo(target, git_dir, work_tree)
    sub, args = argv[i], argv[i + 1:]
    if sub not in _GIT_BUILTINS:
        alias = repo.git(["config", "--get", "alias." + sub])
        if alias and alias.startswith("!"):
            shell = alias[1:] + "".join(" " + shlex.quote(a) for a in args)
            return GitCall(repo, None, args, overrides, shell)
        if alias:
            try:
                expanded = shlex.split(alias)
            except ValueError:
                expanded = alias.split()
            if expanded:
                sub, args = expanded[0], expanded[1:] + args
    return GitCall(repo, sub, args, overrides, None)
