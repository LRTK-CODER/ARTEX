package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 잘못된 JSON, 빈 입력, 타입이 틀린 필드는 모두 제로값 Filter,
	// 즉 '필터 없음'으로 물러나야 한다. 이 불변식이 '놓치느니 더 보낸다'가 자리 잡는 곳이다.
	// 여기를 오류나 부분 파싱으로 바꾸면 사용자가 글자 하나만 잘못 설정해도 높음 이상 알림이 모두 소리 없이 사라진다.
	cases := []struct {
		name string
		raw  string
	}{
		{"빈 입력", ""},
		{"잘못된 JSON", `{not json`},
		{"잘린 JSON", `{"min_severity":`},
		{"타입 불일치", `{"min_severity": 123, "task_ids": "abc"}`},
		{"최상위가 배열", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("잘못된 설정은 제로값 Filter로 물러나야 함, 실제값 %+v", f)
			}
			// 제로값 Filter는 어떤 이벤트와도 일치해야 한다.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("제로값 Filter는 모든 이벤트와 일치해야 함")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 모르는 심각도의 순번은 0이라 비어 있지 않은 기준이면 모두 걸러진다(의심스러우면 보내지 않음).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 결과 = %v, 기대값 %v", tc.min, tc.sev, got, tc.expect)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL 인젝션",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"빈 범위=제한 없음", Filter{}, true},
		{"작업 일치", Filter{TaskIDs: []int64{7}}, true},
		{"작업 불일치", Filter{TaskIDs: []int64{8}}, false},
		{"작업 여러 개 중 일치 포함", Filter{TaskIDs: []int64{8, 7}}, true},
		{"자산 겹침", Filter{AssetIDs: []int64{20, 99}}, true},
		{"자산 겹치지 않음", Filter{AssetIDs: []int64{99}}, false},
		{"작업과 자산 모두 일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"작업은 일치하지만 자산은 불일치", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("결과 = %v, 기대값 %v", got, tc.expect)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include가 비면 모두 받음", Filter{}, "아무 유형", true},
		{"include 일치", Filter{VulnClassInclude: []string{"SQL"}}, "SQL 인젝션", true},
		{"include 불일치", Filter{VulnClassInclude: []string{"명령 실행"}}, "SQL 인젝션", false},
		{"include 여러 단어 중 하나 일치", Filter{VulnClassInclude: []string{"명령 실행", "SQL"}}, "SQL 인젝션", true},
		{"대소문자 구분 없음", Filter{VulnClassInclude: []string{"sql"}}, "SQL 인젝션", true},
		{"exclude 일치하면 제외", Filter{VulnClassExclude: []string{"정보 유출"}}, "정보 유출", false},
		{"exclude 불일치하면 통과", Filter{VulnClassExclude: []string{"정보 유출"}}, "SQL 인젝션", true},
		// 제외가 포함보다 우선한다. 둘 다 일치하면 빠져야 한다.
		{"제외가 포함보다 우선", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"인젝션"},
		}, "SQL 인젝션", false},
		// 공백뿐인 키워드는 무시해야 한다. 그러지 않으면 '공백이 든 모든 문자열과 일치'로 물러난다.
		{"공백 키워드 무시", Filter{VulnClassInclude: []string{"", "  "}}, "SQL 인젝션", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("결과 = %v, 기대값 %v", got, tc.expect)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 기본값 꺼짐: 대부분의 사람이 말하는 '취약점 알림'은 새 취약점 발견이지 상태 변화의 장부가 아니다.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("상태 변경 이벤트는 켜지 않았으면 건너뛰어야 함")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change를 켜면 상태 변경 이벤트가 일치해야 함")
	}
	// 생성 이벤트는 on_status_change의 영향을 받지 않는다.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("생성 이벤트는 on_status_change에 의존하면 안 됨")
	}
}
