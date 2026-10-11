"""PostToolUse hook(format_code.py) 테스트.

임시 git 저장소에서 hook을 실제 프로세스로 실행해 Go 형식 수정·구문 오류 보고·건너뛰기를
확인한다. gofmt가 없으면 Go 테스트는 건너뛴다. web(biome) 경로는 저장소에 로컬 biome이 없으면
건너뛰므로, 여기서는 "biome 없이 건너뛴다"만 확인한다.
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
HOOK = os.path.join(HOOKS_DIR, "format_code.py")

GO_UNFORMATTED = "package app\nfunc  F(){}\n"
GO_FORMATTED = "package app\n\nfunc F() {}\n"
GO_UNPARSEABLE = "package app\nfunc F( {\n"
TS_UNFORMATTED = "export const x=1\n"


def find_gofmt():
    return shutil.which("gofmt")


class FormatCodeTest(unittest.TestCase):
    def setUp(self):
        self.repo = os.path.realpath(tempfile.mkdtemp(prefix="format-code-test-"))
        subprocess.run(["git", "init", "-q", self.repo], check=True)

    def tearDown(self):
        shutil.rmtree(self.repo, ignore_errors=True)

    def write(self, rel, text):
        path = os.path.join(self.repo, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write(text)
        return path

    def read(self, path):
        with open(path, encoding="utf-8") as f:
            return f.read()

    def run_hook(self, file_path, tool="Write"):
        event = {"tool_name": tool, "tool_input": {"file_path": file_path}, "cwd": self.repo}
        return subprocess.run([sys.executable, HOOK], input=json.dumps(event),
                              capture_output=True, text=True, timeout=180)

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_formats_go(self):
        path = self.write("app.go", GO_UNFORMATTED)
        p = self.run_hook(path, tool="Edit")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.read(path), GO_FORMATTED)

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_relative_path_uses_cwd(self):
        self.write("app.go", GO_UNFORMATTED)
        p = self.run_hook("app.go")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.read(os.path.join(self.repo, "app.go")), GO_FORMATTED)

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_reports_unparseable_go_with_exit_2(self):
        path = self.write("broken.go", GO_UNPARSEABLE)
        p = self.run_hook(path)
        self.assertEqual(p.returncode, 2)
        self.assertIn("gofmt", p.stderr)

    def test_skips_non_target(self):
        path = self.write("notes.md", "x  \n")
        p = self.run_hook(path)
        self.assertEqual((p.returncode, p.stderr), (0, ""))
        self.assertEqual(self.read(path), "x  \n")

    @unittest.skipUnless(find_gofmt(), "gofmt가 없다")
    def test_skips_claude_dir(self):
        # .claude/ 아래 .go(가정)는 대상이 아니다
        path = self.write(os.path.join(".claude", "hooks", "tool.go"), GO_UNFORMATTED)
        p = self.run_hook(path)
        self.assertEqual((p.returncode, p.stderr), (0, ""))
        self.assertEqual(self.read(path), GO_UNFORMATTED)

    def test_skips_web_without_biome(self):
        # web 소스라도 로컬 biome이 없으면 건너뛴다(파일을 바꾸지 않는다)
        path = self.write(os.path.join("web", "src", "a.ts"), TS_UNFORMATTED)
        p = self.run_hook(path)
        self.assertEqual((p.returncode, p.stderr), (0, ""))
        self.assertEqual(self.read(path), TS_UNFORMATTED)

    def test_reports_invalid_utf8_output(self):
        # gofmt 오류 출력에 바이트 단위로 잘린 "한"(ed 95 9c의 앞 두 바이트)이 섞여도
        # 디코딩에서 멈추지 않고 대체 문자로 보여 준다(#143)
        bin_dir = os.path.join(self.repo, "fake-bin")
        os.makedirs(bin_dir)
        gofmt = os.path.join(bin_dir, "gofmt")
        with open(gofmt, "w", encoding="utf-8") as f:
            f.write("#!/bin/sh\nprintf 'broken.go: \\355\\225\\n' >&2\nexit 1\n")
        os.chmod(gofmt, 0o755)
        path = self.write("broken.go", GO_UNPARSEABLE)
        event = {"tool_name": "Write", "tool_input": {"file_path": path}, "cwd": self.repo}
        env = dict(os.environ, PATH=bin_dir + os.pathsep + os.environ.get("PATH", ""))
        p = subprocess.run([sys.executable, HOOK], input=json.dumps(event), env=env,
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(p.returncode, 2, p.stderr)
        self.assertIn("broken.go: \ufffd", p.stderr)

    def test_bad_input_does_not_block(self):
        for text in ("not json", "[]", json.dumps({"tool_input": {}})):
            with self.subTest(text=text):
                p = subprocess.run([sys.executable, HOOK], input=text,
                                   capture_output=True, text=True)
                self.assertEqual(p.returncode, 0)


if __name__ == "__main__":
    unittest.main()
