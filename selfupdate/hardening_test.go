package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSmokeTestPassesOnlyAllowlistedEnv 는 스모크 테스트가 내려받은 바이너리에 서버의 비밀 환경 변수를
// 넘기지 않고, 실행에 필요한 변수와 스모크 테스트 표시만 넘기는지 본다.
func TestSmokeTestPassesOnlyAllowlistedEnv(t *testing.T) {
	requireUnix(t)
	dir := t.TempDir()
	envOut := filepath.Join(dir, "env.txt")
	bin := filepath.Join(dir, "artex.new")
	script := "#!/bin/sh\nenv > '" + envOut + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fake-secret")
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:fake-password@db/artex")
	t.Setenv("HOME", dir)

	if err := smokeTest(bin); err != nil {
		t.Fatalf("smokeTest: %v", err)
	}

	got := readAll(t, envOut)
	for _, secret := range []string{"sk-ant-fake-secret", "fake-password", "ANTHROPIC_API_KEY", "ARTEX_PG_DSN"} {
		if strings.Contains(got, secret) {
			t.Errorf("자식 환경에 %q이(가) 있으면 안 됨", secret) // 실제 환경 변수가 로그에 남지 않게 전체를 찍지 않는다
		}
	}
	for _, want := range []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, smokeEnv + "=1"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("자식 환경에 %q이(가) 있어야 함", want)
		}
	}
}

// TestWriteMarkerIsPrivateAndDoesNotFollowSymlink 는 업데이트 마커가 소유자 전용(0600)으로 만들어지고,
// 그 자리에 미리 둔 심볼릭 링크를 따라가 다른 파일을 덮어쓰지 않는지 본다.
func TestWriteMarkerIsPrivateAndDoesNotFollowSymlink(t *testing.T) {
	requireUnix(t)
	tests := []struct {
		name    string
		prepare func(t *testing.T, path, victim string)
	}{
		{name: "새 파일", prepare: func(*testing.T, string, string) {}},
		{name: "기존 0644 파일", prepare: func(t *testing.T, path, _ string) {
			if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "미리 둔 심볼릭 링크", prepare: func(t *testing.T, path, victim string) {
			if err := os.Symlink(victim, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			victim := filepath.Join(p.Dir, "victim")
			if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.prepare(t, p.Marker, victim)

			if err := writeMarker(p.Marker, marker{From: "1.0.0", To: "1.0.1"}); err != nil {
				t.Fatalf("writeMarker: %v", err)
			}

			assertPrivateRegularFile(t, p.Marker)
			if got := readAll(t, victim); got != "keep" {
				t.Errorf("링크 대상 파일이 바뀌면 안 됨: %q", got)
			}
			if m, ok := readMarker(p.Marker); !ok || m.To != "1.0.1" {
				t.Errorf("마커를 다시 읽지 못함: %+v, %v", m, ok)
			}
		})
	}
}

// TestCheckURLRejectsUnusedRawHost 는 다운로드 경로가 쓰지 않는 raw.githubusercontent.com을 허용하지 않는지 본다.
func TestCheckURLRejectsUnusedRawHost(t *testing.T) {
	if err := checkURL(mustParse(t, "https://raw.githubusercontent.com/LRTK-CODER/ARTEX/main/x")); err == nil {
		t.Error("raw.githubusercontent.com은 허용 목록에 없어야 함")
	}
}

func assertPrivateRegularFile(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s 확인: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("%s은(는) 일반 파일이어야 하는데 %v임", path, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s 권한 = %o, want 600", path, perm)
	}
}
