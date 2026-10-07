"""pr_gate.py 테스트.

재채점 동작은 작은 Go 모듈 픽스처로 실제 `go test`를 돌려 확인한다(go가 없으면 건너뛴다).
분류·집계·환경 등 순수 로직은 go 없이 확인한다.
"""
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True

TESTS_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(TESTS_DIR)))
sys.path.insert(0, os.path.join(REPO_ROOT, ".claude", "scripts"))
import pr_gate  # noqa: E402

GO_MOD = "module example.com/calc\n\ngo 1.21\n"
CALC = "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
CALC_TEST = ('package calc\n\nimport "testing"\n\n'
             'func TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal("wrong")\n\t}\n}\n')
# gofmt가 고칠 파일(들여쓰기가 탭이 아니다).
UNFORMATTED = "package calc\n\nfunc Sub(a, b int) int {\n    return a - b\n}\n"
BASE_FILES = {"go.mod": GO_MOD, "calc.go": CALC, "calc_test.go": CALC_TEST}

# 검사는 go test만 돌려 빠르게 둔다. 전체 기본 프로파일(gofmt·vet·build·test)의 모양은 따로 본다.
GO_PROFILE = pr_gate.Profile(setup=[], checks=[("go test", ["go", "test", "./..."])])


def have_go():
    return shutil.which("go")


def git(repo, *args):
    p = subprocess.run(["git", "-C", repo] + list(args), capture_output=True, text=True)
    if p.returncode != 0:
        raise AssertionError("git {} 실패: {}".format(" ".join(args), p.stderr))
    return p.stdout


def first_line(text, prefix):
    for line in text.splitlines():
        if line.strip().startswith(prefix):
            return line
    return None


class GateRepo:
    def __init__(self, base):
        self.repo = os.path.join(base, "repo")
        os.makedirs(self.repo)
        git(self.repo, "init", "-q", "-b", "main")
        git(self.repo, "config", "user.email", "t@example.com")
        git(self.repo, "config", "user.name", "t")
        self._write(BASE_FILES)
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", "init")
        git(self.repo, "remote", "add", "origin", self.repo)  # 자기 자신을 원격으로 쓴다

    def _write(self, files):
        for name, content in files.items():
            path = os.path.join(self.repo, name)
            os.makedirs(os.path.dirname(path) or self.repo, exist_ok=True)
            with open(path, "w", encoding="utf-8") as f:
                f.write(content)

    def commit_main(self, message="more"):
        git(self.repo, "commit", "-q", "--allow-empty", "-m", message)

    def open_pr(self, number, files):
        git(self.repo, "switch", "-q", "-c", "pr-{}".format(number), "main")
        for name, content in files.items():
            if content is None:
                git(self.repo, "rm", "-q", name)
            else:
                self._write({name: content})
                git(self.repo, "add", name)
        git(self.repo, "commit", "-q", "-m", "pr")
        sha = git(self.repo, "rev-parse", "HEAD").strip()
        git(self.repo, "update-ref", "refs/pull/{}/head".format(number), sha)
        git(self.repo, "switch", "-q", "main")


