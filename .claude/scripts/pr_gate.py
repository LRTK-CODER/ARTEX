#!/usr/bin/env python3
"""병합 전 재채점: main의 원래 테스트와 검사 설정으로 PR 코드를 다시 채점한다(Go).

main 체크아웃의 사본으로 실행한다. PR이 이 스크립트를 고쳤을 수 있기 때문이다.
검증 워커가 실행하고 출력을 그대로 보고서에 붙인다. 절차: .claude/skills/pm/SKILL.md

    python3 <main 체크아웃>/.claude/scripts/pr_gate.py 458

하는 일:
1. 저장소 밖 임시 디렉터리에 PR head의 트리를 꺼낸다. 테스트 파일(*_test.go, testdata/, web 테스트)과
   검사 설정 파일(CHECK_CONFIG_NAMES)을 저장소 어디에 있든 지우고 origin/main에 있는 것만 main 것으로
   둔 뒤 main 기준 검사를 돌린다. PR이 테스트나 설정을 고치거나 지워도 원래 기준으로 채점된다.
2. PR head 트리 그대로 같은 검사를 한 번 더 돌린다. 새로 추가한 테스트까지 통과하는지 본다.
3. `git diff --name-status -M origin/main...<head>`에서 기존 테스트 파일의 수정·삭제·이름 바꿈과
   검사 설정 변경을 뽑는다.
4. 두 트리의 테스트 함수 수, t.Skip 호출 수, testing.Short() 참조 수, web 테스트 파일 수를 비교하고
   실행되는 테스트가 줄어드는 쪽(테스트 함수·web 테스트 감소, skip·short 증가)을 표시한다.
5. 요약을 출력한다. 막지 않는다. 종료 코드는 [1](main 기준)의 검사가 실패하면 1, 그 밖에는 0이다.
   스크립트 자체를 실행하지 못했으면(main 체크아웃이 아님, fetch 실패) 2다.

검사는 Go만 돌린다(gofmt·go vet·go build·go test). 프런트는 커밋된 node_modules가 없어 돌리지 않고,
web 테스트 파일 변경은 [3]·[4]에 표시만 한다.

PR head는 `refs/pull/<번호>/head`에서 가져와 로컬 ref `refs/pr-gate/<번호>`에 둔다.
검증 워커가 병렬로 돌아도 PR마다 ref와 임시 디렉터리가 따로라 서로 덮어쓰지 않는다.
"""
from __future__ import annotations

import argparse
import io
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from typing import Dict, List, NamedTuple, Optional, Tuple

sys.dont_write_bytecode = True

SCRIPT_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

REMOTE = "origin"
MAIN_REF = "refs/remotes/origin/main"
# 검사 결과를 바꿀 수 있는 파일 이름(basename). main 기준 트리에서는 어디에 있든 지우고 main 것만 둔다.
CHECK_CONFIG_NAMES = (
    "go.mod", "go.sum", "go.work", "go.work.sum",
    "package.json", "package-lock.json", "tsconfig.json", "biome.json",
    ".golangci.yml", ".golangci.yaml",
)
# [3]에서 검사 설정으로 표시할 경로 접두(저장소 기준 상대 경로).
CONFIG_PREFIXES = (".github/workflows/", ".githooks/", ".claude/")
# 테스트 함수와 skip을 세는 정규식.
TEST_FUNC_RE = re.compile(r"^func\s+(Test|Benchmark|Fuzz|Example)\w")
SKIP_RE = re.compile(r"\bt\.Skip(f|Now)?\(")
SHORT_RE = re.compile(r"testing\.Short\(\)")
WEB_TEST_RE = re.compile(r"\.(test|spec)\.(ts|tsx|js|jsx|mjs|cjs)$")

GIT_TIMEOUT_SECONDS = 120
SETUP_TIMEOUT_SECONDS = 600
CHECK_TIMEOUT_SECONDS = 1200
FAILED_OUTPUT_LINES = 80
# 작업 트리에서 파일을 찾을 때 건너뛰는 디렉터리.
SKIP_WALK_DIRS = (".git", "node_modules")

