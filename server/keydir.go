package server

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// KeyDirEnv 는 서버 키 파일(jwt.key, oauth.key)을 둘 디렉터리를 정하는 환경 변수다.
// 비어 있으면 실행 파일 옆 디렉터리(config.BaseDir())를 쓴다.
const KeyDirEnv = "ARTEX_KEY_DIR"

// keyDirPerm 은 새로 만드는 키 디렉터리의 권한이다. 키는 소유자만 읽어야 한다.
const keyDirPerm = 0o700

// ErrKeyDirInsideDataDir 는 키 디렉터리가 작업 공간(dataDir)과 같거나 그 안일 때 올린다.
// 작업 공간은 파일 관리자로 열람·내려받기가 되므로 키를 두면 서명·암호화 키가 새어 나간다.
var ErrKeyDirInsideDataDir = errors.New("key dir must be outside the data dir")

// keyFilenames 는 키 디렉터리로 옮겨야 하는 키 파일들이다.
var keyFilenames = []string{jwtKeyFilename, db.CredentialKeyFilename}

// PrepareKeyDir 는 서버 키 디렉터리를 정해 절대 경로로 돌려준다.
// configured 가 비어 있으면 baseDir 를 쓰고, 상대 경로면 절대 경로로 바꾼다. 디렉터리가 없으면 0700 으로 만든다.
// 심볼릭 링크를 푼 뒤 dataDir 와 같거나 그 안이면 ErrKeyDirInsideDataDir 를 올린다(dataDir 는 이미 있어야 한다).
// 키 디렉터리가 baseDir 와 다르고 키 파일이 아직 없으면, baseDir 에 있던 jwt.key·oauth.key 를 0600 으로
// 옮기고 원래 파일은 지운다. 옮기지 못하면 오류를 올린다. 새 키를 만들면 저장된 구독 토큰을 풀 수 없게 되므로
// 조용히 넘어가지 않는다.
func PrepareKeyDir(configured, baseDir, dataDir string) (string, error) {
	keyDir := configured
	if keyDir == "" {
		keyDir = baseDir
	}
	keyDir, err := filepath.Abs(keyDir)
	if err != nil {
		return "", fmt.Errorf("resolve key dir: %w", err)
	}
	realDataDir, err := evalAbs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	// 거절할 디렉터리를 작업 공간 안에 만들어 두지 않도록, 만들기 전에 있는 조상까지 풀어 비교한다.
	plannedKeyDir, err := evalExistingPrefix(keyDir)
	if err != nil {
		return "", fmt.Errorf("resolve key dir: %w", err)
	}
	if isWithinDir(plannedKeyDir, realDataDir) {
		return "", fmt.Errorf("%w: key dir %s, data dir %s", ErrKeyDirInsideDataDir, keyDir, dataDir)
	}
	if err := os.MkdirAll(keyDir, keyDirPerm); err != nil {
		return "", fmt.Errorf("create key dir: %w", err)
	}
	// 확인과 생성 사이에 경로가 바뀌었을 수 있으니 만든 뒤 한 번 더 본다.
	realKeyDir, err := filepath.EvalSymlinks(keyDir)
	if err != nil {
		return "", fmt.Errorf("resolve key dir: %w", err)
	}
	if isWithinDir(realKeyDir, realDataDir) {
		return "", fmt.Errorf("%w: key dir %s, data dir %s", ErrKeyDirInsideDataDir, keyDir, dataDir)
	}
	if err := migrateKeyFiles(baseDir, realKeyDir); err != nil {
		return "", err
	}
	return keyDir, nil
}

// migrateKeyFiles 는 legacyDir 에 남은 키 파일을 keyDir 로 옮긴다. 같은 디렉터리면 할 일이 없다.
// keyDir 에 이미 있는 키는 덮지 않는다. 그 키가 지금 쓰이는 키이기 때문이다.
func migrateKeyFiles(legacyDir, keyDir string) error {
	realLegacyDir, err := evalAbs(legacyDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve legacy key dir: %w", err)
	}
	if realLegacyDir == keyDir {
		return nil
	}
	for _, name := range keyFilenames {
		if err := migrateKeyFile(filepath.Join(realLegacyDir, name), filepath.Join(keyDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func migrateKeyFile(legacyPath, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat key file %s: %w", path, err)
	}
	data, err := os.ReadFile(legacyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy key file %s: %w", legacyPath, err)
	}
	if err := writeFileExclusive(path, data); err != nil {
		return fmt.Errorf("migrate key file to %s: %w", path, err)
	}
	if err := os.Remove(legacyPath); err != nil {
		// 새 위치에 이미 썼으므로 서버는 그 키로 돈다. 옛 파일이 남는 것은 서버를 멈출 이유가 아니라서 기록만 한다.
		log.Printf("[auth] 키 파일 %s 를 %s 로 옮겼으나 원래 파일을 지우지 못했다: %v", legacyPath, path, err)
		return nil
	}
	log.Printf("[auth] 키 파일을 %s 에서 %s 로 옮겼다", legacyPath, path)
	return nil
}

// writeFileExclusive 는 같은 디렉터리의 임시 파일에 0600 으로 다 쓴 뒤 link 로 path 에 붙인다.
// link 는 path 가 있으면 실패하므로 쓰는 도중 실패하거나 다른 프로세스가 먼저 만들어도 기존 키를 덮지 않는다.
func writeFileExclusive(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// link 성공 여부와 무관하게 임시 이름은 필요 없다. 지우지 못해도 숨김 임시 파일 하나가 남을 뿐이다.
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close() // 이미 실패한 쓰기라 Close 오류는 더할 정보가 없다.
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // 위와 같다.
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // 위와 같다.
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmpPath, path)
}

// evalAbs 는 dir 를 절대 경로로 바꾸고 심볼릭 링크를 푼다. dir 가 없으면 fs.ErrNotExist 를 잇는 오류를 올린다.
func evalAbs(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// evalExistingPrefix 는 절대 경로 path 에서 실제로 있는 가장 깊은 조상의 심볼릭 링크를 풀고 나머지를 붙인다.
// 아직 없는 디렉터리가 결국 어디에 만들어질지 알기 위해 쓴다.
func evalExistingPrefix(path string) (string, error) {
	var missing []string
	for dir := path; ; dir = filepath.Dir(dir) {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(append([]string{real}, missing...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(dir) == dir {
			return "", err
		}
		missing = append([]string{filepath.Base(dir)}, missing...)
	}
}

// isWithinDir 는 path 가 root 와 같거나 그 아래인지 본다. 두 경로 모두 심볼릭 링크를 푼 절대 경로여야 한다.
func isWithinDir(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
