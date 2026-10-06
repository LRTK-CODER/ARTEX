"""PM 보조 스크립트(.claude/scripts/start_worker.py) 테스트. orca는 실행하지 않는다.

실행: python3 -m unittest discover -s .claude/hooks/tests -v
"""
import contextlib
import io
import json
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(TESTS_DIR)))
SCRIPT = os.path.join(REPO_ROOT, ".claude", "scripts", "start_worker.py")
sys.path.insert(0, os.path.dirname(SCRIPT))
import start_worker  # noqa: E402


def dry_run(*args):
    return subprocess.run([sys.executable, SCRIPT, "--dry-run"] + list(args),
                          capture_output=True, text=True)


class NameTest(unittest.TestCase):
    def test_valid(self):
        self.assertEqual(start_worker.branch_name("feat", 42, None), "feat/issue-42")
        self.assertEqual(start_worker.branch_name("docs", None, "readme-update"), "docs/readme-update")
        self.assertEqual(start_worker.orca_name("feat/issue-42"), "feat-issue-42")

    def test_invalid(self):
        for btype, issue, slug in (("feature", 1, None), ("feat", None, "x"),
                                   ("feat", None, "issue-3"), ("feat", 0, None),
                                   ("feat", None, "Bad-Name")):
            with self.subTest(btype=btype, issue=issue, slug=slug):
                with self.assertRaises(start_worker.UsageError):
                    start_worker.branch_name(btype, issue, slug)


class AgentSettingTest(unittest.TestCase):
    def setUp(self):
        self.wt = tempfile.mkdtemp()

    def tearDown(self):
        shutil.rmtree(self.wt)

    def read(self):
        with open(os.path.join(self.wt, ".claude", "settings.local.json"), encoding="utf-8") as f:
            return json.load(f)

    def test_creates_file(self):
        start_worker.write_agent_setting(self.wt, "reviewer")
        self.assertEqual(self.read(), {"agent": "reviewer"})

    def test_keeps_other_keys(self):
        os.makedirs(os.path.join(self.wt, ".claude"))
        with open(os.path.join(self.wt, ".claude", "settings.local.json"), "w") as f:
            json.dump({"agent": "x", "permissions": {"allow": ["Bash(ls)"]}}, f)
        start_worker.write_agent_setting(self.wt, "implementer")
        self.assertEqual(self.read(), {"agent": "implementer",
                                       "permissions": {"allow": ["Bash(ls)"]}})

    def test_unknown_role(self):
        with self.assertRaises(start_worker.UsageError):
            start_worker.write_agent_setting(self.wt, "admin")

    def test_agent_files_exist(self):
        for role in start_worker.ROLES:
            with self.subTest(role=role):
                self.assertTrue(os.path.isfile(
                    os.path.join(REPO_ROOT, ".claude", "agents", role + ".md")))


class DryRunTest(unittest.TestCase):
    def test_implementer_issue(self):
        p = dry_run("--role", "implementer", "--type", "feat", "--issue", "4242",
                    "--spec", "브리핑 파일을 읽고 그대로 수행한다: /private/tmp/b.md")
        self.assertEqual(p.returncode, 0, p.stderr)
        wt = start_worker.WORKTREE_PLACEHOLDER
        self.assertEqual(p.stdout.splitlines(), [
            "git -C {} fetch origin main".format(shlex.quote(REPO_ROOT)),
            "orca worktree create --name feat-issue-4242 --base-branch origin/main --json",
            "# {}/.claude/settings.local.json 에 {{\"agent\": \"implementer\"}} 기록".format(wt),
            "git -C '{}' branch -m feat/issue-4242".format(wt),
            "orca orchestration worker-start --spec '브리핑 파일을 읽고 그대로 수행한다: /private/tmp/b.md'"
            " --worktree 'path:{}' --agent claude --model claude-opus-5-5 --effort medium --json"
            .format(wt),
        ])

    def test_role_default_effort_and_override(self):
        cases = ((["--role", "researcher"], "--effort low"),
                 (["--role", "reviewer"], "--effort medium"),
                 (["--role", "implementer", "--effort", "high"], "--effort high"))
        for extra, expected in cases:
            with self.subTest(extra=extra):
                p = dry_run(*(extra + ["--type", "docs", "--slug", "survey-parsers", "--task", "task_1",
                                       "--run", "run_1"]))
                self.assertEqual(p.returncode, 0, p.stderr)
                last = p.stdout.splitlines()[-1]
                self.assertIn(expected, last)
                self.assertIn("--task task_1", last)
                self.assertTrue(last.endswith("--run run_1"))

    def test_bad_name_rejected(self):
        p = dry_run("--role", "implementer", "--type", "feature", "--slug", "x-y", "--spec", "s")
        self.assertEqual(p.returncode, 2)
        self.assertIn("오류:", p.stderr)
        self.assertEqual(p.stdout, "")

    def test_existing_branch_rejected(self):
        # 현재 체크아웃의 브랜치와 무관하게, 규칙에 맞는 이름의 브랜치를 직접 만들어 쓴다.
        branch = "test/existing-branch-fixture-{}".format(os.getpid())
        subprocess.run(["git", "-C", REPO_ROOT, "branch", branch], check=True)
        try:
            p = dry_run("--role", "implementer", "--type", "test",
                        "--slug", branch.partition("/")[2], "--spec", "s")
        finally:
            subprocess.run(["git", "-C", REPO_ROOT, "branch", "-D", branch], check=True,
                           capture_output=True)
        self.assertEqual(p.returncode, 2)
        self.assertEqual(p.stdout, "")
        self.assertIn("브랜치 '{}'가 이미 있다".format(branch), p.stderr)