CATEGORY_TESTS = "기존 테스트 파일의 수정·삭제·이름 바꿈"
CATEGORY_CONFIG = "검사 설정"
CATEGORIES = (CATEGORY_TESTS, CATEGORY_CONFIG)


class GateError(Exception):
    """재채점을 시작하지 못했다(fetch 실패, 잘못된 위치 등)."""


class Profile(NamedTuple):
    """트리에서 돌릴 명령."""

    setup: List[List[str]]
    checks: List[Tuple[str, List[str]]]


DEFAULT_PROFILE = Profile(
    setup=[],
    checks=[
        ("gofmt", ["sh", "-c", 'out=$(gofmt -l .); if [ -n "$out" ]; then echo "$out"; exit 1; fi']),
        ("go vet", ["go", "vet", "./..."]),
        ("go build", ["go", "build", "./..."]),
        ("go test", ["go", "test", "./..."]),
    ],
)


class CheckResult(NamedTuple):
    name: str
    is_ok: bool
    output: str


class TreeReport(NamedTuple):
    label: str
    checks: List[CheckResult]
    counts: Dict[str, int]

    @property
    def is_ok(self) -> bool:
        return all(c.is_ok for c in self.checks)


# ---------------------------------------------------------------- git


def git(repo: str, *args: str) -> str:
    """git 명령을 실행하고 표준 출력을 돌려준다. 실패하면 GateError."""
    try:
        done = subprocess.run(["git", "-C", repo] + list(args), capture_output=True, text=True,
                              timeout=GIT_TIMEOUT_SECONDS)
    except subprocess.TimeoutExpired as e:
        raise GateError("git {} 시간 초과({}초)".format(" ".join(args), GIT_TIMEOUT_SECONDS)) from e
    if done.returncode != 0:
        raise GateError("git {} 실패: {}".format(" ".join(args), done.stderr.strip()))
    return done.stdout


def main_checkout(repo: str) -> str:
    """repo가 속한 저장소의 main 체크아웃(공용 git 디렉터리의 상위) 경로."""
    common = git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir").strip()
    return os.path.dirname(common)


def main_checkout_problem(script_root: str, argv: List[str]) -> Optional[str]:
    """스크립트가 main 체크아웃 사본이 아니면 멈출 이유와 올바른 명령을, 맞으면 None을 돌려준다."""
    main = main_checkout(script_root)
    if os.path.realpath(main) == os.path.realpath(script_root):
        return None
    command = " ".join(["python3", os.path.join(main, ".claude", "scripts", "pr_gate.py")] + argv)
    return "\n".join([
        "이 스크립트는 main 체크아웃 사본으로 실행한다. PR이 스크립트를 고쳤을 수 있다.",
        "지금 위치: {}".format(script_root),
        "올바른 명령:",
        "  " + command,
    ])


def fetch(repo: str, pr: int) -> Tuple[str, str]:
    """origin/main과 PR head를 가져와 (main sha, head sha)를 돌려준다."""
    pr_ref = "refs/pr-gate/{}".format(pr)
    git(repo, "fetch", "--no-tags", REMOTE, "+refs/heads/main:" + MAIN_REF,
        "+refs/pull/{}/head:{}".format(pr, pr_ref))
    return (git(repo, "rev-parse", MAIN_REF).strip(), git(repo, "rev-parse", pr_ref).strip())


def extract(repo: str, rev: str, dest: str, paths: Optional[List[str]] = None) -> None:
    """rev의 트리(paths만 주면 그 경로만)를 dest에 꺼낸다."""
    cmd = ["git", "-C", repo, "archive", "--format=tar", rev]
    if paths:
        cmd += ["--"] + paths
    try:
        data = subprocess.run(cmd, capture_output=True, check=True,
                              timeout=GIT_TIMEOUT_SECONDS).stdout
    except subprocess.CalledProcessError as e:
        raise GateError("git archive {} 실패: {}".format(
            rev, e.stderr.decode(errors="replace").strip())) from e
    except subprocess.TimeoutExpired as e:
        raise GateError("git archive {} 시간 초과".format(rev)) from e
    with tarfile.open(fileobj=io.BytesIO(data)) as tar:
        tar.extractall(dest)