@unittest.skipUnless(have_go(), "go가 없다")
class GateTest(unittest.TestCase):
    def setUp(self):
        self.base = os.path.realpath(tempfile.mkdtemp(prefix="pr-gate-test-"))
        self.repo = GateRepo(self.base)

    def tearDown(self):
        shutil.rmtree(self.base, ignore_errors=True)

    def gate(self, number):
        return pr_gate.gate(self.repo.repo, number, profile=GO_PROFILE, workdir_base=self.base)

    def test_normal_pr_passes(self):
        extra = CALC_TEST + ('\nfunc TestAdd2(t *testing.T) {\n\tif Add(1, 1) != 2 {\n'
                             '\t\tt.Fatal("wrong")\n\t}\n}\n')
        self.repo.open_pr(1, {"calc_test.go": extra})
        code, text = self.gate(1)
        self.assertEqual(code, 0, text)
        self.assertIn("[1] main 기준 재채점", text)
        self.assertIn(": 통과", first_line(text, "[1]"))
        self.assertIn("[2] PR 트리 그대로: 통과", text)
        self.assertIn("테스트 함수(Go): 1 → 2 (+1)", text)

    def test_weakened_impl_fails_main_based_rescore(self):
        wrong_code = "package calc\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"
        weak_test = ('package calc\n\nimport "testing"\n\n'
                     'func TestAdd(t *testing.T) {\n\tif Add(2, 3) != -1 {\n'
                     '\t\tt.Fatal("wrong")\n\t}\n}\n')
        self.repo.open_pr(2, {"calc.go": wrong_code, "calc_test.go": weak_test})
        code, text = self.gate(2)
        self.assertEqual(code, 1, text)
        self.assertIn("[2] PR 트리 그대로: 통과", text)
        self.assertIn("M calc_test.go", text)
        self.assertIn("main 기준 재채점 실패(go test)", text)

    def test_deleted_test_is_listed(self):
        self.repo.open_pr(3, {"calc_test.go": None})
        code, text = self.gate(3)
        self.assertIn("D calc_test.go", text)
        self.assertIn("테스트 함수(Go): 1 → 0 (-1)  표시", text)

    def test_added_skip_is_flagged(self):
        skipped = ('package calc\n\nimport "testing"\n\n'
                   'func TestAdd(t *testing.T) {\n\tt.Skip("#1 wip")\n\tif Add(2, 3) != 5 {\n'
                   '\t\tt.Fatal("wrong")\n\t}\n}\n')
        self.repo.open_pr(4, {"calc_test.go": skipped})
        code, text = self.gate(4)
        self.assertIn("t.Skip 호출: 0 → 1 (+1)  표시", text)
        self.assertIn("M calc_test.go", text)

    def test_config_change_is_listed(self):
        self.repo.open_pr(5, {"go.mod": GO_MOD + "\n// pr 주석\n"})
        code, text = self.gate(5)
        self.assertIn("M go.mod", text)

    def test_behind_main_is_reported(self):
        self.repo.open_pr(6, {"calc.go": CALC.replace("a + b", "b + a")})
        self.repo.commit_main()
        code, text = self.gate(6)
        self.assertIn("PR이 main보다 커밋 1개 뒤처져 있다", text)

    def test_unknown_pr_raises(self):
        with self.assertRaises(pr_gate.GateError):
            self.gate(999)

    def gofmt_gate(self, number):
        gofmt_only = pr_gate.Profile(
            setup=[], checks=[c for c in pr_gate.DEFAULT_PROFILE.checks if c[0] == "gofmt"])
        return pr_gate.gate(self.repo.repo, number, profile=gofmt_only, workdir_base=self.base)

    def test_gofmt_ignores_unchanged_files(self):
        # main에 이미 형식이 어긋난 파일이 있어도 PR이 건드리지 않았으면 통과다
        git(self.repo.repo, "switch", "-q", "main")
        self.repo._write({"messy.go": UNFORMATTED})
        git(self.repo.repo, "add", "messy.go")
        git(self.repo.repo, "commit", "-q", "-m", "messy")
        self.repo.open_pr(9, {"calc.go": CALC.replace("a + b", "b + a")})
        code, text = self.gofmt_gate(9)
        self.assertEqual(code, 0, text)
        self.assertIn("[2] PR 트리 그대로: 통과", text)

    def test_gofmt_passes_without_changed_go_files(self):
        self.repo.open_pr(10, {"README.md": "문서만 바꾼다\n"})
        code, text = self.gofmt_gate(10)
        self.assertEqual(code, 0, text)

    def test_gofmt_fails_on_changed_unformatted_file(self):
        self.repo.open_pr(11, {"messy.go": UNFORMATTED})
        code, text = self.gofmt_gate(11)
        self.assertEqual(code, 1, text)
        self.assertIn("main 기준 재채점 실패(gofmt)", text)
        self.assertIn("messy.go", text)

    def test_main_based_tree_uses_main_files(self):
        # main의 calc_test.go가 PR 것을 덮는지 직접 본다
        self.repo.open_pr(8, {"calc_test.go": "package calc\n// PR 전용\n"})
        main = git(self.repo.repo, "rev-parse", "main").strip()
        head = git(self.repo.repo, "rev-parse", "refs/pull/8/head").strip()
        dest = os.path.join(self.base, "main-based")
        pr_gate.build_main_based_tree(self.repo.repo, head=head, main=main, dest=dest)
        with open(os.path.join(dest, "calc_test.go"), encoding="utf-8") as f:
            self.assertEqual(f.read(), CALC_TEST)  # main 것이다


