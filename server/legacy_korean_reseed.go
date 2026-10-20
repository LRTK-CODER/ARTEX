package server

import (
	"log"

	"github.com/Autumn-27/artex/db"
)

// 이 파일의 중국어 상수는 코드가 아니라 비교용 데이터다. 번역 전에 설치한 DB에 남은 옛 기본값과
// 글자 단위로 같은 행만 골라 한국어로 바꾸기 위해 보존한다.

// 0.3.8 뒤, 한국어 번역(#137) 전의 reporter 트리거 기본 문구다(커밋 72a52e9의 기본값).
// upgradeReporterTriggerMessage는 V1만 비교하는데, 이 문구로 시드되거나 업그레이드된 설치는
// 그 플래그가 이미 켜져 있어 한국어 기본값을 받지 못했다(#170).
const reporterToolCallMessageV2 = "上面刚有一个漏洞被 report_finding 登记。请读取返回 JSON 的 finding_id（独立漏洞记录 ID）与 finding_node_id（探索节点 ID），" +
	"先用 get_finding_traffic(finding_id) 读取当前证据清单及其 version（空清单是正常情况，照常写报告）；" +
	"若运行指引启用自动绑定，在读取前先核实并关联本次漏洞的流量。节点详情使用 finding_node_id。" +
	"最后调用 update_finding_report(finding_id=finding_node_id, report, evidence_version=实际读取版本) 保存，" +
	"evidence_version 必须传，否则报告会被永久标记为待更新。不要混用两种编号。"

// legacyText 는 옛 중국어 기본 문구 하나와 그것을 대신할 한국어 문구다.
type legacyText struct {
	chinese string
	korean  string
}

// legacyPromptNotes 는 번역(#119) 전 시스템이 프롬프트 버전에 남긴 메모다.
var legacyPromptNotes = []legacyText{
	{chinese: "内置默认", korean: db.PromptNoteBuiltinDefault},
	{chinese: "恢复为内置默认", korean: db.PromptNoteResetDefault},
}

// legacyTaskAssetLinkSummaries 는 번역(#119) 전 schema.sql이 작업-자산 연결에 남긴 출처 설명이다.
var legacyTaskAssetLinkSummaries = []legacyText{
	{chinese: "任务执行期间自动关联", korean: db.TaskAssetLinkSummaryAuto},
	{chinese: "由历史任务资产关联迁移", korean: db.TaskAssetLinkSummaryLegacy},
}

// legacyKoreanFlag 는 #170 마이그레이션을 한 번만 돌리는 설정 키다.
const legacyKoreanFlag = "legacy_defaults_v1_korean_reporter_trigger_notes"

// reseedLegacyDefaultsToKorean 은 번역 전 설치에 남은 reporter 트리거 기본 문구, 프롬프트 버전 메모,
// 작업-자산 연결 출처 설명을 한국어로 바꾼다(한 번만). 사용자가 고친 값을 지우지 않으려고
// 옛 기본값과 글자가 정확히 같은 값만 바꾼다.
func (s *Server) reseedLegacyDefaultsToKorean() {
	runMigrationOnce(s.m.pg, legacyKoreanFlag, func() bool {
		return refreshLegacyChineseDefaults(s.m.pg, "reporter")
	})
}

// runMigrationOnce 는 flag가 켜져 있지 않을 때만 migrate를 돌리고, migrate가 모두 성공(true)했을
// 때만 flag를 켠다. 실패하면 flag를 켜지 않아 다음 시작 때 다시 시도한다. migrate는 몇 번을
// 돌려도 결과가 같아야 한다.
func runMigrationOnce(pg *db.DB, flag string, migrate func() bool) {
	v, _, err := pg.GetSetting(flag)
	if err != nil {
		log.Printf("[migrate] %s 설정 읽기 실패: %v", flag, err)
		return
	}
	if v == "true" {
		return
	}
	if !migrate() {
		return
	}
	if err := pg.SetSetting(flag, "true"); err != nil {
		log.Printf("[migrate] %s 설정 저장 실패: %v", flag, err)
	}
}

// refreshLegacyChineseDefaults 는 reporterKey 에이전트의 도구 호출 트리거 가운데 메시지가 옛 중국어
// 기본 문구(V1, V2)와 같은 것을 코드의 한국어 기본값으로 바꾸고, 옛 프롬프트 버전 메모와
// 작업-자산 연결 출처 설명을 대응하는 한국어 문구로 바꾼다. 모두 성공하면 true다.
func refreshLegacyChineseDefaults(pg *db.DB, reporterKey string) bool {
	isAllOK := refreshLegacyReporterTriggers(pg, reporterKey)
	for _, n := range legacyPromptNotes {
		changed, err := pg.ReplacePromptNote(n.chinese, n.korean)
		if err != nil {
			log.Printf("[prompts] 옛 메모 %q 한국어 반영 실패: %v", n.chinese, err)
			isAllOK = false
			continue
		}
		if changed > 0 {
			log.Printf("[prompts] 옛 메모 %q를 %q로 바꿨다(%d행, 일회성)", n.chinese, n.korean, changed)
		}
	}
	assets := pg.Assets()
	for _, l := range legacyTaskAssetLinkSummaries {
		changed, err := assets.ReplaceTaskAssetLinkSummary(l.chinese, l.korean)
		if err != nil {
			log.Printf("[assets] 옛 연결 출처 설명 %q 한국어 반영 실패: %v", l.chinese, err)
			isAllOK = false
			continue
		}
		if changed > 0 {
			log.Printf("[assets] 옛 연결 출처 설명 %q를 %q로 바꿨다(%d행, 일회성)", l.chinese, l.korean, changed)
		}
	}
	return isAllOK
}

// refreshLegacyReporterTriggers 는 reporterKey의 도구 호출 트리거 메시지가 옛 중국어 기본 문구와
// 같을 때만 한국어 기본값으로 바꾼다. 모두 성공하면 true다.
func refreshLegacyReporterTriggers(pg *db.DB, reporterKey string) bool {
	triggers, err := pg.ListTriggersFor(reporterKey)
	if err != nil {
		log.Printf("[reporter] 트리거 읽기 실패: %v", err)
		return false
	}
	isAllOK := true
	for _, t := range triggers {
		if !t.OnToolCall || (t.ToolCallMessage != reporterToolCallMessageV1 && t.ToolCallMessage != reporterToolCallMessageV2) {
			continue // 사용자가 고친 메시지나 다른 트리거는 그대로 둔다
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 트리거 %d 한국어 기본값 반영 실패: %v", t.ID, err)
			isAllOK = false
			continue
		}
		log.Printf("[reporter] 트리거 %d의 중국어 기본 메시지를 한국어 기본값으로 바꿨다(일회성)", t.ID)
	}
	return isAllOK
}