# ---------------------------------------------------------------- 분류 서술어


def is_test_file(path: str) -> bool:
    """테스트 파일인가(main 기준 트리에서 main 것으로 바꾸는 대상)."""
    if path.endswith("_test.go"):
        return True
    if WEB_TEST_RE.search(os.path.basename(path)):
        return True
    return "testdata" in path.split("/")  # testdata 디렉터리 안의 파일


def is_config_file(path: str) -> bool:
    """검사 설정 파일인가(basename 기준)."""
    return os.path.basename(path) in CHECK_CONFIG_NAMES


def is_overlay_path(path: str) -> bool:
    return is_test_file(path) or is_config_file(path)


def is_check_config(path: str) -> bool:
    """[3]에 검사 설정으로 표시할 경로인가. 경로는 저장소 기준 상대 경로다."""
    return is_config_file(path) or path.startswith(CONFIG_PREFIXES)


# ---------------------------------------------------------------- 트리 준비


def remove_overlay_targets(tree: str) -> None:
    """PR 트리에서 테스트 파일과 검사 설정 파일을 어디에 있든 지운다. main 것은 뒤에 다시 꺼낸다."""
    for root, dirs, files in os.walk(tree):
        dirs[:] = [d for d in dirs if d not in SKIP_WALK_DIRS]
        for name in files:
            full = os.path.join(root, name)
            rel = os.path.relpath(full, tree)
            if is_overlay_path(rel):
                os.remove(full)


def build_main_based_tree(repo: str, *, head: str, main: str, dest: str) -> None:
    """PR head 트리의 테스트 파일과 검사 설정 파일을 main 것으로 바꾼다."""
    extract(repo, head, dest)
    remove_overlay_targets(dest)
    paths = [p for p in git(repo, "ls-tree", "-r", "--name-only", main).splitlines()
             if is_overlay_path(p)]
    if paths:
        extract(repo, main, dest, paths)


# ---------------------------------------------------------------- 집계


def count_tests(tree: str) -> Dict[str, int]:
    """트리를 훑어 테스트 함수·skip·short·web 테스트 파일 수를 센다."""
    counts = {"go_tests": 0, "skip": 0, "short": 0, "web_tests": 0}
    for root, dirs, files in os.walk(tree):
        dirs[:] = [d for d in dirs if d not in SKIP_WALK_DIRS]
        for name in files:
            full = os.path.join(root, name)
            if name.endswith("_test.go"):
                try:
                    with open(full, encoding="utf-8", errors="replace") as f:
                        text = f.read()
                except OSError:
                    continue
                counts["go_tests"] += sum(1 for line in text.splitlines() if TEST_FUNC_RE.match(line))
                counts["skip"] += len(SKIP_RE.findall(text))
                counts["short"] += len(SHORT_RE.findall(text))
            elif WEB_TEST_RE.search(name):
                counts["web_tests"] += 1
    return counts


def compare_counts(before: Dict[str, int], after: Dict[str, int]) -> List[Tuple[str, int, int, bool]]:
    """(항목, main 기준, PR, 표시 여부) 목록. 실행되는 테스트가 줄어드는 쪽을 표시한다."""
    return [
        ("테스트 함수(Go)", before["go_tests"], after["go_tests"], after["go_tests"] < before["go_tests"]),
        ("t.Skip 호출", before["skip"], after["skip"], after["skip"] > before["skip"]),
        ("testing.Short() 참조", before["short"], after["short"], after["short"] > before["short"]),
        ("web 테스트 파일", before["web_tests"], after["web_tests"], after["web_tests"] < before["web_tests"]),
    ]


# ---------------------------------------------------------------- 검사 실행


