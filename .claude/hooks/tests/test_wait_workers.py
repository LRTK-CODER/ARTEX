"""PM 보조 스크립트(.claude/scripts/wait_workers.py) 테스트. orca는 실행하지 않는다.

샘플 JSON은 실제 `orca orchestration check --json`, `worker-list --json` 출력 모양을 줄인 것이다.

실행: python3 -m unittest discover -s .claude/hooks/tests -v

실제 orca로 시험할 때(수동):
- 워커 터미널에서 `orca orchestration run-create`를 하지 않는다. run-create는 호출한 터미널을 새 run의
  코디네이터로 묶는다. 그 터미널의 `check`는 이후 새 run의 메일함을 읽고, 되돌리려고 `run-use`를 하면
  PM run의 코디네이터 바인딩과 충돌한다.
- `orca terminal create`로 워커가 아닌 셸 터미널 둘(코디네이터용, 워커용)을 만든다. 코디네이터용에서
  run-create·task-create·`dispatch --to <워커용> --return-preamble`을 하고, 워커용에서 preamble의 send
  명령으로 heartbeat와 worker_done을 보낸 뒤, 코디네이터용에서 이 스크립트를 `--run <임시 run>`으로 돌린다.
- 이미 dispatch된 워커 안에서는 dispatch가 막힌다(nested_worker_depth_exceeded). worker_done은 실제
  task와 dispatch가 있어야 받아들여진다.
"""
import contextlib
import io
import json
import os
import subprocess
import sys
import unittest
from unittest import mock

sys.dont_write_bytecode = True

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(TESTS_DIR)))
sys.path.insert(0, os.path.join(REPO_ROOT, ".claude", "scripts"))
import wait_workers  # noqa: E402

KEEPALIVE = '{"_keepalive":true,"elapsedMs":15000}'


def message(mid, mtype, subject="", body="", created="2026-09-23T17:00:00Z", **payload):
    return {"id": mid, "run_id": "run_1", "from_handle": "term_w", "to_handle": "run:run_1",
            "subject": subject, "body": body, "type": mtype, "payload": json.dumps(payload),
            "created_at": created}


def batch(messages, delivery="delivery_1", timed_out=False):
    return {"runId": "run_1", "deliveryId": delivery if messages else None, "messages": messages,
            "count": len(messages), "acknowledged": None, "timedOut": timed_out,
            "cancelled": False, "connectionLost": False}


def envelope(result, ok=True, error=None):
    data = {"id": "x", "ok": ok, "_meta": {}}
    if ok:
        data["result"] = result
    else:
        data["error"] = error or {"code": "boom", "message": "실패"}
    return json.dumps(data, ensure_ascii=False, indent=2)


HB1 = message("msg_h1", "heartbeat", "alive", created="2026-09-23T17:00:00Z",
              taskId="task_a", dispatchId="ctx_a", phase="investigating")
HB2 = message("msg_h2", "heartbeat", "alive", created="2026-09-23T17:05:00Z",
              taskId="task_a", dispatchId="ctx_a", phase="implementing")
HB3 = message("msg_h3", "heartbeat", "alive", created="2026-09-23T17:03:00Z",
              taskId="task_b", dispatchId="ctx_b", phase="reviewing")
DONE = message("msg_d1", "worker_done", "구현 완료", "한 일.\n알아낸 것.\n남은 일.",
               taskId="task_a", dispatchId="ctx_a", outcome="succeeded",
               reportPath="/private/tmp/r.md", filesModified=["a.py", "b.py"])
QUESTION = message("msg_q1", "question", "질문", "A와 B 중 무엇으로 할까?",
                   taskId="task_b", dispatchId="ctx_b")


class FakeOrca:
    """subprocess.run 대역. check·ack 호출을 기록하고 준비한 응답을 차례로 돌려준다."""

    def __init__(self, waits, acks=None, stderr="", heartbeats=()):
        self.waits = list(waits)
        self.acks = list(acks or [])
        self.stderr = stderr
        self.heartbeats = list(heartbeats)
        self.calls = []

    def __call__(self, cmd, *a, **kw):
        self.calls.append(cmd)
        if "--all" in cmd:
            out = envelope({"messages": self.heartbeats, "count": len(self.heartbeats)})
            return subprocess.CompletedProcess(cmd, 0, stdout=out, stderr="")
        if "--ack" in cmd:
            out = self.acks.pop(0) if self.acks else envelope(batch([]))
            return subprocess.CompletedProcess(cmd, 0, stdout=out, stderr="")
        rc, out = self.waits.pop(0)
        return subprocess.CompletedProcess(cmd, rc, stdout=out, stderr=self.stderr)

    def ack_calls(self):
        return [c for c in self.calls if "--ack" in c]

    def wait_calls(self):
        return [c for c in self.calls if "--wait" in c]


