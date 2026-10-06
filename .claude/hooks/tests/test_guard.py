"""Claude Code hook(guard.py → branching_guard.py) 테스트.

실행: python3 -m unittest discover -s .claude/hooks/tests -v

main 체크아웃과 연결 worktree 둘(작업 브랜치, Orca 임시 이름 브랜치)을 가진 임시 저장소를
만들고 hook을 실제 프로세스로 실행해 막을 것과 통과시킬 것을 확인한다.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
HOOKS_DIR = os.path.dirname(TESTS_DIR)
GUARD = os.path.join(HOOKS_DIR, "guard.py")
sys.path.insert(0, HOOKS_DIR)
import branching_guard  # noqa: E402
import hooklib  # noqa: E402


def git(*args, cwd):
    subprocess.run(["git"] + list(args), cwd=cwd, check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


class BranchNameTest(unittest.TestCase):
    def test_valid(self):
        for name in ("feat/issue-42", "fix/issue-1", "chore/branching-rules",
                     "docs/a-b-c-d-e", "ci/oauth2-login", "main",
                     "dependabot/pip/openai-3.16.2", "backup/2026-09-23-pre-redesign"):
            with self.subTest(name=name):
                self.assertIsNone(branching_guard.branch_name_problem(name))

    def test_invalid(self):
        for name in ("feature/login-page", "feat/login", "feat/a-b-c-d-e-f", "feat/Login-page",
                     "feat/login_page", "feat/issue-", "feat/issue-01", "feat/issue-x",
                     "feat/issue-login-page", "feat/a/b-c", "LRTK-CODER/chore-x", "feat",
                     "hotfix/issue-3", "feat/-a-b", "feat/a--b"):
            with self.subTest(name=name):
                self.assertIsNotNone(branching_guard.branch_name_problem(name))

    def test_types_come_from_file(self):
        self.assertEqual(branching_guard.load_types(),
                         ["feat", "fix", "chore", "docs", "refactor", "test", "ci"])
        self.assertIsNone(branching_guard.branch_name_problem("perf/issue-3", types=["perf"]))
        self.assertIsNotNone(branching_guard.branch_name_problem("feat/issue-3", types=["perf"]))


class SplitTest(unittest.TestCase):
    def test_segments(self):
        cases = {
            "git status && git push --force": [["git", "status"], ["git", "push", "--force"]],
            "a; b || c | d & e": [["a"], ["b"], ["c"], ["d"], ["e"]],
            "echo hi # it's a comment\ngit push -f": [["echo", "hi"], ["git", "push", "-f"]],
            "git log 2>&1 >/dev/null | head": [["git", "log"], ["head"]],
            "cat > f <<'EOF'\nit's; git push -f\nEOF\ngit status": [["cat"], ["git", "status"]],
            "echo `git push -f`": [["git", "push", "-f"], ["echo", "$()"]],
            "git -C x push \\\n  --force": [["git", "-C", "x", "push", "--force"]],
        }
        for cmd, expected in cases.items():
            with self.subTest(cmd=cmd):
                self.assertEqual(hooklib.split_command(cmd), expected)

    def test_command_substitution_in_double_quotes(self):
        cmd = ('git commit -m "$(cat <<\'EOF\'\nfeat: "don\'t" (x)\nEOF\n)" '
               '&& git push origin HEAD:main')
        self.assertEqual(hooklib.split_command(cmd), [
            ["cat"], ["git", "commit", "-m", "$()"], ["git", "push", "origin", "HEAD:main"]])

    def test_walk_follows_cd_and_shell_c(self):
        cmds = list(hooklib.walk("cd /a && bash -c 'cd b; git status'", "/x"))
        self.assertEqual([(c.prog, c.cwd) for c in cmds], [("git", "/a/b")])


class HookTestBase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.sandbox = os.path.realpath(tempfile.mkdtemp(prefix="guard-test-"))
        cls.main = os.path.join(cls.sandbox, "ARTEX")
        cls.wt = os.path.join(cls.sandbox, "wt-feat")
        cls.orca_wt = os.path.join(cls.sandbox, "wt-orca")
        os.makedirs(cls.main)
        git("init", "-q", "-b", "main", cwd=cls.main)
        git("config", "user.email", "t@example.com", cwd=cls.main)
        git("config", "user.name", "t", cwd=cls.main)
        git("commit", "-q", "--allow-empty", "-m", "init", cwd=cls.main)
        git("tag", "v1.0", cwd=cls.main)
        git("worktree", "add", "-q", "-b", "feat/issue-1", cls.wt, cwd=cls.main)
        git("worktree", "add", "-q", "-b", "LRTK-CODER/chore-tmp-task", cls.orca_wt, cwd=cls.main)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.sandbox, ignore_errors=True)

    def run_guard(self, tool, tool_input, cwd):
        payload = json.dumps({"tool_name": tool, "tool_input": tool_input, "cwd": cwd})
        env = {k: v for k, v in os.environ.items() if k != "CLAUDE_PROJECT_DIR"}  # 프로젝트는 cwd
        p = subprocess.run([sys.executable, GUARD], input=payload, capture_output=True, text=True,
                           env=env)
        return p.returncode, p.stderr

    def check(self, expected_code, cmds, cwd):
        for cmd in cmds:
            with self.subTest(cmd=cmd, cwd=os.path.basename(cwd)):
                code, err = self.run_guard("Bash", {"command": cmd}, cwd)
                self.assertEqual(code, expected_code, err)
                if expected_code == 2:
                    for label in ("차단:", "이유:", "대신:"):
                        self.assertIn(label, err)

    def blocked(self, cmds, cwd):
        self.check(2, cmds, cwd)

    def allowed(self, cmds, cwd):
        self.check(0, cmds, cwd)


class OnMainTest(HookTestBase):
    """main 브랜치 위에서."""

    def test_commit_push_merge_blocked(self):
        self.blocked([
            "git commit -m x",
            "git commit --allow-empty -m 'docs: x'",
            "git push",
            "git push -u origin HEAD",
            "git push origin main",
            "git merge feat/issue-1",
            "git status && git commit -am x",
            "bash -c 'git commit -m x'",
        ], self.main)

    def test_read_and_update_allowed(self):
        self.allowed([
            "git status",
            "git log --oneline -5 | head -3",
            "git pull --ff-only",
            "git fetch origin",
            "git merge --abort",
            "git push origin v1.0",
            "git push origin --tags",
            "git push origin --delete feat/issue-9",
            "echo 'git push --force'",
            "ls -la",
        ], self.main)

    def test_target_directory_decides(self):
        # 대상은 셸의 cwd가 아니라 git -C, cd로 옮긴 곳이다
        self.allowed([
            "git -C {} commit -m x".format(self.wt),
            "cd {} && git commit -m x".format(self.wt),
        ], self.main)
        self.blocked([
            "git -C {} commit -m x".format(self.main),
            "cd {} && git commit -m x".format(self.main),
            "cd .. && git -C ARTEX push",
            "GIT_DIR={}/.git git commit -m x".format(self.main),
            "git --git-dir={}/.git commit -m x".format(self.main),
        ], self.wt)

    def test_branch_creation_names(self):
        self.allowed([
            "git switch -c feat/issue-7",
            "git checkout -b chore/new-thing",
            "git branch docs/readme-update main",
            "git worktree add -b fix/issue-3 ../x main",
            "git worktree add ../x feat/issue-1",
            "git branch -d feat/issue-1",
            "git branch --list",
            "git branch -u origin/feat/issue-1 feat/issue-1",
        ], self.main)
        self.blocked([
            "git branch bad_name",
            "git switch -c feature/login",
            "git switch --create=Feat/x-y",
            "git checkout -b wip",
            "git checkout -qbwip",
            "git worktree add -b oops ../x",
            "git branch -m feat/issue-1 feat/issue-01",
        ], self.main)


class OnWorkBranchTest(HookTestBase):
    """작업 브랜치(feat/issue-1) 위에서."""

    def test_normal_work_allowed(self):
        self.allowed([
            "git status",
            "git add -A && git commit -m 'feat: 파서를 추가한다'",
            'git commit -m "-n is not a flag here"',
            "git commit -F msg.txt",
            "git push -u origin HEAD",
            "git push origin feat/issue-1",
            "git push origin HEAD:refs/heads/feat/issue-1",
            "git fetch origin && git merge origin/main",
            "gh pr create --title 'feat: x' --body 'Closes #1'",
            "git branch -m feat/issue-9",
            "git branch -M docs/readme-update",
            "git commit -m \"$(cat <<'EOF'\nfeat: \"don't\" stop\n\nbody\nEOF\n)\"",
            "cat > notes.md <<'EOF'\ndon't git push --force\nEOF",
        ], self.wt)

    def test_force_push_blocked(self):
        self.blocked([
            "git push --force",
            "git push -f origin HEAD",
            "git push -uf origin HEAD",
            "git push --force-with-lease",
            "git push --force-with-lease=feat/issue-1:abc origin HEAD",
            "git push --force-if-includes origin HEAD",
            "git push origin +HEAD",
            "git push origin +feat/issue-1:feat/issue-1",
            "git push --mirror origin",
            "git push --all origin",
            "echo ok; git push --force",
            "git status\ngit push -f",
            "sh -c 'git push --force'",
            "bash -lc \"cd . && git push -f\"",
            "FOO=1 git push -f",
            "env GIT_TRACE=1 git push -f",
            "echo $(git push -f)",
            "git push -f origin 'HEAD",
        ], self.wt)

    def test_push_to_main_blocked(self):
        self.blocked([
            "git push origin HEAD:main",
            "git push origin :main",
            "git push origin main",
            "git push origin feat/issue-1:refs/heads/main",
            "git push origin --delete main",
        ], self.wt)

    def test_upstream_main_blocked(self):
        git("config", "push.default", "upstream", cwd=self.main)
        git("config", "branch.feat/issue-1.merge", "refs/heads/main", cwd=self.main)
        try:
            self.blocked(["git push"], self.wt)
        finally:
            git("config", "--unset", "push.default", cwd=self.main)
            git("config", "--unset", "branch.feat/issue-1.merge", cwd=self.main)

    def test_skipping_hooks_blocked(self):
        self.blocked([
            "git commit --no-verify -m x",
            "git commit -n -m x",
            "git commit -anm x",
            "git commit -m x --no-verif",
            "git -c core.hooksPath=/dev/null commit -m x",
            "git -c core.hooksPath=/dev/null push -u origin HEAD",
        ], self.wt)

    def test_bad_branch_names_blocked(self):
        self.blocked([
            "git branch -m Bad",
            "git branch -m feat/issue-1 feat/issue-foo",
            "git switch -c feat/x",
            "git push origin HEAD:weird-name",
        ], self.wt)

    def test_alias_expanded(self):
        git("config", "alias.pf", "push --force", cwd=self.main)
        git("config", "alias.sh-pf", "!git push -f", cwd=self.main)
        try:
            self.blocked(["git pf", "git sh-pf origin HEAD"], self.wt)
        finally:
            git("config", "--unset", "alias.pf", cwd=self.main)
            git("config", "--unset", "alias.sh-pf", cwd=self.main)


class OrcaTemporaryNameTest(HookTestBase):
    """현재 이름이 Orca 임시 이름(LRTK-CODER/...)이면 push만 막는다."""

    def test_rename_and_commit_allowed(self):
        self.allowed([
            "git commit -m 'chore: x'",
            "git branch -m chore/tmp-task",
            "git status",
        ], self.orca_wt)

    def test_push_blocked(self):
        self.blocked([
            "git push -u origin HEAD",
            "git push",
            "git push origin LRTK-CODER/chore-tmp-task:chore/tmp-task",
        ], self.orca_wt)


class RobustnessTest(HookTestBase):
    def test_bad_input_does_not_block(self):
        for payload in ("", "not json", "[]"):
            with self.subTest(payload=payload):
                p = subprocess.run([sys.executable, GUARD], input=payload,
                                   capture_output=True, text=True)
                self.assertEqual(p.returncode, 0)
                self.assertIn("경고", p.stderr)

    def test_other_tools_and_dirs_pass(self):
        code, _ = self.run_guard("Write", {"file_path": "/etc/hosts", "content": ""}, self.main)
        self.assertEqual(code, 0)
        non_repo = tempfile.mkdtemp()
        try:
            self.allowed(["git status", "git commit -m x", "echo hi"], non_repo)
        finally:
            shutil.rmtree(non_repo)


if __name__ == "__main__":
    unittest.main()
