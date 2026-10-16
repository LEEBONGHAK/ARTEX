package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter notification_channels.filter라는 JSONB 컬럼의 계약이다:채널 인스턴스의 필터 조건.
// 모든 필드는 선택이고, 생략하면「필터 안 함」——이것이 바로 기형 설정의 폴백 의미다, ParseFilter 참조.
type Filter struct {
	// MinSeverity 최저 레벨 문턱이다（low/medium/high/critical）, 비면=문턱 미설정.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs 빈 배열이면 제한 없음을 나타냄；비어있지 않으면 이벤트와 교집합이 있어야 함.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude 비면 전부 수신을 나타냄；비어있지 않으면 vulnclass가 그중 임의의 키워드에 명중해야 함.
	// VulnClassExclude 임의의 키워드에 명중하면 배제（배제가 포함보다 우선）.
	// 매칭 방식은 대소문자 구분 없는 부분 문자열이다——정규식보다 안전하다:사용자가 정규식을 잘못 설정해도 채널이 조용히 무효화되지 않는다.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange 이 채널이 취약점 상태 변경 이벤트를 수신할지 결정한다（realtime 모드에서만 의미 있음）.
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter 채널 필터 설정을 파싱한다.
//
// **절대 error를 반환하지 않는다.** 이것은 의도적인 설계 선택이다:필터 조건 설정이 기형일 때 일률적으로 제로값으로 퇴화한다
// Filter필터 안 함 = 전부 명중）, 왜냐하면 취약점 알림 시스템에게는, **한 건 더 보내는 게
// 고위험 한 건을 조용히 놓치는 것보다 훨씬 낫기** 때문이다. 파싱 실패를「푸시 안 함」으로 만드는 것은, 사용자에게 설정된 것처럼 보이는、
// 실제로는 아무것도 푸시하지 않는 채널을 주는 셈이다——이것이 가장 나쁜 실패 모드다.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 파싱 실패 시 f는 제로값을 유지한다, 즉 필터 안 함.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity s가 합법적인 레벨 문턱인지 보고한다（빈 문자열은 문턱 미설정을 나타냄）.
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate 필터 설정에서 **값이 제한된** 필드를 검증하며, 채널 저장 시 호출하도록 제공한다.
//
// 왜 반드시 쓰기 시 막아야 하나:Match는 알 수 없는 문턱에 대한 판정이 `rank >= 0`이라, 항상 참이다——
// 즉 min_severity에 한 글자만 틀려도（"hgih"）, 필터가 **조용히 무효화**돼
// 「전체 푸시」가 된다. 이것은 이 패키지의「적게 보내느니 많이, 누락은 안 됨」취사 방향과 일치한다（누락 안 함）,
// 하지만 결과는 사용자가 등급별 푸시를 하고 있다고 여기지만、실제로는 전체 취약점을 그룹에 들이붓는 것이고,
// 게다가 그가 잘못 설정했다는 어떤 징후도 없다. 이런「조용한 강등」이야말로 입구에서 막아야 한다.
//
// Validate는 **쓰기** 경로에만 쓰임에 주의. 읽기 경로는 여전히 ParseFilter의 관용적 의미로 가고,
// 이렇게 하면 과거 데이터에 이미 존재하는 나쁜 값이 채널 전체를 못 읽게 만들지 않는다.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("최저 레벨 %q 무효, 선택 가능: low / medium / high / critical, 또는 비우면 제한 없음", f.MinSeverity)
	}
	return nil
}

// Match 한 이벤트가 그 필터 조건을 가진 채널로 전송돼야 하는지 판정한다.
//
// **절대 error를 반환하지 않는다**, 이유는 ParseFilter와 같다:어떤 내부 예외든「명중」으로 처리한다.
// 판정 순서:이벤트 유형 → 레벨 문턱 → 작업/자산 범위 → 취약점 유형 키워드.
func Match(f Filter, s Snapshot) bool {
	// 상태 변경 이벤트는 명시적으로 켠 채널만 수신한다. 기본 꺼짐, 왜냐하면 대다수 사용자가
	// 「푸시」가「새 취약점 발견」을 가리키길 기대하지, 모든 상태 전이를 유수장부식으로 따라가는 게 아니기 때문이다.
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
	// 배제 우선:임의의 배제 키워드에 명중하면 탈락, 동시에 포함 목록에 명중했더라도.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 작은 집합은 선형 스캔이면 된다；양쪽 규모 모두「사람이 손으로 체크한 수십 개」라,
	// map을 만드는 비용이 이득보다 크다.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold s가 keywords 중 임의의 키워드를 포함하는지 보고한다（대소문자 구분 없음）.
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