class VerifyPrTest(unittest.TestCase):
    """--verify-pr: origin/main의 새 worktree에 리뷰어로 검증 워커를 띄운다."""

    def test_dry_run_plan(self):
        p = dry_run("--verify-pr", "4243", "--spec", "s")
        self.assertEqual(p.returncode, 0, p.stderr)
        wt = start_worker.WORKTREE_PLACEHOLDER
        self.assertEqual(p.stdout.splitlines(), [
            "git -C {} fetch origin main".format(shlex.quote(REPO_ROOT)),
            "orca worktree create --name chore-verify-pr-4243 --base-branch origin/main --json",
            "# {}/.claude/settings.local.json 에 {{\"agent\": \"reviewer\"}} 기록".format(wt),
            "git -C '{}' branch -m chore/verify-pr-4243".format(wt),
            "orca orchestration worker-start --spec s --worktree 'path:{}' --agent claude"
            " --model claude-opus-5-5 --effort medium --json".format(wt),
        ])

    def test_explicit_reviewer_and_effort(self):
        p = dry_run("--verify-pr", "4243", "--role", "reviewer", "--effort", "high", "--spec", "s")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("--effort high", p.stdout.splitlines()[-1])

    def test_rejects_other_role_and_naming_options(self):
        for extra in (["--role", "implementer"], ["--type", "feat"], ["--issue", "3"],
                      ["--slug", "a-b"], ["--base-branch", "origin/x"], ["--replace-role"]):
            with self.subTest(extra=extra):
                p = dry_run(*(["--verify-pr", "4243", "--spec", "s"] + extra))
                self.assertEqual(p.returncode, 2)
                self.assertIn("오류:", p.stderr)
                self.assertEqual(p.stdout, "")

    def test_rejects_other_targets(self):
        for extra in (["--worktree", "feat/issue-1"], ["--follow-up", "ctx_1"]):
            with self.subTest(extra=extra):
                p = dry_run(*(["--verify-pr", "4243", "--spec", "s"] + extra))
                self.assertEqual(p.returncode, 2)
                self.assertIn("not allowed", p.stderr)

    def test_existing_branch_prints_cleanup(self):
        branch = "chore/verify-pr-4244"
        subprocess.run(["git", "-C", REPO_ROOT, "branch", branch], check=True)
        try:
            p = dry_run("--verify-pr", "4244", "--spec", "s")
        finally:
            subprocess.run(["git", "-C", REPO_ROOT, "branch", "-D", branch], check=True,
                           capture_output=True)
        self.assertEqual(p.returncode, 2)
        self.assertEqual(p.stdout, "")
        self.assertIn("검증 브랜치 'chore/verify-pr-4244'가 이미 있다", p.stderr)
        self.assertIn("worker-release", p.stderr)
        self.assertIn("git branch -D chore/verify-pr-4244", p.stderr)


