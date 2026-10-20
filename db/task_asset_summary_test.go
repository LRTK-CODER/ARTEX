package db

import (
	"strings"
	"testing"
)

// TestTaskAssetLinkSummariesMatchSchema 는 Go 상수의 연결 출처 설명이 schema.sql의 트리거·이관 SQL과
// 글자가 같음을 확인한다. 어긋나면 옛 문구를 바꾸는 마이그레이션(#170)이 새 설치와 다른 값을 남긴다.
func TestTaskAssetLinkSummariesMatchSchema(t *testing.T) {
	tests := []struct {
		name    string
		snippet string
	}{
		{"auto", "'system', '" + TaskAssetLinkSummaryAuto + "'"},
		{"legacy", "'legacy', '" + TaskAssetLinkSummaryLegacy + "'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(schemaSQL, tc.snippet) {
				t.Errorf("schema.sql에 %q가 없다", tc.snippet)
			}
		})
	}
}
