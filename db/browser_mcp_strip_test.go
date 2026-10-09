package db

import (
	"slices"
	"testing"
)

func TestStripBrowserProxyArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"프록시 없음", []string{"@playwright/mcp", "--headless"}, []string{"@playwright/mcp", "--headless"}},
		{"값을 따로 준 형식", []string{"a", "--proxy-server", "h:1", "--proxy-bypass", "localhost", "b"}, []string{"a", "b"}},
		{"= 형식", []string{"a", "--proxy-server=h:1", "--proxy-bypass=x", "b"}, []string{"a", "b"}},
		{"다른 인자는 남긴다", []string{"--proxy-serverx", "--browser", "chromium"}, []string{"--proxy-serverx", "--browser", "chromium"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := slices.Clone(tc.in)
			if got := StripBrowserProxyArgs(in); !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if !slices.Equal(in, tc.in) {
				t.Fatalf("입력이 바뀌었다: %q", in)
			}
		})
	}
}
