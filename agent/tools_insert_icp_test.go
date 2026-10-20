package agent

import (
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// ICP 등록 번호는 한국 사용에서 쓰지 않으므로 자산·범위 도구의 설명과 스키마에 싣지 않는다(#192).
// 핸들러는 호환을 위해 icp 인자를 계속 받는다.
func TestAssetScopeToolsDoNotAdvertiseICP(t *testing.T) {
	ts := NewToolSet(nil, "")
	cases := []struct {
		name string
		tool actool.CoreTool
	}{
		{name: "insert_assets", tool: ts.insertAssets()},
		{name: "add_company_scope", tool: ts.addCompanyScope()},
		{name: "add_task_scope", tool: ts.addTaskScope()},
		{name: "list_assets", tool: ts.listAssets()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema, err := json.Marshal(tc.tool.InputSchema())
			if err != nil {
				t.Fatal(err)
			}
			for label, text := range map[string]string{"description": tc.tool.Description(), "schema": string(schema)} {
				if strings.Contains(strings.ToLower(text), "icp") {
					t.Errorf("%s %s mentions ICP: %s", tc.name, label, text)
				}
			}
		})
	}
}
