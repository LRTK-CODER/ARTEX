package server

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// keyDirLayout 은 실행 파일 디렉터리(base), 그 아래 작업 공간(data), 밖의 디렉터리(outside)를 만든다.
func keyDirLayout(t *testing.T) (base, data, outside string) {
	t.Helper()
	root := t.TempDir()
	base = filepath.Join(root, "app")
	data = filepath.Join(base, "data")
	outside = filepath.Join(root, "outside")
	for _, d := range []string{data, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return base, data, outside
}

func TestPrepareKeyDirRejectsDataDir(t *testing.T) {
	base, data, outside := keyDirLayout(t)
	linkToData := filepath.Join(outside, "link-to-data")
	if err := os.Symlink(data, linkToData); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		configured string
	}{
		{"작업 공간 자체", data},
		{"작업 공간 아래", filepath.Join(data, "keys")},
		{"작업 공간을 가리키는 심볼릭 링크", linkToData},
		{"심볼릭 링크를 거친 작업 공간 아래", filepath.Join(linkToData, "keys")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PrepareKeyDir(tc.configured, base, data)
			if !errors.Is(err, ErrKeyDirInsideDataDir) {
				t.Fatalf("PrepareKeyDir(%q) err = %v, want ErrKeyDirInsideDataDir", tc.configured, err)
			}
		})
	}
}

func TestPrepareKeyDirAccepts(t *testing.T) {
	base, data, outside := keyDirLayout(t)
	cases := []struct {
		name       string
		configured string
		want       string
	}{
		{"비어 있으면 baseDir", "", base},
		{"작업 공간 밖", filepath.Join(outside, "keys"), filepath.Join(outside, "keys")},
		{"이름이 작업 공간으로 시작하는 형제 디렉터리", data + "-keys", data + "-keys"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PrepareKeyDir(tc.configured, base, data)
			if err != nil {
				t.Fatalf("PrepareKeyDir(%q): %v", tc.configured, err)
			}
			if got != tc.want {
				t.Fatalf("PrepareKeyDir(%q) = %q, want %q", tc.configured, got, tc.want)
			}
		})
	}
}

func TestPrepareKeyDirCreatesAbsoluteOwnerOnlyDir(t *testing.T) {
	base, data, outside := keyDirLayout(t)
	t.Chdir(outside)
	got, err := PrepareKeyDir("keys", base, data)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("key dir %q is not absolute", got)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("key dir perm = %o, want 700", perm)
	}
}

// TestPrepareKeyDirMigratesLegacyKeys 는 예전 위치(baseDir)의 두 키가 새 키 디렉터리로 옮겨지고,
// 옮긴 뒤 불러온 키가 예전 키와 같은지 본다. 같아야 웹 세션과 저장된 구독 토큰이 유지된다.
func TestPrepareKeyDirMigratesLegacyKeys(t *testing.T) {
	base, data, outside := keyDirLayout(t)
	oldJWT, err := loadOrCreateJWTKey(base, data)
	if err != nil {
		t.Fatal(err)
	}
	oldCred, err := db.LoadOrCreateCredentialKey(base)
	if err != nil {
		t.Fatal(err)
	}

	keyDir, err := PrepareKeyDir(filepath.Join(outside, "keys"), base, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{jwtKeyFilename, db.CredentialKeyFilename} {
		if _, err := os.Stat(filepath.Join(base, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("legacy %s still present (stat err = %v)", name, err)
		}
		info, err := os.Stat(filepath.Join(keyDir, name))
		if err != nil {
			t.Fatalf("migrated %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perm = %o, want 600", name, perm)
		}
	}

	newJWT, err := loadOrCreateJWTKey(keyDir, data)
	if err != nil {
		t.Fatal(err)
	}
	newCred, err := db.LoadOrCreateCredentialKey(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldJWT, newJWT) {
		t.Error("jwt key changed after migration")
	}
	if !bytes.Equal(oldCred, newCred) {
		t.Error("credential key changed after migration")
	}
}

// TestPrepareKeyDirKeepsExistingKey 는 새 위치에 키가 이미 있으면 예전 위치의 키로 덮지 않는지 본다.
func TestPrepareKeyDirKeepsExistingKey(t *testing.T) {
	base, data, outside := keyDirLayout(t)
	keyDir := filepath.Join(outside, "keys")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	current := []byte("current-key")
	legacy := []byte("legacy-key")
	if err := os.WriteFile(filepath.Join(keyDir, jwtKeyFilename), current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, jwtKeyFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := PrepareKeyDir(keyDir, base, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(keyDir, jwtKeyFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, current) {
		t.Fatalf("key = %q, want %q", got, current)
	}
	if _, err := os.Stat(filepath.Join(base, jwtKeyFilename)); err != nil {
		t.Fatalf("legacy key should be left alone: %v", err)
	}
}

// TestPrepareKeyDirSameAsBaseDir 는 키 디렉터리가 baseDir 이면 키를 옮기거나 지우지 않는지 본다.
func TestPrepareKeyDirSameAsBaseDir(t *testing.T) {
	base, data, _ := keyDirLayout(t)
	want := []byte("same-dir-key")
	path := filepath.Join(base, jwtKeyFilename)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareKeyDir("", base, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("key = %q, want %q", got, want)
	}
}

// TestPrepareKeyDirMigrationFailure 는 키를 옮기지 못하면 새 키를 만들 여지를 주지 않고 오류를 올리는지 본다.
func TestPrepareKeyDirMigrationFailure(t *testing.T) {
	base, data, outside := keyDirLayout(t)
	if err := os.WriteFile(filepath.Join(base, jwtKeyFilename), []byte("legacy-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(outside, "keys")
	if err := os.MkdirAll(keyDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(keyDir, 0o700) }) // TempDir 정리가 지울 수 있게 되돌린다.
	if os.Geteuid() == 0 {
		t.Skip("root 는 쓰기 권한 없는 디렉터리에도 쓸 수 있어 실패를 만들 수 없다")
	}

	if _, err := PrepareKeyDir(keyDir, base, data); err == nil {
		t.Fatal("PrepareKeyDir succeeded, want migration error")
	}
	if _, err := os.Stat(filepath.Join(base, jwtKeyFilename)); err != nil {
		t.Fatalf("legacy key must survive a failed migration: %v", err)
	}
}