def check_env() -> Dict[str, str]:
    env = dict(os.environ)
    # 부른 쪽 셸의 Go 옵션을 트리의 검사에 섞지 않는다. 두 트리가 같은 조건에서 돌아야 한다.
    for name in ("GOFLAGS", "GOWORK"):
        env.pop(name, None)
    env["GOWORK"] = "off"  # 저장소 밖 go.work를 줍지 않는다
    return env


def run_command(cmd: List[str], *, cwd: str, timeout_seconds: int) -> Tuple[bool, str]:
    """명령을 실행해 (성공 여부, 표준 출력과 오류를 합친 출력)을 돌려준다."""
    try:
        done = subprocess.run(cmd, cwd=cwd, env=check_env(), stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, text=True, timeout=timeout_seconds)
    except subprocess.TimeoutExpired as e:
        partial = e.output if isinstance(e.output, str) else ""
        return False, "{}\n시간 초과({}초)".format(partial, timeout_seconds)
    except OSError as e:
        return False, "실행하지 못했다: {}".format(e)
    return done.returncode == 0, done.stdout


def run_tree(label: str, tree: str, profile: Profile) -> TreeReport:
    for cmd in profile.setup:
        ok, output = run_command(cmd, cwd=tree, timeout_seconds=SETUP_TIMEOUT_SECONDS)
        if not ok:
            failed = CheckResult("설치: " + " ".join(cmd), False, output)
            return TreeReport(label, [failed], count_tests(tree))
    checks = []
    for name, cmd in profile.checks:
        ok, output = run_command(cmd, cwd=tree, timeout_seconds=CHECK_TIMEOUT_SECONDS)
        checks.append(CheckResult(name, ok, output))
    return TreeReport(label, checks, count_tests(tree))


# ---------------------------------------------------------------- 변경 분류


def classify(name_status: str) -> Dict[str, List[str]]:
    """`git diff --name-status -M` 출력에서 표시할 변경을 분류한다.

    한 항목은 검사 설정, 기존 테스트 순으로 처음 맞는 분류 하나에만 넣는다.
    기존 테스트는 main에 있던 파일(추가가 아닌 것)만 센다.
    """
    result: Dict[str, List[str]] = {c: [] for c in CATEGORIES}
    for line in name_status.splitlines():
        if not line.strip():
            continue
        fields = line.split("\t")
        status, paths = fields[0], fields[1:]
        entry = "{} {}".format(status, " -> ".join(paths))
        if any(is_check_config(p) for p in paths):
            result[CATEGORY_CONFIG].append(entry)
        elif status[0] in "MDR" and is_test_file(paths[0]):
            result[CATEGORY_TESTS].append(entry)
    return result


# ---------------------------------------------------------------- 출력


def tail(text: str, lines: int = FAILED_OUTPUT_LINES) -> str:
    return "\n".join(text.rstrip().splitlines()[-lines:])


def format_tree(number: int, title: str, report: TreeReport) -> List[str]:
    out = ["[{}] {}: {}".format(number, title, "통과" if report.is_ok else "실패")]
    for check in report.checks:
        out.append("    {} {}".format("통과" if check.is_ok else "실패", check.name))
    return out


