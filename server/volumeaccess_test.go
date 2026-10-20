package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 예전 root 이미지가 만든 ./data·./keys를 붙이면 서버가 읽거나 쓰지 못한다. 시작 전에 막힌 디렉터리를 알아내야
// cmd/artex가 소유권 안내를 찍을 수 있다.
func TestCheckDirAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root는 디렉터리 권한을 무시하므로 권한 거부를 만들 수 없다")
	}
	cases := []struct {
		name     string
		perm     fs.FileMode // 0이면 디렉터리를 만들지 않는다
		wantPerm bool        // fs.ErrPermission을 이어야 하는가
	}{
		{name: "쓸 수 있는 디렉터리", perm: 0o700},
		{name: "없는 디렉터리", perm: 0},
		{name: "읽기만 되는 디렉터리(root 소유 0755와 같음)", perm: 0o500, wantPerm: true},
		{name: "목록을 볼 수 없는 디렉터리(root 소유 0700과 같음)", perm: 0o300, wantPerm: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "vol")
			if tc.perm != 0 {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, tc.perm); err != nil {
					t.Fatal(err)
				}
				// t.TempDir 정리가 막히지 않게 권한을 되돌린다.
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			}
			// 앞의 정상 디렉터리와 빈 문자열은 건너뛰고, 막힌 디렉터리를 찾아야 한다.
			err := CheckDirAccess(t.TempDir(), "", dir)
			if got := errors.Is(err, fs.ErrPermission); got != tc.wantPerm {
				t.Fatalf("CheckDirAccess 오류 = %v, 권한 오류 기대 %v", err, tc.wantPerm)
			}
			if !tc.wantPerm && err != nil {
				t.Fatalf("CheckDirAccess 오류 = %v, 기대값 nil", err)
			}
			if tc.wantPerm && !strings.Contains(err.Error(), dir) {
				t.Errorf("오류에 막힌 디렉터리 경로가 없다: %v", err)
			}
			if tc.perm == 0o700 {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Errorf("확인용 파일이 남았다: %v", entries)
				}
			}
		})
	}
}

func TestVolumeOwnershipHintNamesFix(t *testing.T) {
	hint := VolumeOwnershipHint(10001)
	for _, want := range []string{"Docker 볼륨 소유권", "./update.sh", "chown -R 10001:10001 data keys"} {
		if !strings.Contains(hint, want) {
			t.Errorf("안내에 %q가 없다: %s", want, hint)
		}
	}
}
