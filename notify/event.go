package notify

// Snapshot 은 notification_events.snapshot JSONB 열의 계약이다. 쓰는 쪽은 db 계층의
// 취약점 저장 트랜잭션이고, 읽는 쪽은 server 계층의 전달 엔진과 필터 대조다. 정의를 이
// 패키지에 두는 것은 '알림 도메인'의 페이로드이기 때문이다. db는 직렬화만 하고 필드 뜻은 모른다.
//
// 렌더링할 때 다시 조회하지 않고 취약점 필드를 중복 저장하는 이유: 취약점은 나중에 이름·심각도·상태가
// 바뀔 수 있는데, 알림 내용은 **이벤트가 일어난 당시**의 결론을 보여야 한다. 다시 조회하면 '나중에
// low로 바뀐' 값을 보여 줘 위험하게 오도한다. 또 fan-out과 렌더링이 findings/tasks/assets 세 테이블을 JOIN하지 않아도 된다.
type Snapshot struct {
	// 이벤트 유형: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed일 때만 비어 있지 않다.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item 은 알림으로 보낼 취약점 하나다. 알림 채널이 렌더링한다.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets 는 이름을 풀어 둔 자산 표시 이름(도메인/IP 등)이다. server 계층이 채운다.
	// 이 패키지는 데이터베이스를 쓰지 않아 이름을 얻을 수 없다.
	Assets []string
	// DetailURL 은 취약점 상세 링크다. 비어 있으면 public_base_url이 설정 안 된 것이고, 렌더링할 때 생략한다.
	DetailURL string
	// 상태 변경 이벤트 전용이다. 두 값이 모두 있으면 '처리 대기 → 수정됨'으로 렌더링한다.
	FromStatus string
	ToStatus   string
}

// IsStatusChange 는 이 항목이 상태 변경 이벤트인지 알려 준다.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title 은 항목의 표시 제목을 돌려준다. 사람이 지은 name을 먼저 쓰고, 없으면 취약점 유형 vulnclass를 쓰고,
// 둘 다 비어 있으면 자리표시자를 쓴다. 빈 제목은 절대 내보내지 않는다.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 취약점)"
}

// Message 는 알림 채널로 한 번 보내는 내용 전체다.
type Message struct {
	// 단건 알림이면 길이가 1이고, 다이제스트 알림이면 한 배치 전체다.
	// 빈 슬라이스는 잘못된 값이다. 호출자가 최소 한 건을 보장해야 한다.
	Items []Item
	// Batch=true면 다이제스트 메시지로 렌더링한다(제목을 바꾸고 시간 범위와 건수를 붙인다).
	Batch bool
	// WindowMinutes 는 다이제스트 주기(분)이고, Batch=true일 때 '최근 N분' 문구에만 쓴다.
	// 렌더링할 때 time.Since로 계산하지 않고 설정에서 일부러 넘겨받는다. 렌더링이 결정적이어야 테스트하기 쉽다.
	WindowMinutes int
	// HomeURL 은 플랫폼 대시보드 주소(전역 public_base_url)다. 비어 있으면 대시보드 링크를 넣지 않는다.
	HomeURL string
}
