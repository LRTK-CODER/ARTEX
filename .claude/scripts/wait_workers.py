#!/usr/bin/env python3
"""PM 보조: 워커 결과를 기다리고, 필요한 메시지만 요약하고, 받은 묶음을 확인 처리한다.
절차: .claude/skills/pm/SKILL.md

main 체크아웃(run에 묶인 PM 터미널)에서 실행한다.

    python3 .claude/scripts/wait_workers.py                   # worker_done·escalation·question·heartbeat를 기다린다
    python3 .claude/scripts/wait_workers.py --timeout-ms 60000 --run run_x
    python3 .claude/scripts/wait_workers.py --status          # 기다리지 않고 워커 상태를 요약한다

하는 일:
1. `orca orchestration check --wait --types worker_done,escalation,question,heartbeat --json`으로
   기다린다. stderr의 keepalive 줄(15초마다)은 버린다.
   heartbeat도 대기 종류에 넣는 이유: orca는 걸린 대기의 --types에 든 종류를 PM 채팅 알림에서
   뺀다. 넣지 않으면 heartbeat마다 PM 채팅에 알림이 뜬다. 한계: 대기가 걸려 있지 않은 틈에 온
   heartbeat는 여전히 알림으로 뜬다.
2. heartbeat는 워커별로 개수와 마지막 phase·시각만 한 줄씩 요약한다. 묶음으로 받은 것과
   `check --all --types heartbeat`(읽음 처리하지 않는다)로 읽은 것을 id로 합친다.
   개수는 run이 받은 누적 개수다.
   나머지 메시지는 하나씩 사람이 읽을 형태로 출력한다.
3. `check --ack <deliveryId>`로 받은 묶음을 확인 처리한다(--no-ack이면 하지 않는다).
   run에 묶인 check는 확인 처리 전까지 같은 묶음을 계속 다시 준다. 확인 처리하지 않으면
   이미 처리한 worker_done이 다음 실행 때 새 완료처럼 다시 나온다.
   heartbeat만 든 묶음은 확인 처리하고 남은 시간 동안 계속 기다린다.

종료 코드: 메시지를 받으면 0, 타임아웃이면 2, orca 오류면 1.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from typing import Dict, List, Optional

sys.dont_write_bytecode = True

WAIT_TYPES = "worker_done,escalation,question,heartbeat"
DEFAULT_TIMEOUT_MS = 30 * 60 * 1000
EXIT_OK, EXIT_ORCA_ERROR, EXIT_TIMEOUT = 0, 1, 2


WAITER_EXISTS_HINT = (
    "원인: 이 터미널(run)에 다른 대기가 이미 걸려 있다. 앞 대기가 아직 돌고 있거나, 셸 `&`로 띄웠다가 "
    "죽인 대기의 등록이 Orca에 남았다.\n"
    "대처: `pgrep -fl 'orchestration check .*--wait'`로 남은 대기 프로세스를 찾는다. 프로세스를 kill해도 "
    "등록은 남으니, 앞 대기가 끝나거나 타임아웃될 때까지 기다린 뒤 다시 띄운다.")


class OrcaError(Exception):
    def __init__(self, message: str, code: str = ""):
        super().__init__(message)
        self.code = code


def is_keepalive(line: str) -> bool:
    """`check --wait`가 stderr에 내는 keepalive JSON 줄인가. `_heartbeat`는 옛 이름이다."""
    try:
        data = json.loads(line)
    except ValueError:
        return False
    return isinstance(data, dict) and ("_keepalive" in data or "_heartbeat" in data)


def strip_keepalive(text: str) -> List[str]:
    return [line for line in text.splitlines() if line.strip() and not is_keepalive(line)]


def orca(args: List[str]) -> dict:
    """orca 명령을 실행하고 JSON의 result를 돌려준다. 실패하면 OrcaError."""
    cmd = ["orca"] + args
    try:
        p = subprocess.run(cmd, capture_output=True, text=True)
    except OSError as e:
        raise OrcaError("orca를 실행하지 못했다: {}".format(e))
    for line in strip_keepalive(p.stderr or ""):
        print(line, file=sys.stderr)
    body = "\n".join(strip_keepalive(p.stdout or ""))
    try:
        data = json.loads(body)
    except ValueError:
        raise OrcaError("orca 출력이 JSON이 아니다(종료 코드 {}): {}".format(p.returncode, body[:500]))
    if p.returncode != 0 or not data.get("ok"):
        err = data.get("error") or {}
        raise OrcaError("{} 실패(종료 코드 {}): {} {}".format(
            " ".join(cmd[:3]), p.returncode, err.get("code", ""), err.get("message", "")).rstrip(),
            str(err.get("code") or ""))
    return data.get("result") or {}


def payload_of(msg: dict) -> dict:
    raw = msg.get("payload")
    if isinstance(raw, dict):
        return raw
    try:
        data = json.loads(raw or "{}")
    except ValueError:
        return {}
    return data if isinstance(data, dict) else {}


def worker_key(msg: dict) -> str:
    """heartbeat를 묶는 기준. dispatch id가 없으면 보낸 터미널로 묶는다."""
    return payload_of(msg).get("dispatchId") or msg.get("from_handle") or "?"


def summarize_heartbeats(messages: List[dict]) -> Dict[str, dict]:
    """heartbeat를 워커별로 센다. 같은 메시지 id는 한 번만 센다.

    messages는 오래된 것부터 온다고 본다. created_at은 초 단위라서 같은 초에 온 것은 뒤의 것을
    마지막으로 친다.
    """
    summary: Dict[str, dict] = {}
    seen = set()
    for msg in messages:
        if msg.get("type") != "heartbeat" or msg.get("id") in seen:
            continue
        seen.add(msg.get("id"))
        p = payload_of(msg)
        s = summary.setdefault(worker_key(msg), {"count": 0, "task": p.get("taskId"),
                                                 "phase": None, "at": None})
        s["count"] += 1
        at = msg.get("created_at")
        if s["at"] is None or (at or "") >= s["at"]:
            s["at"], s["phase"] = at, p.get("phase")
            s["task"] = p.get("taskId") or s["task"]
    return summary


def format_heartbeats(summary: Dict[str, dict]) -> List[str]:
    lines = []
    for key in sorted(summary):
        s = summary[key]
        lines.append("heartbeat {} (task {}): {}회, 마지막 phase {}, {}".format(
            key, s["task"] or "-", s["count"], s["phase"] or "-", s["at"] or "-"))
    return lines


def format_message(msg: dict) -> List[str]:
    """heartbeat가 아닌 메시지 하나. reply에는 메시지 id가 필요하다."""
    p = payload_of(msg)
    lines = [
        "[{}] {}".format(msg.get("type", "?"), msg.get("id", "?")),
        "  dispatch: {}  task: {}  보낸 터미널: {}".format(
            p.get("dispatchId") or "-", p.get("taskId") or "-", msg.get("from_handle") or "-"),
        "  제목: {}".format(msg.get("subject") or "-"),
    ]
    if p.get("outcome"):
        lines.append("  outcome: {}".format(p["outcome"]))
    if p.get("reportPath"):
        lines.append("  보고서: {}".format(p["reportPath"]))
    files = p.get("filesModified")
    if files:
        lines.append("  바뀐 파일: {}".format(", ".join(files) if isinstance(files, list) else files))
    body = (msg.get("body") or "").strip()
    if body:
        lines.append("  본문:")
        lines.extend("    " + line for line in body.splitlines())
    if msg.get("type") == "question":
        lines.append("  답하기: orca orchestration reply --id {} --body \"<답>\"".format(msg.get("id")))
    return lines


def run_flag(run: Optional[str]) -> List[str]:
    return ["--run", run] if run else []


def check_wait(run: Optional[str], timeout_ms: int) -> dict:
    return orca(["orchestration", "check", "--wait", "--types", WAIT_TYPES,
                 "--timeout-ms", str(max(timeout_ms, 1)), "--json"] + run_flag(run))


def ack(run: Optional[str], delivery_id: str) -> dict:
    """묶음을 확인 처리한다. orca는 확인 처리 뒤 다음 묶음을 새 deliveryId로 함께 돌려준다.
    그 묶음은 확인 처리되지 않았으므로 다음 check에서 replayed로 다시 나온다."""
    return orca(["orchestration", "check", "--ack", delivery_id, "--json"] + run_flag(run))


def all_heartbeats(run: Optional[str]) -> List[dict]:
    """run이 받은 heartbeat 전체를 오래된 것부터. --all은 읽음 처리하지 않고 최신 것부터 준다."""
    result = orca(["orchestration", "check", "--all", "--types", "heartbeat", "--json"] + run_flag(run))
    return list(reversed(result.get("messages") or []))


def print_heartbeats(run: Optional[str], received: List[dict]) -> None:
    """묶음으로 받은 heartbeat와 --all로 읽은 heartbeat를 합쳐 워커별로 한 줄씩 출력한다."""
    messages = list(received)
    try:
        messages += all_heartbeats(run)
    except OrcaError as e:
        print("경고: heartbeat를 읽지 못했다: {}".format(e), file=sys.stderr)
    for line in format_heartbeats(summarize_heartbeats(messages)):
        print(line)


def wait(args: argparse.Namespace, clock=time.monotonic) -> int:
    deadline = clock() + args.timeout_ms / 1000.0
    heartbeats: List[dict] = []
    while True:
        remaining_ms = int((deadline - clock()) * 1000)
        if remaining_ms <= 0:
            return timed_out(args.run, heartbeats)
        result = check_wait(args.run, remaining_ms)
        if result.get("cancelled") or result.get("connectionLost"):
            raise OrcaError("check가 중간에 끝났다(cancelled={}, connectionLost={})".format(
                result.get("cancelled"), result.get("connectionLost")))
        messages = result.get("messages") or []
        if result.get("timedOut") or not messages:
            return timed_out(args.run, heartbeats)
        heartbeats += [m for m in messages if m.get("type") == "heartbeat"]
        others = [m for m in messages if m.get("type") != "heartbeat"]
        delivery = result.get("deliveryId")
        if not others:
            # heartbeat만 든 묶음: 확인 처리하지 않으면 같은 묶음이 계속 돌아온다.
            if delivery:
                ack(args.run, delivery)
            continue
        print_heartbeats(args.run, heartbeats)
        for msg in others:
            print()
            for line in format_message(msg):
                print(line)
        print()
        if args.no_ack:
            print("확인 처리하지 않았다(--no-ack). 묶음 {}은 다음 실행 때 다시 나온다".format(delivery))
        elif delivery:
            nxt = ack(args.run, delivery)
            pending = nxt.get("count") or len(nxt.get("messages") or [])
            print("묶음 {}을 확인 처리했다.{}".format(
                delivery, " 다음 묶음에 {}개가 대기 중이다".format(pending) if pending else ""))
        return EXIT_OK


def timed_out(run: Optional[str], heartbeats: List[dict]) -> int:
    print_heartbeats(run, heartbeats)
    print("타임아웃: worker_done·escalation·question 메시지가 오지 않았다")
    return EXIT_TIMEOUT


def worktree_name(worktree_id: Optional[str]) -> str:
    """`<repo id>::<경로>` 꼴의 worktreeId에서 끝 디렉터리 이름만 꺼낸다."""
    if not worktree_id:
        return "-"
    return os.path.basename(worktree_id.split("::")[-1].rstrip("/")) or worktree_id


def format_worker(w: dict) -> str:
    resource = w.get("resource") or {}
    projection = w.get("projection") or {}
    return "{}  {}  워커={}  진행={}  해제={}".format(
        w.get("dispatchId") or "-", worktree_name(resource.get("worktreeId")),
        w.get("workerState") or "-", projection.get("outcome") or "-",
        resource.get("releaseState") or "-")


def status(args: argparse.Namespace) -> int:
    result = orca(["orchestration", "worker-list", "--json"] + run_flag(args.run))
    workers = result.get("workers") or []
    if not workers:
        print("워커 없음")
    for w in workers:
        print(format_worker(w))
    if (result.get("page") or {}).get("hasMore"):
        print("(더 있다: orca orchestration worker-list --cursor ...)")
    return EXIT_OK


def parse_args(argv: List[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description="워커 결과를 기다리고 요약한 뒤 확인 처리한다")
    p.add_argument("--run", help="orchestration run id(생략하면 이 터미널에 묶인 run)")
    p.add_argument("--timeout-ms", type=int, default=DEFAULT_TIMEOUT_MS,
                   help="최대 대기 시간(기본 30분)")
    p.add_argument("--no-ack", action="store_true", help="받은 묶음을 확인 처리하지 않는다")
    p.add_argument("--status", action="store_true",
                   help="기다리지 않고 worker-list를 워커당 한 줄로 요약한다")
    return p.parse_args(argv)


def main(argv: List[str]) -> int:
    args = parse_args(argv)
    try:
        return status(args) if args.status else wait(args)
    except OrcaError as e:
        print("orca 오류: {}".format(e), file=sys.stderr)
        if e.code == "waiter_exists":
            print(WAITER_EXISTS_HINT, file=sys.stderr)
        return EXIT_ORCA_ERROR


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
