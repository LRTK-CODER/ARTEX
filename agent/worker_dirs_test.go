package agent

import (
	"path/filepath"
	"testing"
)

// TestIsTaskWorkspaceName 은 sharedRunDirs가 만드는 작업 디렉터리 이름만 받고, 같은 tasks/<tid> 아래의 다른 이름
// (첨부, 큰 출력, 끈 상태가 남긴 디렉터리, runDirPath가 만들지 않는 모양)은 거르는지 본다. server는 이 결과로만
// 에이전트 그룹 쓰기를 주므로, 거르지 못하면 재시작 때 uploads 같은 서버 디렉터리에 쓰기가 생긴다.
func TestIsTaskWorkspaceName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{name: filepath.Base(sharedRunDirs("/data", 7, 0).work), want: true},
		{name: filepath.Base(sharedRunDirs("/data", 7, 1).work), want: true},
		{name: filepath.Base(sharedRunDirs("/data", 7, 42).work), want: true},
	}
	for _, name := range []string{"uploads", ToolOutputDirName, "scans", "i", "i0", "i03", "i-1", "i1x", "1", "work2", "Work"} {
		cases = append(cases, struct {
			name string
			want bool
		}{name: name})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTaskWorkspaceName(tc.name); got != tc.want {
				t.Errorf("IsTaskWorkspaceName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
