"""PM 대기 검사. 규칙 문서: .claude/skills/pm/SKILL.md "기다리기"

`wait_workers.py`(`--status` 제외)나 `orca orchestration check --wait`는 Orca 서버에 대기를
등록한다. 셸 `&`로 띄웠다가 죽이면 프로세스가 끝나도 등록이 남고, 앞 대기가 도는 중에 새 대기를
띄우면 둘 다 다음 대기를 `waiter_exists`로 실패하게 만든다. 그래서 대기 명령은 아래 형태로만
허용한다. PM·워커 세션 모두에 적용한다.

- Bash 도구의 `run_in_background`로 띄운다.
- 셸 백그라운드(`&`), `disown`, `nohup`, `setsid`를 쓰지 않는다.
- 단독 명령이다. 앞에 `cd <경로> &&` 하나만 붙일 수 있다.
- 같은 사용자의 `orca ... orchestration check --wait` 프로세스가 이미 돌고 있지 않다.
  확인하지 못하면(pgrep 없음, 오류) 막지 않는다.
"""
from __future__ import annotations

import os
import re
import shlex
import subprocess
from typing import List, Optional, Tuple

import hooklib
from hooklib import Blocked

RULES_DOC = '.claude/skills/pm/SKILL.md "기다리기"'
WAIT_SCRIPT = "wait_workers.py"
DETACHERS = ("disown", "nohup", "setsid")
PGREP_TIMEOUT_SECONDS = 5
# wait_workers.py는 `orca orchestration check --wait ...`를 부른다. 옵션 순서가 달라도 잡는다.
RUNNING_WAIT_PATTERN = r"orchestration check .*--wait"

_PUNCTUATION = ";&|()<>\n"
_REDIRECTS = {">", ">>", "<", "<<", "<<<", ">&", "<&", "&>", "&>>", ">|", "<>"}
_OPERATOR_RE = re.compile(r"&&|\|\||;;|\|&|[;&|()\n]")

RIGHT_FORM = ("대기 명령만 단독으로(앞에 `cd <경로> &&`까지만) 쓰고 Bash 도구의 run_in_background를 켜서 띄운다. "
              "예: `python3 .claude/scripts/wait_workers.py --timeout-ms 3600000`. "
              "앞 대기가 돌고 있으면 그것이 끝난 뒤에 띄운다")


def _blocked(what: str, why: str) -> Blocked:
    return Blocked(what, why, RIGHT_FORM, RULES_DOC)


# ---------------------------------------------------------------- 대기 명령 알아보기

def is_wait(argv: List[str]) -> bool:
    """앞붙임(변수 대입, env·nohup 같은 감싸기)을 뗀 argv가 Orca 대기를 거는 명령인가."""
    argv, _ = hooklib.strip_prefix(argv)
    while argv and os.path.basename(argv[0]) in DETACHERS:  # setsid는 hooklib 감싸기 목록에 없다
        argv = argv[1:]
        while argv and argv[0].startswith("-"):
            argv = argv[1:]
        argv, _ = hooklib.strip_prefix(argv)
    if not argv:
        return False
    prog = os.path.basename(argv[0])
    if prog == "orca":
        return "orchestration" in argv and "check" in argv and "--wait" in argv
    is_script = prog == WAIT_SCRIPT or (
        prog.startswith("python") and any(os.path.basename(a) == WAIT_SCRIPT for a in argv[1:]))
    return is_script and "--status" not in argv


# ---------------------------------------------------------------- 셸 모양 나누기

def _tokens(command: str) -> List[Tuple[str, str]]:
    """명령 문자열을 ("word", 단어)와 ("op", 제어 연산자)로 나눈다. 리다이렉션은 단어로 둔다.

    따옴표 안의 `&`는 단어가 된다. 주석은 줄 끝까지 버리되 줄바꿈 연산자는 남긴다.
    """
    lexer = shlex.shlex(command, posix=True, punctuation_chars=_PUNCTUATION)
    lexer.whitespace = " \t\r"
    lexer.whitespace_split = True
    lexer.commenters = ""
    result: List[Tuple[str, str]] = []
    in_comment = False
    try:
        raw = list(lexer)
    except ValueError:  # 닫히지 않은 따옴표: 흔한 형태가 아니므로 단순히 나눈다
        raw = command.split()
    for tok in raw:
        is_punct = bool(tok) and all(c in _PUNCTUATION for c in tok)
        if in_comment:
            if is_punct and "\n" in tok:
                in_comment = False
                result.append(("op", "\n"))
            continue
        if not is_punct:
            if tok.startswith("#"):
                in_comment = True
                continue
            result.append(("word", tok))
        elif tok in _REDIRECTS:
            result.append(("word", tok))
        else:
            result.extend(("op", op) for op in _OPERATOR_RE.findall(tok))
    return result


