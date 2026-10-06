"""대기 검사(guard.py → wait_guard.py) 테스트. 실제 orca 대기는 띄우지 않는다.

실행: python3 -m unittest discover -s .claude/hooks/tests -v

도는 대기 확인(pgrep)은 결과가 그 순간의 프로세스에 달려 있어서 `running_waits`를 바꿔 끼운다.
"""
import json
import os
import subprocess
import sys
import unittest
from unittest import mock

sys.dont_write_bytecode = True

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
HOOKS_DIR = os.path.dirname(TESTS_DIR)
GUARD = os.path.join(HOOKS_DIR, "guard.py")
sys.path.insert(0, HOOKS_DIR)
import wait_guard  # noqa: E402
from hooklib import Blocked  # noqa: E402

WAIT = "python3 .claude/scripts/wait_workers.py --no-ack --timeout-ms 3600000"
REPO = "/Users/x/Documents/project/ARTEX"


def event(command, background=True):
    tool_input = {"command": command}
    if background is not None:
        tool_input["run_in_background"] = background
    return {"tool_name": "Bash", "tool_input": tool_input, "cwd": REPO}


class WaitGuardTest(unittest.TestCase):
    def check(self, command, background=True, running=()):
        with mock.patch.object(wait_guard, "running_waits", return_value=list(running)):
            wait_guard.check(event(command, background))

    def blocked(self, command, background=True, running=()):
        with self.assertRaises(Blocked) as ctx:
            self.check(command, background, running)
        return ctx.exception.message()

    def test_allowed(self):
        cases = [
            (WAIT, True),
            ("cd {} && {}".format(REPO, WAIT), True),
            ("cd {} &&\n  {}".format(REPO, WAIT), True),
            ("{} 2>&1".format(WAIT), True),
            ("python3 .claude/scripts/wait_workers.py --status", False),
            ("python3 .claude/scripts/wait_workers.py --status", None),
            ("orca orchestration check --wait --types worker_done --json", True),
            ("orca orchestration check --terminal term_1 --json", False),
            ("git status && ls &", False),
            ("cat .claude/scripts/wait_workers.py | head", False),
        ]
        for command, background in cases:
            with self.subTest(command=command, background=background):
                self.check(command, background)

    def test_shell_background_blocked(self):
        for command in (WAIT + " &", WAIT + " & disown", "nohup " + WAIT,
                        "setsid " + WAIT, "cd {} && nohup {} > /tmp/w.log 2>&1 &".format(REPO, WAIT),
                        "orca orchestration check --wait --json &"):
            with self.subTest(command=command):
                msg = self.blocked(command)
                self.assertIn("셸 백그라운드로 띄운 대기 명령", msg)
                self.assertIn("waiter_exists", msg)

    def test_combined_blocked(self):
        for command in (WAIT + "; echo done", WAIT + " && echo done", WAIT + " || true",
                        WAIT + " | tail -5", "echo start\n" + WAIT, "sleep 1 && " + WAIT,
                        "cd {}; {}".format(REPO, WAIT), "({})".format(WAIT),
                        "bash -c '{}'".format(WAIT), "x=$(orca orchestration check --wait --json)"):
            with self.subTest(command=command):
                self.assertIn("다른 명령과 묶은 대기 명령", self.blocked(command))

    def test_foreground_blocked(self):
        for background in (False, None):
            with self.subTest(background=background):
                msg = self.blocked(WAIT, background)
                self.assertIn("전경으로 띄운 대기 명령", msg)
                self.assertIn("run_in_background", msg)

    def test_running_wait_blocked(self):
        msg = self.blocked(WAIT, running=["4242"])
        self.assertIn("이미 도는 대기가 있는데 새로 띄운 대기 명령 (PID 4242)", msg)
        self.assertIn("끝난 뒤에 띄운다", msg)

    def test_unknown_running_state_allows(self):
        with mock.patch.object(wait_guard, "running_waits", return_value=None):
            wait_guard.check(event(WAIT))

    def test_message_shows_right_form(self):
        msg = self.blocked(WAIT + " &")
        self.assertIn("대신: 대기 명령만 단독으로(앞에 `cd <경로> &&`까지만) 쓰고 Bash 도구의 "
                      "run_in_background를 켜서 띄운다", msg)
        self.assertIn('.claude/skills/pm/SKILL.md "기다리기"', msg)

    def test_other_tools_ignored(self):
        with mock.patch.object(wait_guard, "running_waits", return_value=["1"]):
            wait_guard.check({"tool_name": "Write", "tool_input": {"file_path": "/tmp/x"}})


class RunningWaitsTest(unittest.TestCase):
    def run_with(self, result):
        def fake_run(cmd, *a, **kw):
            if isinstance(result, Exception):
                raise result
            return subprocess.CompletedProcess(cmd, result[0], stdout=result[1], stderr="")
        with mock.patch.object(wait_guard.subprocess, "run", fake_run):
            return wait_guard.running_waits()

    def test_found(self):
        self.assertEqual(self.run_with((0, "101\n202\n")), ["101", "202"])

    def test_none_running(self):
        self.assertEqual(self.run_with((1, "")), [])

    def test_unknown(self):
        self.assertIsNone(self.run_with((3, "")))
        self.assertIsNone(self.run_with(FileNotFoundError("pgrep")))
        self.assertIsNone(self.run_with(subprocess.TimeoutExpired("pgrep", 5)))


class GuardEntryTest(unittest.TestCase):
    def test_guard_runs_wait_check(self):
        payload = json.dumps(event(WAIT + " &"))
        p = subprocess.run([sys.executable, GUARD], input=payload, capture_output=True, text=True)
        self.assertEqual(p.returncode, 2, p.stderr)
        self.assertIn("셸 백그라운드로 띄운 대기 명령", p.stderr)


if __name__ == "__main__":
    unittest.main()