def run_main(fake, argv):
    out, err = io.StringIO(), io.StringIO()
    with mock.patch.object(wait_workers.subprocess, "run", fake), \
            contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = wait_workers.main(argv)
    return code, out.getvalue(), err.getvalue()


class KeepaliveTest(unittest.TestCase):
    def test_strip(self):
        text = "\n".join([KEEPALIVE, '{"_heartbeat":true}', "진짜 경고", ""])
        self.assertEqual(wait_workers.strip_keepalive(text), ["진짜 경고"])

    def test_keepalive_not_printed(self):
        fake = FakeOrca([(0, envelope(batch([DONE])))], stderr=KEEPALIVE + "\n" + KEEPALIVE + "\n")
        code, out, err = run_main(fake, [])
        self.assertEqual(code, 0)
        self.assertNotIn("_keepalive", out + err)

    def test_keepalive_on_stdout_is_ignored(self):
        fake = FakeOrca([(0, KEEPALIVE + "\n" + envelope(batch([DONE])))])
        code, out, _ = run_main(fake, [])
        self.assertEqual(code, 0)
        self.assertIn("[worker_done] msg_d1", out)


class HeartbeatSummaryTest(unittest.TestCase):
    def test_per_worker_count_and_last_phase(self):
        summary = wait_workers.summarize_heartbeats([HB2, HB1, HB3, DONE, HB1])
        self.assertEqual(wait_workers.format_heartbeats(summary), [
            "heartbeat ctx_a (task task_a): 2회, 마지막 phase implementing, 2026-09-23T17:05:00Z",
            "heartbeat ctx_b (task task_b): 1회, 마지막 phase reviewing, 2026-09-23T17:03:00Z",
        ])

    def test_reads_heartbeats_with_all_without_consuming(self):
        # 묶음에 없던 heartbeat도 --all로 읽어 센다.
        fake = FakeOrca([(0, envelope(batch([DONE])))], heartbeats=[HB2, HB3, HB1])
        _, out, _ = run_main(fake, ["--run", "run_1"])
        self.assertIn(["orca", "orchestration", "check", "--all", "--types", "heartbeat",
                       "--json", "--run", "run_1"], fake.calls)
        lines = out.splitlines()
        self.assertEqual(lines[:2], [
            "heartbeat ctx_a (task task_a): 2회, 마지막 phase implementing, 2026-09-23T17:05:00Z",
            "heartbeat ctx_b (task task_b): 1회, 마지막 phase reviewing, 2026-09-23T17:03:00Z",
        ])
        self.assertNotIn("msg_h1", out)

    def test_same_second_uses_all_order(self):
        # --all은 최신 것부터 준다. 같은 초에 보낸 두 heartbeat 중 나중 것(implementing)이 마지막이다.
        early = message("msg_s1", "heartbeat", created="2026-09-23T17:12:12Z",
                        taskId="task_a", dispatchId="ctx_a", phase="investigating")
        late = message("msg_s2", "heartbeat", created="2026-09-23T17:12:12Z",
                       taskId="task_a", dispatchId="ctx_a", phase="implementing")
        fake = FakeOrca([(0, envelope(batch([DONE])))], heartbeats=[late, early])
        _, out, _ = run_main(fake, [])
        self.assertIn("heartbeat ctx_a (task task_a): 2회, 마지막 phase implementing", out)

    def test_batch_and_all_are_not_double_counted(self):
        fake = FakeOrca([(0, envelope(batch([HB1, HB2, DONE])))], heartbeats=[HB1, HB2])
        _, out, _ = run_main(fake, [])
        self.assertIn("heartbeat ctx_a (task task_a): 2회", out)

    def test_heartbeat_only_batch_is_acked_and_waits_again(self):
        fake = FakeOrca([(0, envelope(batch([HB1], delivery="delivery_h"))),
                         (0, envelope(batch([DONE], delivery="delivery_d")))])
        code, out, _ = run_main(fake, [])
        self.assertEqual(code, 0)
        self.assertEqual(len(fake.wait_calls()), 2)
        self.assertEqual([c[c.index("--ack") + 1] for c in fake.ack_calls()],
                         ["delivery_h", "delivery_d"])
        self.assertIn("heartbeat ctx_a (task task_a): 1회", out)

    def test_mixed_batch_prints_heartbeats_as_summary_only(self):
        fake = FakeOrca([(0, envelope(batch([HB1, HB3, DONE, HB2])))])
        code, out, _ = run_main(fake, [])
        self.assertEqual(code, 0)
        self.assertEqual(out.splitlines()[:2], [
            "heartbeat ctx_a (task task_a): 2회, 마지막 phase implementing, 2026-09-23T17:05:00Z",
            "heartbeat ctx_b (task task_b): 1회, 마지막 phase reviewing, 2026-09-23T17:03:00Z",
        ])
        self.assertNotIn("[heartbeat]", out)
        self.assertIn("[worker_done] msg_d1", out)
        self.assertEqual(len(fake.ack_calls()), 1)

    def test_heartbeat_only_batches_then_timeout(self):
        fake = FakeOrca([(0, envelope(batch([HB1], delivery="delivery_h1"))),
                         (0, envelope(batch([HB2], delivery="delivery_h2"))),
                         (0, envelope(batch([], timed_out=True)))])
        code, out, _ = run_main(fake, [])
        self.assertEqual(code, 2)
        self.assertEqual([c[c.index("--ack") + 1] for c in fake.ack_calls()],
                         ["delivery_h1", "delivery_h2"])
        self.assertIn("heartbeat ctx_a (task task_a): 2회, 마지막 phase implementing", out)

    def test_heartbeat_read_failure_is_only_a_warning(self):
        fake = FakeOrca([(0, envelope(batch([DONE])))])
        real = fake.__call__

        def failing(cmd, *a, **kw):
            if "--all" in cmd:
                return subprocess.CompletedProcess(cmd, 1, stdout=envelope(None, ok=False), stderr="")
            return real(cmd, *a, **kw)
        code, out, err = run_main(failing, [])
        self.assertEqual(code, 0)
        self.assertIn("경고: heartbeat를 읽지 못했다", err)
        self.assertIn("[worker_done] msg_d1", out)