def format_report(*, pr: int, head: str, main: str, merge_base: str, behind: int,
                  main_based: TreeReport, pr_tree: TreeReport,
                  changes: Dict[str, List[str]]) -> str:
    out = ["PR #{} 재채점".format(pr),
           "  PR head: {}".format(head),
           "  origin/main: {}".format(main),
           "  merge-base: {}{}".format(merge_base,
                                       " (PR이 main보다 커밋 {}개 뒤처져 있다)".format(behind)
                                       if behind else ""),
           ""]
    out += format_tree(1, "main 기준 재채점(PR 코드 + main의 테스트 파일과 검사 설정)", main_based)
    out += format_tree(2, "PR 트리 그대로", pr_tree)
    out += ["", "[3] 표시할 변경(git diff --name-status -M origin/main...head)"]
    for category in CATEGORIES:
        out.append("  {}:".format(category))
        out += ["    " + e for e in changes[category]] or ["    없음"]
    out += ["", "[4] 테스트 수 비교(main 기준 → PR 트리). '표시'는 실행되는 테스트가 줄어드는 쪽",
            "    프런트 검사는 돌리지 않는다. web 테스트 파일 수만 센다"]
    for name, before, after, is_flagged in compare_counts(main_based.counts, pr_tree.counts):
        out.append("  {}: {} → {} ({:+d}){}".format(
            name, before, after, after - before, "  표시" if is_flagged else ""))
    failed = [c.name for c in main_based.checks if not c.is_ok]
    out += ["", "[5] 결론: " + ("main 기준 재채점 실패({}). 종료 코드 1".format(", ".join(failed))
                              if failed else "main 기준 재채점 통과. 종료 코드 0")]
    for number, report in ((1, main_based), (2, pr_tree)):
        for check in report.checks:
            if not check.is_ok:
                out += ["", "--- [{}] {} 실패 출력(마지막 {}줄) ---".format(
                    number, check.name, FAILED_OUTPUT_LINES), tail(check.output)]
    return "\n".join(out) + "\n"


# ---------------------------------------------------------------- 진입


def default_workdir_base() -> str:
    """임시 디렉터리를 만들 곳. macOS는 /private/tmp, 그 밖은 시스템 임시 디렉터리."""
    return "/private/tmp" if os.path.isdir("/private/tmp") else tempfile.gettempdir()


def gate(repo: str, pr: int, *, profile: Profile = DEFAULT_PROFILE,
         workdir_base: Optional[str] = None, keep: bool = False) -> Tuple[int, str]:
    """재채점해 (종료 코드, 보고 문자열)을 돌려준다.

    Raises:
        GateError: fetch나 트리 준비에 실패했을 때.
    """
    main, head = fetch(repo, pr)
    merge_base = git(repo, "merge-base", main, head).strip()
    behind = int(git(repo, "rev-list", "--count", "{}..{}".format(merge_base, main)).strip())
    workdir = tempfile.mkdtemp(prefix="pr-gate-{}-".format(pr),
                               dir=workdir_base or default_workdir_base())
    try:
        main_tree = os.path.join(workdir, "main-based")
        pr_tree_dir = os.path.join(workdir, "pr")
        build_main_based_tree(repo, head=head, main=main, dest=main_tree)
        extract(repo, head, pr_tree_dir)
        main_based = run_tree("main 기준", main_tree, profile)
        pr_tree = run_tree("PR 트리", pr_tree_dir, profile)
        changes = classify(git(repo, "diff", "--name-status", "-M",
                               "{}...{}".format(main, head)))
        text = format_report(pr=pr, head=head, main=main, merge_base=merge_base, behind=behind,
                             main_based=main_based, pr_tree=pr_tree, changes=changes)
        if keep:
            text += "\n작업 디렉터리를 남겼다: {}\n".format(workdir)
        return (0 if main_based.is_ok else 1), text
    finally:
        if not keep:
            shutil.rmtree(workdir, ignore_errors=True)


def parse_args(argv: List[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="main의 원래 테스트와 검사 설정으로 PR 코드를 다시 채점한다(Go). 막지 않는다.",
        epilog="main 체크아웃 사본으로 실행한다. 종료 코드: main 기준 재채점 실패 1,"
               " 실행하지 못함 2, 그 밖 0.")
    p.add_argument("pr", type=int, help="PR 번호")
    p.add_argument("--keep", action="store_true", help="임시 디렉터리를 지우지 않는다(조사용)")
    return p.parse_args(argv)


def main(argv: List[str]) -> int:
    args = parse_args(argv)
    try:
        problem = main_checkout_problem(SCRIPT_ROOT, argv)
        if problem:
            print("중단: " + problem, file=sys.stderr)
            return 2
        code, text = gate(SCRIPT_ROOT, args.pr, keep=args.keep)
    except GateError as e:
        print("실패: {}".format(e), file=sys.stderr)
        return 2
    sys.stdout.write(text)
    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
