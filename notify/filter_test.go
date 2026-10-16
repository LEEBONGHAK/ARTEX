package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 비정상 JSON、빈 입력、유형이 틀린 필드——전부 제로값 Filter로 퇴화해야 함,
	// 즉 「필터링 안 함」. 이 불변식은 「덜 보내느니 더 보낸다」의 귀착점:
	// 여기가 에러나 반쪽 파싱으로 바뀌면, 사용자가 한 글자만 잘못 설정해도 모든 고위험 알림을 조용히 잃음.
	cases := []struct {
		name string
		raw  string
	}{
		{"빈 입력", ""},
		{"비합법 JSON", `{not json`},
		{"절단된 JSON", `{"min_severity":`},
		{"유형 불일치", `{"min_severity": 123, "task_ids": "abc"}`},
		{"최상위가 배열", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("비정상 구성은 제로값 Filter로 퇴화해야 하는데 %+v", f)
			}
			// 제로값 Filter는 임의의 이벤트에 명중해야 함.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("제로값 Filter는 모든 이벤트에 명중해야 함")
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
		// 알 수 없는 등급 서수는 0, 비어있지 않은 어떤 임계값에도 막혀야 함(의심 시 보내지 않음).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 기대 %v 실제 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL注入",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"빈 범위=무제한", Filter{}, true},
		{"작업 명중", Filter{TaskIDs: []int64{7}}, true},
		{"작업 미명중", Filter{TaskIDs: []int64{8}}, false},
		{"작업 다중선택에 명중 포함", Filter{TaskIDs: []int64{8, 7}}, true},
		{"자산 교집합 있음", Filter{AssetIDs: []int64{20, 99}}, true},
		{"자산 교집합 없음", Filter{AssetIDs: []int64{99}}, false},
		{"작업과 자산 동시 명중", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"작업 명중이나 자산 미명중", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("기대 %v 실제 %v", tc.expect, got)
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
		{"include 비면=전부 수용", Filter{}, "任意类型", true},
		{"include 명중", Filter{VulnClassInclude: []string{"SQL"}}, "SQL注入", true},
		{"include 미명중", Filter{VulnClassInclude: []string{"命令执行"}}, "SQL注入", false},
		{"include 다중어 중 하나 명중", Filter{VulnClassInclude: []string{"命令执行", "SQL"}}, "SQL注入", true},
		{"대소문자 구분 안 함", Filter{VulnClassInclude: []string{"sql"}}, "SQL注入", true},
		{"exclude 명중 시 배제", Filter{VulnClassExclude: []string{"信息泄露"}}, "信息泄露", false},
		{"exclude 미명중이면 통과", Filter{VulnClassExclude: []string{"信息泄露"}}, "SQL注入", true},
		// 배제가 포함보다 우선: 동시 명중 시 탈락해야 함.
		{"배제가 포함보다 우선", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"注入"},
		}, "SQL注入", false},
		// 순수 공백 키워드는 무시해야 함, 아니면 「공백 든 모든 문자열 매칭」으로 퇴화.
		{"공백 키워드 무시", Filter{VulnClassInclude: []string{"", "  "}}, "SQL注入", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("기대 %v 실제 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 기본 꺼짐: 대부분의 사람이 말하는 「취약점 푸시」는 새 취약점 발견을 뜻하지 상태 변경 내역이 아님.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("상태 변경 이벤트는 미활성 시 건너뛰어야 함")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change 활성 후 상태 변경 이벤트는 명중해야 함")
	}
	// 생성 이벤트는 on_status_change의 영향을 받지 않음.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("생성 이벤트는 on_status_change에 의존하면 안 됨")
	}
}