class MessageFormatTest(unittest.TestCase):
    def test_worker_done(self):
        self.assertEqual(wait_workers.format_message(DONE), [
            "[worker_done] msg_d1",
            "  dispatch: ctx_a  task: task_a  보낸 터미널: term_w",
            "  제목: 구현 완료",
            "  outcome: succeeded",
            "  보고서: /private/tmp/r.md",
            "  바뀐 파일: a.py, b.py",
            "  본문:",
            "    한 일.",
            "    알아낸 것.",
            "    남은 일.",
        ])

    def test_question_shows_reply_command(self):
        self.assertEqual(wait_workers.format_message(QUESTION), [
            "[question] msg_q1",
            "  dispatch: ctx_b  task: task_b  보낸 터미널: term_w",
            "  제목: 질문",
            "  본문:",
            "    A와 B 중 무엇으로 할까?",
            "  답하기: orca orchestration reply --id msg_q1 --body \"<답>\"",
        ])

    def test_bad_payload(self):
        msg = dict(DONE, payload="not json")
        self.assertIn("  dispatch: -  task: -  보낸 터미널: term_w", wait_workers.format_message(msg))


class WaitTest(unittest.TestCase):
    def test_wait_command_and_ack(self):
        fake = FakeOrca([(0, envelope(batch([DONE, QUESTION])))])
        code, out, _ = run_main(fake, ["--run", "run_1", "--timeout-ms", "5000"])
        self.assertEqual(code, 0)
        first = fake.wait_calls()[0]
        self.assertEqual(first[:3], ["orca", "orchestration", "check"])
        for flag in ("--wait", "--json"):
            self.assertIn(flag, first)
        self.assertEqual(first[first.index("--types") + 1], "worker_done,escalation,question,heartbeat")
        self.assertEqual(first[first.index("--run") + 1], "run_1")
        self.assertLessEqual(int(first[first.index("--timeout-ms") + 1]), 5000)
        self.assertEqual(fake.ack_calls(), [["orca", "orchestration", "check", "--ack", "delivery_1",
                                             "--json", "--run", "run_1"]])
        self.assertIn("[worker_done] msg_d1", out)
        self.assertIn("[question] msg_q1", out)
        self.assertIn("묶음 delivery_1을 확인 처리했다", out)

    def test_default_timeout_is_30_minutes(self):
        fake = FakeOrca([(0, envelope(batch([DONE])))])
        run_main(fake, [])
        first = fake.wait_calls()[0]
        self.assertGreater(int(first[first.index("--timeout-ms") + 1]), 29 * 60 * 1000)
        self.assertNotIn("--run", first)

    def test_no_ack(self):
        fake = FakeOrca([(0, envelope(batch([DONE])))])
        code, out, _ = run_main(fake, ["--no-ack"])
        self.assertEqual(code, 0)
        self.assertEqual(fake.ack_calls(), [])
        self.assertIn("--no-ack", out)

    def test_reports_pending_next_batch(self):
        fake = FakeOrca([(0, envelope(batch([DONE])))],
                        acks=[envelope(batch([QUESTION], delivery="delivery_2"))])
        _, out, _ = run_main(fake, [])
        self.assertIn("다음 묶음에 1개가 대기 중이다", out)
        self.assertNotIn("msg_q1", out)

    def test_timeout_exit_code(self):
        fake = FakeOrca([(0, envelope(batch([], timed_out=True)))])
        code, out, _ = run_main(fake, ["--timeout-ms", "1000"])
        self.assertEqual(code, 2)
        self.assertIn("타임아웃", out)
        self.assertEqual(fake.ack_calls(), [])

    def test_timeout_still_summarizes_heartbeats(self):
        fake = FakeOrca([(0, envelope(batch([], timed_out=True)))], heartbeats=[HB3])
        code, out, _ = run_main(fake, [])
        self.assertEqual(code, 2)
        self.assertEqual(out.splitlines()[0],
                         "heartbeat ctx_b (task task_b): 1회, 마지막 phase reviewing, 2026-09-23T17:03:00Z")

    def test_orca_error_exit_code(self):
        for rc, out in ((1, envelope(None, ok=False)), (0, envelope(None, ok=False)),
                        (1, "not json")):
            with self.subTest(rc=rc, out=out[:20]):
                code, _, err = run_main(FakeOrca([(rc, out)]), [])
                self.assertEqual(code, 1)
                self.assertIn("orca 오류", err)

    def test_waiter_exists_explains_cause_and_remedy(self):
        error = {"code": "waiter_exists", "message": "a waiter is already registered"}
        code, _, err = run_main(FakeOrca([(1, envelope(None, ok=False, error=error))]), [])
        self.assertEqual(code, 1)
        self.assertIn("waiter_exists", err)
        self.assertIn("원인: 이 터미널(run)에 다른 대기가 이미 걸려 있다", err)
        self.assertIn("pgrep", err)
        self.assertIn("kill해도 등록은 남으니", err)

    def test_other_orca_error_has_no_waiter_hint(self):
        _, _, err = run_main(FakeOrca([(1, envelope(None, ok=False))]), [])
        self.assertNotIn("원인:", err)

    def test_orca_missing(self):
        def missing(cmd, *a, **kw):
            raise FileNotFoundError("orca")
        code, _, err = run_main(missing, [])
        self.assertEqual(code, 1)
        self.assertIn("orca 오류", err)


