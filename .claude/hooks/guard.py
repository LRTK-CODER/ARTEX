#!/usr/bin/env python3
"""Claude Code PreToolUse hook 진입점.

stdin으로 도구 호출(JSON: tool_name, tool_input, cwd)을 받아 CHECKS의 검사 모듈을
차례로 실행한다. 어느 검사든 hooklib.Blocked를 던지면 그 이유를 stderr에 쓰고 exit 2로
막는다. 그 밖에는 exit 0이다. 입력을 읽지 못하는 등 hook 자체 오류는 작업을 멈추지
않도록 경고만 남기고 exit 0으로 끝낸다.

검사 모듈은 이 디렉터리에 두고 `check(event: dict) -> None` 함수를 제공한다.
"""
import json
import os
import sys

sys.dont_write_bytecode = True  # worktree에 __pycache__를 남기지 않는다


def _warn(msg: str) -> None:
    sys.stderr.write("[guard 경고] {} — 검사를 건너뛴다\n".format(msg))


def main() -> int:
    try:
        event = json.loads(sys.stdin.read())
        if not isinstance(event, dict):
            raise ValueError("JSON 객체가 아니다")
        event.setdefault("cwd", os.getcwd())
    except Exception as e:  # noqa: BLE001 - hook 입력 오류로 작업을 멈추지 않는다
        _warn("hook 입력을 읽지 못했다: {}".format(e))
        return 0

    try:
        from hooklib import Blocked
        import branching_guard
        import wait_guard
        import worker_guard

        checks = (branching_guard.check, worker_guard.check, wait_guard.check)  # 검사 모듈을 더할 때는 여기에 추가한다
        for check in checks:
            check(event)
    except Blocked as b:
        sys.stderr.write(b.message())
        return 2
    except Exception as e:  # noqa: BLE001 - hook 자체 오류로 작업을 멈추지 않는다
        _warn("hook 내부 오류: {}: {}".format(type(e).__name__, e))
        return 0
    return 0


if __name__ == "__main__":
    sys.exit(main())
