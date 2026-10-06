"""워커 격리 검사(guard.py → worker_guard.py)와 settings.json hook 명령 테스트.

실행: python3 -m unittest discover -s .claude/hooks/tests -v

임시 디렉터리는 워커에게 허용된 곳이라 "worktree 밖"을 시험할 수 없다. 그래서 저장소는
이 폴더 아래 `.sandbox-*`(.gitignore 처리)에 만든다. main 체크아웃, 워커 worktree 둘,
관계없는 저장소 하나를 두고 hook을 실제 프로세스로 실행한다.
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
REPO_ROOT = os.path.dirname(os.path.dirname(HOOKS_DIR))
GUARD = os.path.join(HOOKS_DIR, "guard.py")
SETTINGS = os.path.join(REPO_ROOT, ".claude", "settings.json")


def git(*args, cwd):
    subprocess.run(["git"] + list(args), cwd=cwd, check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


class SandboxBase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.sandbox = os.path.realpath(tempfile.mkdtemp(prefix=".sandbox-", dir=TESTS_DIR))
        cls.main = os.path.join(cls.sandbox, "ARTEX")
        cls.wt = os.path.join(cls.sandbox, "wt-a")
        cls.other_wt = os.path.join(cls.sandbox, "wt-b")
        cls.other_repo = os.path.join(cls.sandbox, "other")
        for d in (cls.main, cls.other_repo):
            os.makedirs(d)
            git("init", "-q", "-b", "main", cwd=d)
            git("config", "user.email", "t@example.com", cwd=d)
            git("config", "user.name", "t", cwd=d)
            git("commit", "-q", "--allow-empty", "-m", "init", cwd=d)
        git("worktree", "add", "-q", "-b", "feat/issue-1", cls.wt, cwd=cls.main)
        git("worktree", "add", "-q", "-b", "feat/issue-2", cls.other_wt, cwd=cls.main)
        cls.wt_git_dir = os.path.join(cls.main, ".git", "worktrees", "wt-a")

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.sandbox, ignore_errors=True)


class WorkerGuardTest(SandboxBase):
    def run_guard(self, tool, tool_input, cwd, project=None):
        env = dict(os.environ)
        env["CLAUDE_PROJECT_DIR"] = project or cwd
        payload = json.dumps({"tool_name": tool, "tool_input": tool_input, "cwd": cwd})
        p = subprocess.run([sys.executable, GUARD], input=payload, capture_output=True,
                           text=True, env=env)
        return p.returncode, p.stderr

    def expect(self, code, tool, key, values, cwd, project=None):
        for v in values:
            with self.subTest(tool=tool, value=v, cwd=os.path.basename(cwd)):
                got, err = self.run_guard(tool, {key: v}, cwd, project)
                self.assertEqual(got, code, err)
                if code == 2:
                    for label in ("차단:", "이유:", "대신:"):
                        self.assertIn(label, err)

    def bash(self, code, cmds, cwd=None, project=None):
        self.expect(code, "Bash", "command", cmds, cwd or self.wt, project)

    # ------------------------------------------------------------ 파일 도구

    def test_write_inside_allowed(self):
        paths = [
            os.path.join(self.wt, "a.txt"),
            "src/new/deep/file.py",
            "./a.txt",
            os.path.join(self.wt_git_dir, "COMMIT_EDITMSG"),
            "/tmp/x.txt",
            "/private/tmp/artex-orchestration/reports/r.md",
            os.path.join(tempfile.gettempdir(), "x.txt"),
            "~/.claude/projects/x/memory/m.md",
        ]
        for tool in ("Write", "Edit"):
            self.expect(0, tool, "file_path", paths, self.wt)
        self.expect(0, "NotebookEdit", "notebook_path", ["n.ipynb"], self.wt)

    def test_write_outside_blocked(self):
        paths = [
            os.path.join(self.main, "a.txt"),
            os.path.join(self.other_wt, "a.txt"),
            os.path.join(self.other_repo, "a.txt"),
            "../wt-b/a.txt",
            "../ARTEX/.claude/hooks/guard.py",
            os.path.join(self.wt, "..", "ARTEX", "x"),
            os.path.join(self.main, ".git", "config"),
            os.path.expanduser("~/.zshrc"),
            "/etc/hosts",
        ]
        for tool in ("Write", "Edit"):
            self.expect(2, tool, "file_path", paths, self.wt)
        self.expect(2, "NotebookEdit", "notebook_path", ["../ARTEX/n.ipynb"], self.wt)

    def test_symlink_escape_blocked(self):
        link = os.path.join(self.wt, "to-main")
        os.symlink(self.main, link)
        try:
            self.expect(2, "Write", "file_path", [os.path.join(link, "a.txt")], self.wt)
        finally:
            os.remove(link)

    # ------------------------------------------------------------ 병합

    def test_pr_merge_blocked(self):
        self.bash(2, [
            "gh pr merge 999999",
            "gh pr merge --squash 999999",
            "gh -R LRTK-CODER/ARTEX pr merge 999999",
            "echo ok && gh pr merge 999999 --auto",
            "bash -c 'gh pr merge 999999'",
            "gh api -X PUT repos/o/r/pulls/999999/merge",
        ])

    def test_other_gh_allowed(self):
        self.bash(0, [
            "gh pr create --title 'chore: x' --body 'y'",
            "gh pr view 1",
            "gh pr list",
            "gh api repos/o/r/pulls/1",
            "gh issue view 3",
        ])

    # ------------------------------------------------------------ git 대상

    def test_git_inside_allowed(self):
        self.bash(0, [
            "git status",
            "git add -A && git commit -m 'feat: x'",
            "git -C . log --oneline",
            "git -C src status",
            "cd src && git status",
            "git fetch origin && git merge origin/main",
            "git log main --oneline",
            "git -C /tmp status",
            "cd /private/tmp && git init -q scratch",
            "git --git-dir={} status".format(self.wt_git_dir),
        ])

    def test_git_outside_blocked(self):
        self.bash(2, [
            "git -C {} status".format(self.main),
            "git -C ../ARTEX log",
            "cd {} && git status".format(self.main),
            "cd .. && git -C wt-b commit -m x",
            "cd ../ARTEX; git pull --ff-only",
            "git -C {} commit -m x".format(self.other_repo),
            "GIT_DIR={}/.git git log".format(self.main),
            "git --git-dir={}/.git log".format(self.other_repo),
            "git --work-tree={} status".format(self.main),
            "bash -c 'cd {} && git status'".format(self.other_wt),
            "cd ~ && git status",
        ])

    def test_shell_alias_expanded(self):
        git("config", "alias.upmain", "!git -C {} pull".format(self.main), cwd=self.main)
        try:
            self.bash(2, ["git upmain"])
        finally:
            git("config", "--unset", "alias.upmain", cwd=self.main)

    def test_non_git_commands_not_checked(self):
        self.bash(0, ["ls {}".format(self.main), "cat {}/README".format(self.main), "cd .. && ls"])

    # ------------------------------------------------------------ 워커가 아닐 때

    def test_main_checkout_not_restricted(self):
        self.bash(0, ["gh pr merge 999999", "git -C {} status".format(self.wt)], cwd=self.main)
        self.expect(0, "Write", "file_path", [os.path.join(self.wt, "a.txt"), "/etc/hosts"],
                    self.main)

    def test_project_dir_decides(self):
        # 세션 프로젝트가 main 체크아웃이면 cwd가 worktree여도 워커가 아니다
        self.bash(0, ["gh pr merge 999999"], cwd=self.wt, project=self.main)
        # 세션 프로젝트가 worktree면 cwd가 main이어도 워커다
        self.bash(2, ["gh pr merge 999999"], cwd=self.main, project=self.wt)

    def test_non_repo_not_restricted(self):
        non_repo = os.path.realpath(tempfile.mkdtemp())
        try:
            self.bash(0, ["gh pr merge 999999"], cwd=non_repo)
        finally:
            shutil.rmtree(non_repo)


class SettingsCommandTest(SandboxBase):
    """settings.json의 hook 명령이 main 체크아웃 사본을 먼저 쓰고, 없으면 worktree 사본을 쓴다."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        with open(SETTINGS, encoding="utf-8") as f:
            settings = json.load(f)
        entries = settings["hooks"]["PreToolUse"]
        cls.matchers = [e["matcher"] for e in entries]
        cls.commands = {h["command"] for e in entries for h in e["hooks"]}

    def put_marker(self, checkout, name):
        d = os.path.join(checkout, ".claude", "hooks")
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "guard.py"), "w", encoding="utf-8") as f:
            f.write("import sys; sys.stderr.write({!r}); sys.exit(2)\n".format(name))

    def run_command(self, project):
        (command,) = self.commands
        env = dict(os.environ)
        env["CLAUDE_PROJECT_DIR"] = project
        p = subprocess.run(["sh", "-c", command], input="{}", capture_output=True, text=True,
                           env=env, cwd=project)
        return p.stderr

    def test_matchers(self):
        self.assertEqual(self.matchers, ["Bash", "Write|Edit|NotebookEdit"])
        self.assertEqual(len(self.commands), 1)

    def test_prefers_main_checkout_copy(self):
        self.put_marker(self.wt, "worktree")
        self.put_marker(self.main, "main")
        try:
            self.assertEqual(self.run_command(self.wt), "main")
            self.assertEqual(self.run_command(self.main), "main")
        finally:
            shutil.rmtree(os.path.join(self.main, ".claude"))
            shutil.rmtree(os.path.join(self.wt, ".claude"))

    def test_falls_back_to_worktree_copy(self):
        self.put_marker(self.wt, "worktree")
        try:
            self.assertEqual(self.run_command(self.wt), "worktree")
        finally:
            shutil.rmtree(os.path.join(self.wt, ".claude"))

    def test_real_guard_via_settings_command(self):
        # 이 저장소 자체에서: 어느 사본이 돌든 hook 입력 오류로 작업을 막지 않는다
        self.assertNotIn("Traceback", self.run_command(REPO_ROOT))


if __name__ == "__main__":
    unittest.main()