WORKERS = {"workers": [
    {"dispatchId": "ctx_1", "workerState": "ready", "terminalState": "active",
     "resource": {"releaseState": "not_requested",
                  "worktreeId": "repo-id::/Users/x/orca/workspaces/ARTEX/feat-issue-42"},
     "projection": {"outcome": "in_progress"}},
    {"dispatchId": "ctx_2", "workerState": "done",
     "resource": {"releaseState": "released", "worktreeId": None},
     "projection": {"outcome": "succeeded"}},
], "page": {"hasMore": False}}


class StatusTest(unittest.TestCase):
    def test_one_line_per_worker(self):
        fake = FakeOrca([(0, envelope(WORKERS))])
        code, out, _ = run_main(fake, ["--status", "--run", "run_1"])
        self.assertEqual(code, 0)
        self.assertEqual(fake.calls, [["orca", "orchestration", "worker-list", "--json",
                                       "--run", "run_1"]])
        self.assertEqual(out.splitlines(), [
            "ctx_1  feat-issue-42  워커=ready  진행=in_progress  해제=not_requested",
            "ctx_2  -  워커=done  진행=succeeded  해제=released",
        ])

    def test_empty(self):
        _, out, _ = run_main(FakeOrca([(0, envelope({"workers": []}))]), ["--status"])
        self.assertEqual(out.strip(), "워커 없음")

    def test_error(self):
        code, _, _ = run_main(FakeOrca([(1, envelope(None, ok=False))]), ["--status"])
        self.assertEqual(code, 1)


if __name__ == "__main__":
    unittest.main()
