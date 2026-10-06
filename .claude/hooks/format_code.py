#!/usr/bin/env python3
"""Claude Code PostToolUse hook: Edit·Write로 바뀐 소스를 형식·린트 정리한다.

stdin으로 도구 호출(JSON: tool_input.file_path)을 받는다.
- `*.go`: `gofmt -w`로 형식을 고친다. 구문 오류 등으로 gofmt가 실패하면 그 출력을 보여 준다.
- `web/` 아래 프런트 소스(`*.ts`·`*.tsx`·`*.js`·`*.jsx`·`*.mjs`·`*.cjs`·`*.json`·`*.jsonc`·`*.css`):
  web의 로컬 biome(`web/node_modules/.bin/biome`)로 `biome check --write`를 돌린다.
  자동으로 고칠 수 없는 위반이 남으면 그 출력을 보여 준다.

- 고칠 수 없는 위반이 남으면 출력을 stderr에 쓰고 exit 2로 끝낸다. PostToolUse의
  exit 2는 도구가 이미 실행된 뒤 stderr를 Claude에게 보여 준다.
- 대상이 아니거나(확장자·위치), `.claude/` 아래이거나, 도구(gofmt·biome)를 찾지 못하면
  아무것도 하지 않고 exit 0이다.
- 입력을 읽지 못하는 등 hook 자체 오류도 작업을 멈추지 않도록 exit 0이다.

시스템 python3(3.9 이상)으로 돌므로 표준 라이브러리만 쓴다.
"""
import json
import os
import shutil
import subprocess
import sys

sys.dont_write_bytecode = True  # worktree에 __pycache__를 남기지 않는다

TIMEOUT_SECONDS = 120  # biome는 첫 실행이 느릴 수 있다
WEB_EXTS = (".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".json", ".jsonc", ".css")


def work_tree_root(path):
    """path가 들어 있는 git 작업 트리의 루트. git 밖이면 None."""
    p = subprocess.run(["git", "-C", os.path.dirname(path), "rev-parse", "--show-toplevel"],
                       capture_output=True, text=True)
    if p.returncode != 0:
        return None
    return os.path.realpath(p.stdout.strip())


def abs_path(event):
    """이벤트에서 검사할 파일의 절대 경로. 없으면 None."""
    path = (event.get("tool_input") or {}).get("file_path")
    if not isinstance(path, str) or not path:
        return None
    if not os.path.isabs(path):
        path = os.path.join(event.get("cwd") or os.getcwd(), path)
    path = os.path.realpath(path)
    return path if os.path.isfile(path) else None


def in_claude_dir(path, root):
    """path가 작업 트리의 .claude/ 아래인가. root가 없으면 경로 조각으로 본다."""
    if root is not None:
        rel = os.path.relpath(path, root)
        return rel.split(os.sep)[0] == ".claude"
    return ".claude" + os.sep in path


def run(cmd, cwd=None):
    return subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=TIMEOUT_SECONDS)


def format_go(path):
    """gofmt -w. (성공 여부, 보여 줄 출력)을 돌려준다."""
    gofmt = shutil.which("gofmt")
    if gofmt is None:
        return True, ""
    p = run([gofmt, "-w", "--", path])
    if p.returncode == 0:
        return True, ""
    return False, "gofmt가 형식을 고치지 못했다(구문 오류일 수 있다):\n" + p.stdout + p.stderr


def format_web(path, root):
    """web의 로컬 biome로 check --write. (성공 여부, 보여 줄 출력)을 돌려준다."""
    if root is None:
        return True, ""
    web_root = os.path.join(root, "web")
    if os.path.commonpath([path, web_root]) != web_root:
        return True, ""  # web/ 밖이면 대상이 아니다
    biome = os.path.join(web_root, "node_modules", ".bin", "biome")
    if not os.access(biome, os.X_OK):
        return True, ""  # 의존성 미설치면 건너뛴다
    p = run([biome, "check", "--write", "--no-errors-on-unmatched", path], cwd=web_root)
    if p.returncode == 0:
        return True, ""
    return False, "biome가 자동으로 고치지 못한 위반이 있다. 고친다(설정: web/biome.json):\n" + p.stdout + p.stderr


def handle(event):
    """(exit 코드, stderr 문자열)을 돌려준다."""
    path = abs_path(event)
    if path is None:
        return 0, ""
    root = work_tree_root(path)
    if in_claude_dir(path, root):
        return 0, ""  # .claude/ 아래 스크립트는 대상이 아니다
    ext = os.path.splitext(path)[1]
    if ext == ".go":
        ok, output = format_go(path)
    elif ext in WEB_EXTS:
        ok, output = format_web(path, root)
    else:
        return 0, ""
    return (0, "") if ok else (2, output)


def main():
    try:
        event = json.loads(sys.stdin.read())
        if not isinstance(event, dict):
            return 0
        code, output = handle(event)
    except Exception as e:  # noqa: BLE001 - hook 자체 오류로 작업을 멈추지 않는다
        sys.stderr.write("[format_code 경고] {}: {} — 검사를 건너뛴다\n".format(type(e).__name__, e))
        return 0
    if output:
        sys.stderr.write(output)
    return code


if __name__ == "__main__":
    sys.exit(main())
