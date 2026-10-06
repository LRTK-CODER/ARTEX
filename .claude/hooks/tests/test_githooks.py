""".githooks(pre-commit, pre-push) 테스트.

임시 저장소에 hook 파일을 복사하고 core.hooksPath를 건 뒤, 로컬 bare 저장소를 원격으로
두고 실제 git commit·push로 확인한다.
"""
import os
import shutil
import subprocess
import tempfile
import unittest

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(TESTS_DIR)))
HOOK_FILES = (".githooks/pre-commit", ".githooks/pre-push", ".claude/hooks/branching_guard.py",
              ".claude/hooks/hooklib.py", ".claude/hooks/branch-types.txt")

# gofmt가 다시 쓰는(형식이 어긋난) Go 파일과, 이미 정리된 Go 파일, 구문이 깨진 Go 파일.
BAD_GO = "package x\nfunc  f(){}\n"
GOOD_GO = "package x\n\nfunc f() {}\n"
UNPARSEABLE_GO = "package x\nfunc f( {\n"


def find_gofmt():
    return shutil.which("gofmt")


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)


class GitHooksTest(unittest.TestCase):
    def setUp(self):
        self.tmp = os.path.realpath(tempfile.mkdtemp(prefix="githooks-test-"))
        self.remote = os.path.join(self.tmp, "remote.git")
        self.repo = os.path.join(self.tmp, "repo")
        self.git("init", "-q", "--bare", "-b", "main", self.remote, cwd=self.tmp)
        self.git("init", "-q", "-b", "main", self.repo, cwd=self.tmp)
        for rel in HOOK_FILES:
            dst = os.path.join(self.repo, rel)
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copy2(os.path.join(REPO_ROOT, rel), dst)
        self.git("config", "user.email", "t@example.com")
        self.git("config", "user.name", "t")
        self.git("remote", "add", "origin", self.remote)
        self.git("add", "-A")
        self.git("commit", "-q", "-m", "init")
        self.git("push", "-q", "origin", "main")  # hook을 켜기 전에 원격 main을 만든다
        self.git("config", "core.hooksPath", ".githooks")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def git(self, *args, cwd=None, check=True):
        p = subprocess.run(["git"] + list(args), cwd=cwd or self.repo,
                           capture_output=True, text=True)
        if check and p.returncode != 0:
            self.fail("git {} 실패: {}".format(" ".join(args), p.stderr))
        return p

    def assertRejected(self, *args):
        p = self.git(*args, check=False)
        self.assertNotEqual(p.returncode, 0, "통과하면 안 된다: git {}".format(" ".join(args)))
        self.assertIn("차단:", p.stderr)
        return p

    def commit(self, msg):
        return self.git("commit", "-q", "--allow-empty", "-m", msg)

    def test_pre_commit_rejects_main(self):
        self.assertRejected("commit", "--allow-empty", "-m", "x")
        self.git("switch", "-q", "-c", "feat/issue-1")
        self.commit("feat: x")

    def test_pre_push_rejects_main(self):
        self.git("switch", "-q", "-c", "feat/issue-1")
        self.commit("feat: x")
        self.assertRejected("push", "origin", "HEAD:main")
        self.assertRejected("push", "origin", "feat/issue-1:refs/heads/main")

    def test_pre_push_branch_names(self):
        self.git("switch", "-q", "-c", "feat/issue-1")
        self.commit("feat: x")
        self.git("push", "-q", "-u", "origin", "HEAD")
        self.assertRejected("push", "origin", "HEAD:weird-name")
        self.git("switch", "-q", "-c", "LRTK-CODER/chore-tmp")
        self.assertRejected("push", "origin", "HEAD")
        self.assertRejected("push", "origin", "HEAD:chore/tmp-task")  # 로컬 이름도 본다
        self.git("branch", "-m", "chore/tmp-task")
        self.git("push", "-q", "origin", "HEAD")
        self.git("push", "-q", "origin", "--delete", "chore/tmp-task")

    def test_pre_push_rejects_non_fast_forward(self):
        self.git("switch", "-q", "-c", "fix/issue-2")
        self.commit("fix: a")
        self.git("push", "-q", "-u", "origin", "HEAD")
        self.git("reset", "-q", "--hard", "HEAD~1")
        self.commit("fix: b")
        p = self.assertRejected("push", "--force", "origin", "HEAD")
        self.assertIn("fast-forward", p.stderr)
        self.commit("fix: c")  # 정상 추가 커밋은 통과
        self.git("merge", "-q", "--no-edit", "-s", "ours", "origin/fix/issue-2")
        self.git("push", "-q", "origin", "HEAD")

    def test_pre_push_allows_tags(self):
        self.git("tag", "v1.0")
        self.git("push", "-q", "origin", "v1.0")

    def test_pre_commit_skips_gofmt_without_go(self):
        # 스테이징된 *.go가 없으면 gofmt를 찾지 않는다(gofmt 없는 PATH에서도 커밋된다)
        self.git("switch", "-q", "-c", "feat/issue-1")
        write(os.path.join(self.repo, "notes.md"), "x\n")
        self.git("add", "-A")
        env = dict(os.environ, PATH="/usr/bin:/bin", HOME=self.tmp)
        p = subprocess.run(["git", "commit", "-q", "-m", "feat: x"], cwd=self.repo, env=env,
                           capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_pre_commit_warns_web_without_biome(self):
        # web 소스를 스테이징했는데 biome가 없으면 막지 않고 경고만 한다
        self.git("switch", "-q", "-c", "feat/issue-1")
        write(os.path.join(self.repo, "web", "src", "a.ts"), "export const x=1\n")
        self.git("add", "-A")
        p = self.git("commit", "-q", "-m", "feat: web", check=False)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("biome", p.stderr)

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_pre_commit_runs_gofmt_on_go(self):
        self.git("switch", "-q", "-c", "feat/issue-1")
        bad = os.path.join(self.repo, "bad.go")
        write(bad, BAD_GO)
        self.git("add", "bad.go")
        p = self.assertRejected("commit", "-q", "-m", "feat: bad")
        self.assertIn("형식 위반", p.stderr)
        write(bad, GOOD_GO)
        self.git("add", "bad.go")
        self.git("commit", "-q", "-m", "feat: good")

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_pre_commit_rejects_unparseable_go(self):
        self.git("switch", "-q", "-c", "feat/issue-1")
        write(os.path.join(self.repo, "broken.go"), UNPARSEABLE_GO)
        self.git("add", "broken.go")
        p = self.assertRejected("commit", "-q", "-m", "feat: broken")
        self.assertIn("gofmt 오류", p.stderr)

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_pre_commit_checks_staged_content_not_work_tree(self):
        # 스테이징된 내용에 위반이 있으면 작업 트리를 고쳤어도 막는다
        self.git("switch", "-q", "-c", "feat/issue-1")
        path = os.path.join(self.repo, "sub dir", "a b.go")  # 공백 이름도 다룬다
        write(path, BAD_GO)
        self.git("add", "-A")
        write(path, GOOD_GO)
        p = self.assertRejected("commit", "-q", "-m", "feat: bad")
        self.assertIn("a b.go", p.stderr)
        self.git("add", "-A")
        self.git("commit", "-q", "-m", "feat: good")

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_pre_commit_ignores_dirty_work_tree(self):
        # 스테이징된 내용이 깨끗하면 작업 트리에만 있는 위반은 막지 않는다
        self.git("switch", "-q", "-c", "feat/issue-1")
        path = os.path.join(self.repo, "sub dir", "a b.go")
        write(path, GOOD_GO)
        self.git("add", "-A")
        write(path, BAD_GO)
        self.git("commit", "-q", "-m", "feat: good")
        self.assertEqual(self.git("status", "--porcelain").stdout.strip(), 'M "sub dir/a b.go"')


if __name__ == "__main__":
    unittest.main()