class RoleDefinitionCheckTest(unittest.TestCase):
    """역할 정의가 없는 worktree에서는 worker-start 전에 멈춘다."""

    def setUp(self):
        self.wt = os.path.realpath(tempfile.mkdtemp())
        subprocess.run(["git", "init", "-q", "-b", "LRTK-CODER/feat-issue-7", self.wt], check=True)

    def tearDown(self):
        shutil.rmtree(self.wt)

    def put_definition(self, role):
        os.makedirs(os.path.join(self.wt, ".claude", "agents"), exist_ok=True)
        open(os.path.join(self.wt, ".claude", "agents", role + ".md"), "w").close()

    def test_present_passes(self):
        self.put_definition("reviewer")
        start_worker.check_role_definition(self.wt, "reviewer")

    def test_missing_explains_and_cleans_up(self):
        self.put_definition("implementer")
        with self.assertRaises(start_worker.MissingRoleDefinition) as cm:
            start_worker.check_role_definition(self.wt, "reviewer")
        msg = str(cm.exception)
        self.assertIn(os.path.join(self.wt, ".claude", "agents", "reviewer.md"), msg)
        self.assertIn("main에 병합", msg)
        self.assertIn("orca worktree rm --worktree path:{} --force".format(self.wt), msg)
        self.assertIn("git branch -D LRTK-CODER/feat-issue-7", msg)

    def test_run_stops_before_worker_start(self):
        calls = []

        def fake_run(cmd, *a, **kw):
            calls.append(cmd)
            return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="")

        with mock.patch.object(start_worker.subprocess, "run", fake_run), \
                mock.patch.object(start_worker, "find_worktree", return_value=self.wt), \
                mock.patch("sys.stderr"):
            code = start_worker.main(["--role", "reviewer", "--type", "feat", "--issue", "7",
                                      "--spec", "s"])
        self.assertEqual(code, 1)
        self.assertEqual(calls[1][-3:], ["fetch", "origin", "main"])
        self.assertEqual(calls[2][:3], ["orca", "worktree", "create"])
        self.assertFalse(any(c[:3] == ["orca", "orchestration", "worker-start"] for c in calls))
        self.assertFalse(any("branch" in c and "-m" in c for c in calls))
        self.assertFalse(os.path.exists(os.path.join(self.wt, ".claude", "settings.local.json")))

    def test_dry_run_warns_when_main_lacks_definition(self):
        p = dry_run("--role", "reviewer", "--type", "feat", "--issue", "4242", "--spec", "s")
        self.assertEqual(p.returncode, 0, p.stderr)
        has = subprocess.run(["git", "-C", REPO_ROOT, "cat-file", "-e",
                              "main:.claude/agents/reviewer.md"],
                             capture_output=True).returncode == 0
        self.assertEqual("경고:" in p.stderr, not has)


def in_process(argv, **patches):
    """main()을 프로세스 안에서 돌려 (종료 코드, stdout, stderr)를 돌려준다."""
    out, err = io.StringIO(), io.StringIO()
    with contextlib.ExitStack() as stack:
        for name, value in patches.items():
            stack.enter_context(mock.patch.object(start_worker, name, value))
        stack.enter_context(contextlib.redirect_stdout(out))
        stack.enter_context(contextlib.redirect_stderr(err))
        code = start_worker.main(argv)
    return code, out.getvalue(), err.getvalue()


class ResearcherTest(unittest.TestCase):
    def test_no_type_uses_docs(self):
        p = dry_run("--role", "researcher", "--issue", "1", "--spec", "x")
        self.assertEqual(p.returncode, 0, p.stderr)
        lines = p.stdout.splitlines()
        self.assertIn("--name docs-issue-1", lines[1])
        self.assertIn("branch -m docs/issue-1", lines[3])
        self.assertIn("--effort low", lines[4])

    def test_other_roles_need_type(self):
        for role in ("implementer", "reviewer"):
            with self.subTest(role=role):
                p = dry_run("--role", role, "--issue", "1", "--spec", "x")
                self.assertEqual(p.returncode, 2)
                self.assertIn("--type이 필요하다", p.stderr)

    def test_base_branch_override(self):
        p = dry_run("--role", "researcher", "--issue", "1", "--spec", "x",
                    "--base-branch", "origin/feat/issue-9")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("--base-branch origin/feat/issue-9", p.stdout.splitlines()[1])

    def test_new_needs_name(self):
        p = dry_run("--role", "implementer", "--type", "feat", "--spec", "x")
        self.assertEqual(p.returncode, 2)
        self.assertIn("--issue나 --slug", p.stderr)


