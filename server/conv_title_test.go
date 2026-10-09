package server

import "testing"

func TestIsDefaultConvTitle(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  bool
	}{
		{"빈 제목", "", true},
		{"한국어 기본 제목", "새 대화", true},
		{"옛 중국어 기본 제목", "新对话", true},
		{"사용자가 정한 제목", "내가 정한 제목", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDefaultConvTitle(tc.title); got != tc.want {
				t.Errorf("isDefaultConvTitle(%q) = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}
