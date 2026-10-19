package server

import (
	"log"

	"github.com/Autumn-27/artex/db"
)

// 사용자 지정 에이전트 두 개의 한국어 기본 이름·설명이다. 첫 시드(seedReporterAgent,
// seedFindingRetester)와 기존 설치 마이그레이션(reseedAgentsToKorean)이 함께 쓴다.
const (
	reporterAgentName        = "보고서 작성"
	reporterAgentDescription = "취약점 상세 보고서 작성: 취약점을 발견하면 자동으로 실행되어, 증거와 실행 과정을 조회한 뒤 Markdown 보고서를 작성해 기록한다."
	retesterAgentName        = "취약점 재검사"
	retesterAgentDescription = "취약점 상세에서 직접 시작하며, 원래 증거를 읽고 별도의 재검사 결론을 저장합니다."
)

// agentDefault 는 사용자 지정 에이전트 하나의 코드 기본 이름과 설명이다.
type agentDefault struct {
	key         string
	name        string
	description string
}

// seededAgentDefaults 는 시작 시 한 번 시드하는 사용자 지정 에이전트의 코드 기본값이다.
func seededAgentDefaults() []agentDefault {
	return []agentDefault{
		{key: "reporter", name: reporterAgentName, description: reporterAgentDescription},
		{key: db.FindingRetestAgentKey, name: retesterAgentName, description: retesterAgentDescription},
	}
}

// agentsKoreanFlag 는 #160 마이그레이션을 한 번만 돌리는 설정 키다.
const agentsKoreanFlag = "agent_defaults_v1_korean_names"

// reseedAgentsToKorean 은 번역(#126) 전에 설치한 DB에 남은 reporter·retester의 중국어
// 이름·설명을 코드의 한국어 기본값으로 바꾼다(한 번만). 두 에이전트는 builtin=false라
// 첫 설치 때만 넣고 이후에는 덮어쓰지 않기 때문에 번역이 기존 DB에 닿지 않았다.
// 사용자가 한국어·영어로 고친 값을 지우지 않으려고 옛 한자가 남은 쪽만 바꾼다.
func (s *Server) reseedAgentsToKorean() {
	if v, _, _ := s.m.pg.GetSetting(agentsKoreanFlag); v == "true" {
		return
	}
	if !refreshHanAgentDefaults(s.m.pg, seededAgentDefaults()) {
		return // 실패한 행이 있으면 플래그를 켜지 않아 다음 시작 때 다시 시도한다
	}
	if err := s.m.pg.SetSetting(agentsKoreanFlag, "true"); err != nil {
		log.Printf("[agents] %s 설정 저장 실패: %v", agentsKoreanFlag, err)
	}
}

// refreshHanAgentDefaults 는 defaults의 각 사용자 지정 에이전트에서 옛 한자가 남은 이름·설명만
// 코드 기본값으로 바꾼다. 행이 없으면(사용자가 지움) 다시 만들지 않고, 내장 에이전트는
// 건드리지 않는다. 바꾼 뒤에는 옛 한자가 없으므로 몇 번을 돌려도 결과가 같다. 모두 성공하면 true다.
func refreshHanAgentDefaults(pg *db.DB, defaults []agentDefault) bool {
	isAllOK := true
	changed := 0
	for _, d := range defaults {
		a, err := pg.GetAgentByKey(d.key)
		if err != nil {
			log.Printf("[agents] %s 조회 실패: %v", d.key, err)
			isAllOK = false
			continue
		}
		if a == nil || a.Builtin {
			continue
		}
		isNameLegacy := hasLegacyHan(a.Name, d.name)
		isDescLegacy := hasLegacyHan(a.Description, d.description)
		if !isNameLegacy && !isDescLegacy {
			continue
		}
		name, desc := a.Name, a.Description
		if isNameLegacy {
			name = d.name
		}
		if isDescLegacy {
			desc = d.description
		}
		if err := pg.UpdateAgentMeta(d.key, name, desc); err != nil {
			log.Printf("[agents] %s 한국어 기본값 반영 실패: %v", d.key, err)
			isAllOK = false
			continue
		}
		changed++
	}
	if changed > 0 {
		log.Printf("[agents] 중국어가 남은 사용자 지정 에이전트 %d개를 한국어 기본값으로 바꿨다(일회성)", changed)
	}
	return isAllOK
}