class ResolveWorktreeTest(unittest.TestCase):
    def setUp(self):
        self.main = os.path.realpath(tempfile.mkdtemp())
        self.wt = os.path.realpath(tempfile.mkdtemp())
        self.trees = [(self.main, "main"), (self.wt, "feat/issue-9"), ("/nowhere", None)]

    def tearDown(self):
        shutil.rmtree(self.main)
        shutil.rmtree(self.wt)

    def test_by_branch_and_path(self):
        self.assertEqual(start_worker.resolve_worktree("feat/issue-9", self.trees), self.wt)
        self.assertEqual(start_worker.resolve_worktree(self.wt + "/", self.trees), self.wt)

    def test_main_checkout_rejected(self):
        for sel in ("main", self.main):
            with self.subTest(sel=sel), self.assertRaises(start_worker.UsageError):
                start_worker.resolve_worktree(sel, self.trees)

    def test_unknown_rejected(self):
        with self.assertRaises(start_worker.UsageError):
            start_worker.resolve_worktree("feat/issue-10", self.trees)

    def test_parses_real_git_output(self):
        trees = start_worker.git_worktrees()
        self.assertEqual(os.path.realpath(trees[0][0]),
                         os.path.realpath(subprocess.run(
                             ["git", "-C", REPO_ROOT, "rev-parse", "--path-format=absolute",
                              "--git-common-dir"], capture_output=True, text=True
                         ).stdout.strip() + "/.."))


class ExistingWorktreeTest(unittest.TestCase):
    """--worktree: 만들지도 이름을 바꾸지도 않고, 역할 파일만 바꿔 쓴다."""

    def setUp(self):
        self.wt = os.path.realpath(tempfile.mkdtemp())
        os.makedirs(os.path.join(self.wt, ".claude", "agents"))
        for role in start_worker.ROLES:
            open(os.path.join(self.wt, ".claude", "agents", role + ".md"), "w").close()
        self.trees = [("/main", "main"), (self.wt, "feat/issue-9")]

    def tearDown(self):
        shutil.rmtree(self.wt)

    def record(self, role):
        start_worker.write_agent_setting(self.wt, role)

    def go(self, *extra, active=()):
        calls = []

        def fake_run(cmd, *a, **kw):
            calls.append(cmd)
            return subprocess.CompletedProcess(cmd, 0, stdout="", stderr="")

        code, out, err = in_process(
            ["--worktree", "feat/issue-9", "--spec", "s"] + list(extra),
            git_worktrees=lambda: self.trees,
            active_worker_paths=lambda run: set(active),
            subprocess=mock.Mock(run=fake_run, CalledProcessError=subprocess.CalledProcessError))
        return code, out, err, calls

    def test_dry_run_plan(self):
        self.record("implementer")
        code, out, err, calls = self.go("--role", "implementer", "--effort", "high", "--dry-run")
        self.assertEqual(code, 0, err)
        self.assertEqual(out.splitlines(), [
            "# {}/.claude/settings.local.json 에 {{\"agent\": \"implementer\"}} 기록".format(self.wt),
            "orca orchestration worker-start --spec s --worktree path:{} --agent claude"
            " --model claude-opus-5-5 --effort high --json".format(self.wt),
        ])
        self.assertEqual(err, "")
        self.assertEqual(calls, [])

    def test_run_writes_role_without_create_or_rename(self):
        code, out, err, calls = self.go("--role", "reviewer")
        self.assertEqual(code, 0, err)
        self.assertEqual(start_worker.read_agent_setting(self.wt), "reviewer")
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][:3], ["orca", "orchestration", "worker-start"])

    def test_conflict_with_active_worker_stops(self):
        self.record("implementer")
        code, out, err, calls = self.go("--role", "reviewer", active=[self.wt])
        self.assertEqual(code, 1)
        self.assertIn("중단:", err)
        self.assertIn("--replace-role", err)
        self.assertIn("재시작", err)
        self.assertEqual(calls, [])
        self.assertEqual(start_worker.read_agent_setting(self.wt), "implementer")

    def test_conflict_replaced_with_flag_warns(self):
        self.record("implementer")
        code, out, err, calls = self.go("--role", "reviewer", "--replace-role", active=[self.wt])
        self.assertEqual(code, 0, err)
        self.assertIn("경고:", err)
        self.assertIn("--role implementer", err)
        self.assertEqual(start_worker.read_agent_setting(self.wt), "reviewer")

    def test_no_active_worker_warns_and_overwrites(self):
        self.record("implementer")
        code, out, err, calls = self.go("--role", "reviewer", active=["/elsewhere"])
        self.assertEqual(code, 0, err)
        self.assertIn("경고:", err)
        self.assertEqual(start_worker.read_agent_setting(self.wt), "reviewer")

    def test_same_role_no_warning(self):
        self.record("reviewer")
        code, out, err, calls = self.go("--role", "reviewer", active=[self.wt])
        self.assertEqual(code, 0, err)
        self.assertEqual(err.count("경고:"), 0)

    def test_missing_definition_stops(self):
        os.remove(os.path.join(self.wt, ".claude", "agents", "reviewer.md"))
        code, out, err, calls = self.go("--role", "reviewer")
        self.assertEqual(code, 1)
        self.assertIn("main을 merge", err)
        self.assertNotIn("orca worktree rm", err)
        self.assertEqual(calls, [])

    def test_creation_options_rejected(self):
        for extra in (["--type", "feat"], ["--issue", "9"], ["--base-branch", "origin/main"],
                      ["--repo", "name:ARTEX"]):
            with self.subTest(extra=extra):
                code, out, err, calls = self.go("--role", "reviewer", *extra)
                self.assertEqual(code, 2)
                self.assertIn("--worktree에는", err)

    def test_active_worker_paths_parses_worker_list(self):
        payload = {"result": {"workers": [
            {"terminalState": "active", "resource": {"worktreeId": "r1::" + self.wt}},
            {"terminalState": "released", "resource": {"worktreeId": "r1::/gone"}},
        ]}}
        seen = []

        def fake_run(cmd, *a, **kw):
            seen.append(cmd)
            return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(payload), stderr="")

        with mock.patch.object(start_worker.subprocess, "run", fake_run):
            self.assertEqual(start_worker.active_worker_paths("run_1"), {self.wt})
        self.assertEqual(seen[0][-2:], ["--run", "run_1"])


