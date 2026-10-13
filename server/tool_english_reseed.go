package server

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
)

// toolDefault 는 시스템 도구 하나의 코드 기본 설명과 schema다.
type toolDefault struct {
	key    string
	desc   string
	schema json.RawMessage
}

// builtinToolDefaults 는 시작 시 tools 표에 씨앗으로 넣는 모든 시스템 도구의 코드 기본값을 모은다.
// 씨앗 경로(wireTools, seedOrchestrationTools, seedFindingRetester)와 같은 출처를 쓴다.
func (s *Server) builtinToolDefaults() []toolDefault {
	var defaults []toolDefault
	for _, sd := range agent.BuiltinToolSeeds() {
		schema, _ := json.Marshal(sd.Schema)
		defaults = append(defaults, toolDefault{key: sd.Key, desc: sd.Desc, schema: schema})
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	tools = append(tools, traffic.SeedToolMetas()...)
	tools = append(tools, s.findingRetestTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		defaults = append(defaults, toolDefault{key: t.Name(), desc: t.Description(), schema: schema})
	}
	return defaults
}

// toolsEnglishFlag 는 #145 마이그레이션을 한 번만 돌리는 설정 키다.
const toolsEnglishFlag = "tool_defaults_v1_english_descriptions"

// reseedToolsToEnglish 는 번역(#99) 뒤에도 기존 설치의 tools 표에 남은 중국어 설명과
// schema를 코드의 영어 기본값으로 바꾼다(한 번만). SeedTool은 첫 삽입만 하고,
// refreshBuiltinToolSchemas는 번역 전에 이미 플래그가 켜져 다시 돌지 않기 때문이다.
// 무조건 덮어쓰지 않는 이유: 사용자가 한국어·영어로 고친 설명을 지우지 않으려고,
// 옛 한자가 남은 쪽만 바꾼다.
func (s *Server) reseedToolsToEnglish() {
	if v, _, _ := s.m.pg.GetSetting(toolsEnglishFlag); v == "true" {
		return
	}
	if !refreshHanToolDefaults(s.m.pg, s.builtinToolDefaults()) {
		return // 실패한 행이 있으면 플래그를 켜지 않아 다음 시작 때 다시 시도한다
	}
	if err := s.m.pg.SetSetting(toolsEnglishFlag, "true"); err != nil {
		log.Printf("[tools] %s 설정 저장 실패: %v", toolsEnglishFlag, err)
	}
}

// hanRun 은 이어진 한자 덩어리 하나에 맞는다.
var hanRun = regexp.MustCompile(`[\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}]+`)

// hasLegacyHan 은 current에 code 기본값에 없는 한자 덩어리가 있는지 본다. 코드 기본값도
// 일부러 한자를 담을 수 있으므로(traffic_search의 중국어 검색 예시) 한자가 있다는 것만으로는
// 옛 중국어로 보지 않는다. 사용자가 고친 한국어·영어 값에는 한자가 없어 false다.
func hasLegacyHan(current, code string) bool {
	for _, run := range hanRun.FindAllString(current, -1) {
		if !strings.Contains(code, run) {
			return true
		}
	}
	return false
}

// refreshHanToolDefaults 는 defaults의 각 시스템 도구에서 옛 중국어가 남은 설명·schema만
// 코드 기본값으로 바꾼다. 나머지(사용자 수정값, 연결, 사용 여부)는 그대로 둔다. 바꾼 뒤에는
// 옛 한자가 없으므로 몇 번을 돌려도 결과가 같다. 모두 성공하면 true다.
func refreshHanToolDefaults(pg *db.DB, defaults []toolDefault) bool {
	isAllOK := true
	changed := 0
	for _, d := range defaults {
		t, err := pg.GetTool(d.key)
		if err != nil {
			log.Printf("[tools] %s 조회 실패: %v", d.key, err)
			isAllOK = false
			continue
		}
		if t == nil || !t.System {
			continue
		}
		isDescLegacy := hasLegacyHan(t.Description, d.desc)
		isSchemaLegacy := hasLegacyHan(string(t.Schema), string(d.schema))
		if !isDescLegacy && !isSchemaLegacy {
			continue
		}
		desc, schema := t.Description, t.Schema
		if isDescLegacy {
			desc = d.desc
		}
		if isSchemaLegacy {
			schema = d.schema
		}
		if err := pg.RefreshToolDefaults(d.key, desc, schema); err != nil {
			log.Printf("[tools] %s 영어 기본값 반영 실패: %v", d.key, err)
			isAllOK = false
			continue
		}
		changed++
	}
	if changed > 0 {
		log.Printf("[tools] 중국어가 남은 시스템 도구 %d개를 영어 기본값으로 바꿨다(일회성)", changed)
	}
	return isAllOK
}
