package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// CheckDirAccess 는 서버가 dirs의 각 디렉터리를 읽고(목록) 그 안에 파일을 만들 수 있는지 시작 전에 본다.
// 없는 디렉터리와 빈 문자열은 건너뛴다(뒤에서 만든다). 처음 막힌 디렉터리의 오류를 올리며, 권한 문제면
// fs.ErrPermission을 잇는다. 확인하려고 만든 임시 파일은 바로 지운다.
//
// 예전 root 이미지가 만든 ./data·./keys를 그대로 붙이면, 이 검사가 없을 때 서버는 키 파일 lstat이나 저장소 열기에서
// permission denied로 끝나고 감시 스크립트가 같은 오류로 다시 띄우기만 한다. 원인과 고칠 방법을 한 번에 알리려고 먼저 본다.
func CheckDirAccess(dirs ...string) error {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := checkDirAccess(dir); err != nil {
			return err
		}
	}
	return nil
}

func checkDirAccess(dir string) error {
	if _, err := os.ReadDir(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("디렉터리 %s를 읽을 수 없습니다: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".artex-access-*")
	if err != nil {
		return fmt.Errorf("디렉터리 %s에 쓸 수 없습니다: %w", dir, err)
	}
	name := probe.Name()
	closeErr := probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("확인용 파일 %s를 지울 수 없습니다: %w", name, err)
	}
	if closeErr != nil {
		return fmt.Errorf("확인용 파일 %s를 닫을 수 없습니다: %w", name, closeErr)
	}
	return nil
}

// VolumeOwnershipHint 는 컨테이너에서 CheckDirAccess가 권한 오류를 냈을 때 로그에 남길 안내 한 줄이다.
// uid는 지금 프로세스의 사용자다(이미지 기본은 10001). 키 내용이나 경로 밖의 정보는 싣지 않는다.
func VolumeOwnershipHint(uid int) string {
	return fmt.Sprintf("[config] 컨테이너 사용자(uid %d)가 ./data·./keys를 읽거나 쓰지 못합니다. "+
		"호스트의 compose 디렉터리에서 ./update.sh 를 실행하거나, README 'Docker 볼륨 소유권' 절대로 "+
		"sudo chown -R %d:%d data keys 를 실행한 뒤 다시 시작하세요", uid, uid, uid)
}
