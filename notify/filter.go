package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter 는 notification_channels.filter JSONB 열의 계약으로, 알림 채널 인스턴스의 필터 조건이다.
// 모든 필드는 선택이고 생략하면 '필터 없음'이다. 이것이 바로 잘못된 설정의 대비 의미다. ParseFilter 참고.
type Filter struct {
	// MinSeverity 는 최저 심각도 기준(low/medium/high/critical)이다. 비어 있으면 기준이 없다.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs가 빈 배열이면 제한 없음이고, 값이 있으면 이벤트와 겹치는 것이 있어야 한다.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude 가 비어 있으면 모두 받고, 값이 있으면 vulnclass가 그중 어느 키워드와 일치해야 한다.
	// VulnClassExclude는 어느 키워드와 일치하면 제외한다(제외가 포함보다 우선한다).
	// 대소문자를 구분하지 않는 부분 문자열로 대조한다. 정규식보다 안전하다. 사용자가 정규식을 잘못 설정해 알림 채널이 소리 없이 무력해지지 않는다.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange 는 이 알림 채널이 취약점 상태 변경 이벤트를 받을지 정한다(realtime 모드에서만 의미가 있다).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter 는 알림 채널 필터 설정을 파싱한다.
//
// **error를 절대 돌려주지 않는다.** 일부러 이렇게 설계했다. 필터 설정이 잘못되면 모두 제로값
// Filter(= 필터 없음 = 모두 일치)로 물러난다. 취약점 알림 시스템에서는 **한 건 더 보내는 편이
// 높음 심각도 한 건을 소리 없이 놓치는 것보다 훨씬 낫기** 때문이다. 파싱 실패를 '보내지 않음'으로 만들면 설정이 된 것처럼 보이지만
// 실제로는 아무것도 보내지 않는 알림 채널을 사용자에게 주게 된다. 가장 나쁜 실패 방식이다.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 파싱에 실패하면 f는 제로값, 즉 필터 없음으로 남는다.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity 는 s가 올바른 심각도 기준인지 알려 준다(빈 문자열은 기준 없음이다).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate 는 필터 설정에서 **값이 제한된** 필드를 검사한다. 알림 채널을 저장할 때 부른다.
//
// 쓸 때 막아야 하는 이유: Match는 모르는 기준을 `rank >= 0`으로 판정해 늘 참이다.
// 즉 min_severity에 오타 하나("hgih")만 있어도 필터가 **소리 없이 무력해져**
// '모두 보내기'가 된다. 이 패키지의 '놓치느니 더 보낸다'는 방향과는 맞지만(놓치지는 않는다),
// 결과적으로 사용자는 심각도별로 나눠 보낸다고 믿는데 실제로는 모든 취약점을 그룹에 쏟아붓고,
// 설정이 틀렸다는 단서도 전혀 없다. 이런 '소리 없는 기능 저하'는 입구에서 막아야 한다.
//
// Validate는 **쓰기** 경로에만 쓴다. 읽기 경로는 여전히 ParseFilter의 너그러운 의미를 따르므로,
// 이력 데이터에 이미 있는 잘못된 값 때문에 알림 채널 전체를 읽지 못하는 일이 없다.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("최저 심각도 %q은(는) 잘못된 값입니다. low / medium / high / critical 중 하나를 고르거나, 비워 두면 제한 없음입니다.", f.MinSeverity)
	}
	return nil
}

// Match 는 이벤트를 이 필터 조건의 알림 채널로 전달해야 하는지 판정한다.
//
// **error를 절대 돌려주지 않는다.** 이유는 ParseFilter와 같다. 내부 이상은 모두 '일치'로 다룬다.
// 판정 순서: 이벤트 유형 → 심각도 기준 → 작업/자산 범위 → 취약점 유형 키워드.
func Match(f Filter, s Snapshot) bool {
	// 상태 변경 이벤트는 명시적으로 켠 알림 채널만 받는다. 기본값 꺼짐인 이유는 대부분의 사용자가
	// '알림'을 '새 취약점 발견'으로 기대하지, 상태가 바뀔 때마다 장부처럼 따라가길 바라지 않기 때문이다.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 제외 우선: 포함 목록과 동시에 일치하더라도 제외 키워드 하나와 일치하면 빠진다.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 작은 집합이라 선형 탐색이면 된다. 양쪽 모두 '사람이 손으로 고른 수십 개' 규모라
	// map을 만드는 비용이 이득보다 크다.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold 는 s가 keywords 중 어느 키워드를 포함하는지 알려 준다(대소문자 구분 없음).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