class ClassifyTest(unittest.TestCase):
    def test_categories(self):
        name_status = "\n".join([
            "M\tcalc_test.go",
            "D\tserver/handler_test.go",
            "M\tweb/src/lib/x.test.mjs",
            "M\tgo.mod",
            "M\t.github/workflows/release.yml",
            "M\t.githooks/pre-commit",
            "A\tnew_test.go",            # 새 테스트는 기존 테스트가 아니다
            "M\tcalc.go",               # 테스트가 아닌 코드는 어느 분류에도 없다
        ])
        out = pr_gate.classify(name_status)
        self.assertEqual(out[pr_gate.CATEGORY_TESTS],
                         ["M calc_test.go", "D server/handler_test.go", "M web/src/lib/x.test.mjs"])
        self.assertEqual(out[pr_gate.CATEGORY_CONFIG],
                         ["M go.mod", "M .github/workflows/release.yml", "M .githooks/pre-commit"])


class CompareCountsTest(unittest.TestCase):
    def test_flags_only_reductions(self):
        before = {"go_tests": 5, "skip": 1, "short": 1, "web_tests": 2}
        after = {"go_tests": 4, "skip": 2, "short": 1, "web_tests": 1}
        rows = {name: flagged for name, _, _, flagged in pr_gate.compare_counts(before, after)}
        self.assertTrue(rows["테스트 함수(Go)"])   # 5 → 4
        self.assertTrue(rows["t.Skip 호출"])        # 1 → 2
        self.assertFalse(rows["testing.Short() 참조"])  # 변화 없음
        self.assertTrue(rows["web 테스트 파일"])    # 2 → 1


class CountTestsTest(unittest.TestCase):
    def test_counts_funcs_skips_and_web(self):
        tree = tempfile.mkdtemp(prefix="count-test-")
        try:
            os.makedirs(os.path.join(tree, "sub"))
            with open(os.path.join(tree, "a_test.go"), "w", encoding="utf-8") as f:
                f.write("package a\nimport \"testing\"\n"
                        "func TestOne(t *testing.T) { t.Skip(\"x\") }\n"
                        "func BenchmarkTwo(b *testing.B) {}\n"
                        "func helper() {}\n")
            with open(os.path.join(tree, "sub", "b_test.go"), "w", encoding="utf-8") as f:
                f.write("package b\nimport \"testing\"\n"
                        "func TestThree(t *testing.T) { if testing.Short() { t.Skip(\"s\") } }\n")
            with open(os.path.join(tree, "x.test.mjs"), "w", encoding="utf-8") as f:
                f.write("test('x', () => {})\n")
            counts = pr_gate.count_tests(tree)
            self.assertEqual(counts["go_tests"], 3)   # TestOne, BenchmarkTwo, TestThree
            self.assertEqual(counts["skip"], 2)       # t.Skip 두 번
            self.assertEqual(counts["short"], 1)      # testing.Short() 한 번
            self.assertEqual(counts["web_tests"], 1)
        finally:
            shutil.rmtree(tree, ignore_errors=True)


class OverlayPathTest(unittest.TestCase):
    def test_predicates(self):
        for p in ("calc_test.go", "server/x_test.go", "web/a.test.ts", "web/a.spec.tsx",
                  "pkg/testdata/sample.json"):
            self.assertTrue(pr_gate.is_test_file(p), p)
        for p in ("calc.go", "web/a.ts", "README.md"):
            self.assertFalse(pr_gate.is_test_file(p), p)
        for p in ("go.mod", "go.sum", "web/package.json", "web/biome.json", "web/tsconfig.json"):
            self.assertTrue(pr_gate.is_config_file(p), p)
        self.assertFalse(pr_gate.is_config_file("calc.go"))


class CheckEnvTest(unittest.TestCase):
    def test_drops_caller_go_options(self):
        os.environ["GOFLAGS"] = "-mod=vendor"
        os.environ["GOWORK"] = "/somewhere/go.work"
        try:
            env = pr_gate.check_env()
            self.assertNotIn("-mod=vendor", env.get("GOFLAGS", ""))
            self.assertEqual(env.get("GOWORK"), "off")
        finally:
            del os.environ["GOFLAGS"]
            del os.environ["GOWORK"]


class DefaultProfileTest(unittest.TestCase):
    def test_runs_go_test(self):
        names = [name for name, _ in pr_gate.DEFAULT_PROFILE.checks]
        self.assertIn("go test", names)
        self.assertIn("gofmt", names)


if __name__ == "__main__":
    unittest.main()