class FollowUpTest(unittest.TestCase):
    """--follow-up: 끝난 워커의 터미널에 새 작업을 보낸다."""

    def go(self, *argv):
        return in_process(list(argv),
                          dispatch_target=lambda d: ("term_abc", "/wt/feat-issue-9"))

    def test_dry_run_plan(self):
        code, out, err = self.go("--follow-up", "ctx_1", "--spec", "리뷰 지적 반영", "--run", "run_1",
                                 "--dry-run")
        self.assertEqual(code, 0, err)
        self.assertEqual(out.splitlines(), [
            "orca orchestration worker-start --spec '리뷰 지적 반영' --worktree path:/wt/feat-issue-9"
            " --terminal term_abc --json --run run_1",
        ])

    def test_rejects_launch_options(self):
        for extra in (["--effort", "high"], ["--model", "m"], ["--role", "implementer"],
                      ["--issue", "9"], ["--worktree", "feat/issue-9"]):
            with self.subTest(extra=extra):
                with mock.patch("sys.stderr", io.StringIO()):
                    try:
                        code, out, err = self.go("--follow-up", "ctx_1", "--spec", "s", *extra)
                    except SystemExit as e:  # argparse의 상호 배제
                        code = e.code
                self.assertEqual(code, 2)

    def test_dispatch_target_parses_worker_show(self):
        payload = {"result": {"worker": {"agentTerminalHandle": "term_x",
                                         "worktreeId": "repo::/wt/a"}}}

        def fake_run(cmd, *a, **kw):
            return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(payload), stderr="")

        with mock.patch.object(start_worker.subprocess, "run", fake_run):
            self.assertEqual(start_worker.dispatch_target("ctx_1"), ("term_x", "/wt/a"))

    def test_dispatch_target_missing_handle(self):
        def fake_run(cmd, *a, **kw):
            return subprocess.CompletedProcess(cmd, 0, stdout='{"result": {"worker": {}}}',
                                               stderr="")

        with mock.patch.object(start_worker.subprocess, "run", fake_run):
            with self.assertRaises(RuntimeError):
                start_worker.dispatch_target("ctx_1")


class HelpTest(unittest.TestCase):
    def test_help_mentions_new_options(self):
        p = subprocess.run([sys.executable, SCRIPT, "--help"], capture_output=True, text=True)
        self.assertEqual(p.returncode, 0)
        for text in ("--worktree", "--follow-up", "--replace-role", "--base-branch",
                     "docs", "재시작", "--role implementer로 역할을 되돌린다",
                     "--verify-pr", "chore/verify-pr-<번호>", "커밋·push하지 않는 작업용"):
            with self.subTest(text=text):
                self.assertIn(text, p.stdout)


if __name__ == "__main__":
    unittest.main()