def _segments(tokens: List[Tuple[str, str]]) -> Tuple[List[List[str]], List[str]]:
    """단순 명령 단어 목록과 그 사이 연산자. 연산자 바로 뒤나 앞뒤 끝의 줄바꿈은 줄 잇기로 본다."""
    segments: List[List[str]] = [[]]
    ops: List[str] = []
    for kind, value in tokens:
        if kind == "word":
            segments[-1].append(value)
        elif value == "\n" and (not segments[-1]):
            continue
        else:
            ops.append(value)
            segments.append([])
    if not segments[-1]:
        segments.pop()
        if ops and ops[-1] == "\n":
            ops.pop()
    return segments, ops


def _is_cd(words: List[str]) -> bool:
    return len(words) == 2 and words[0] == "cd"


def _shape_problem(command: str, cwd: str) -> Optional[Tuple[str, str]]:
    """대기 명령의 셸 모양 문제. 대기 명령이 없거나 문제가 없으면 None."""
    tokens = _tokens(command)
    segments, ops = _segments(tokens)
    has_top_wait = any(is_wait(words) for words in segments)
    if not has_top_wait and not any(is_wait(c.argv) for c in hooklib.walk(command, cwd)):
        return None
    words = [v for k, v in tokens if k == "word"]
    detachers = sorted({os.path.basename(w) for w in words} & set(DETACHERS))
    if "&" in ops or detachers:
        found = (["&"] if "&" in ops else []) + detachers
        return ("셸 백그라운드로 띄운 대기 명령 ({})".format(", ".join(found)),
                "셸에서 떼어 띄운 대기를 죽이면 Orca에 대기 등록이 남아 다음 대기가 waiter_exists로 실패한다. "
                "프로세스를 kill해도 등록은 남는다")
    standalone = has_top_wait and (
        (len(segments) == 1 and not ops)
        or (len(segments) == 2 and ops == ["&&"] and _is_cd(segments[0]) and is_wait(segments[1])))
    if not standalone:
        return ("다른 명령과 묶은 대기 명령",
                "대기를 다른 명령(;, &&, ||, 파이프, 줄바꿈, 하위 셸)과 묶으면 언제 어떻게 끝나는지 "
                "알기 어렵고, 중간에 끊으면 Orca에 대기 등록이 남는다")
    return None


# ---------------------------------------------------------------- 도는 대기

def running_waits() -> Optional[List[str]]:
    """같은 사용자의 `orca ... orchestration check --wait` 프로세스 PID 목록. 확인하지 못하면 None."""
    try:
        p = subprocess.run(["pgrep", "-u", str(os.getuid()), "-f", RUNNING_WAIT_PATTERN],
                           capture_output=True, text=True, timeout=PGREP_TIMEOUT_SECONDS)
    except (OSError, subprocess.SubprocessError):
        return None
    if p.returncode == 1:  # 맞는 프로세스 없음
        return []
    if p.returncode != 0:
        return None
    return p.stdout.split()


# ---------------------------------------------------------------- 진입

def check(event: dict) -> None:
    if event.get("tool_name") != "Bash":
        return
    tool_input = event.get("tool_input") or {}
    command = str(tool_input.get("command") or "")
    cwd = event.get("cwd") or os.getcwd()
    problem = _shape_problem(command, cwd)
    if problem is not None:
        raise _blocked(*problem)
    if not any(is_wait(c.argv) for c in hooklib.walk(command, cwd)):
        return
    if tool_input.get("run_in_background") is not True:
        raise _blocked("전경으로 띄운 대기 명령",
                       "대기는 최대 수십 분 걸린다. 전경 명령은 도구 제한 시간에 끊기고, 끊긴 대기는 "
                       "Orca에 대기 등록이 남아 다음 대기가 waiter_exists로 실패한다")
    pids = running_waits()
    if pids:
        raise _blocked("이미 도는 대기가 있는데 새로 띄운 대기 명령 (PID {})".format(" ".join(pids)),
                       "한 터미널에는 대기가 하나만 걸린다. 앞 대기가 도는 동안 새 대기는 waiter_exists로 "
                       "실패한다. 남은 프로세스는 `ps -o args= -p {}`로 본다".format(shlex.quote(pids[0])))
