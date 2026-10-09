// Package notify 는 취약점 발견 사항을 IM / 이메일로 보내는 알림 채널 어댑터 계층이다.
//
// 계층: 이 패키지는 **잎 패키지**이고 표준 라이브러리에만 의존한다. 데이터베이스도 server도 모른다. 알림 채널 설정은
// map[string]any로 받고(notification_channels.config JSONB 열에 해당한다),
// 보낼 내용은 Message로 받는다. 이렇게 나누면 서명 계산, UTF-8 자르기, 필터 대조처럼
// 실제로 틀리기 쉬운 부분을 PostgreSQL 없이 단위 테스트할 수 있고, 호스트는 server 쪽에서 조율만 하면 된다.
//
// 동시성 약속: Channel 구현은 **상태가 없어야** 한다. 같은 Channel 인스턴스를 여러 알림 채널 설정이
// (같은 알림 채널의 여러 봇 인스턴스까지) 동시에 함께 쓰므로, 자격 증명은 모두 cfg 파라미터로 받는다.
// 웹훅 URL 같은 것을 구현 자신의 필드에 캐시하면 안 된다.
package notify

// 알림 채널 유형 식별자. 값은 notification_channels.kind의 허용 집합이기도 하며, server 쪽
// 허용 목록으로 검사한다(findings.status와 같은 이유로 DB CHECK를 쓰지 않아 나중에 알림 채널을 더하기 쉽다).
const (
	KindDingTalk = "dingtalk" // DingTalk 사용자 지정 봇
	KindFeishu   = "feishu"   // Feishu(Lark 포함) 사용자 지정 봇
	KindWeCom    = "wecom"    // WeCom 그룹 봇
	KindWebhook  = "webhook"  // 범용 웹훅: 사용자 지정 메서드/헤더/JSON 템플릿
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP 이메일
)

// 이벤트 유형. notification_events.kind에 해당한다.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind 는 config에 kind가 비어 있을 때 쓰는 기본값이다.
const InitKind = KindDingTalk

// severityRank 는 취약점 심각도를 비교할 수 있는 순번으로 바꾼다. 모르는 심각도는 0을 돌려주므로
// 어떤 min_severity 설정이든 모르는 심각도를 걸러 낸다. 의심스러우면 보내지 않아 오탐으로 알림이 넘치지 않게 한다.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank 는 심각도의 순번을 돌려준다. 모르는 심각도는 0을 돌려준다.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel 은 이모지를 붙인 한국어 심각도 이름을 돌려준다. 메시지 제목과 카드 색에 쓴다.
// 모르는 심각도는 지어내지 않고 그대로 돌려준다.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 치명"
	case "high":
		return "🟠 높음"
	case "medium":
		return "🟡 중간"
	case "low":
		return "🔵 낮음"
	default:
		return severity
	}
}

// StatusLabel 은 처리 상태를 한국어로 바꾼다. 상태 변경 메시지에 쓴다.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "처리 대기"
	case "in_progress":
		return "처리 중"
	case "confirmed":
		return "확인됨"
	case "resolved":
		return "처리됨"
	case "fixed":
		return "수정됨"
	case "false_positive":
		return "오탐"
	case "ignored":
		return "무시"
	case "duplicate":
		return "중복"
	case "risk_accepted":
		return "위험 수용"
	default:
		return status
	}
}

// AtLeast 는 severity가 min 기준에 이르는지 판단한다. min이 비어 있으면 기준이 없는 것이라 모두 통과한다.
// 모르는 severity의 순번은 0이라 비어 있지 않은 min이면 모두 걸러진다(severityRank 주석 참고).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
