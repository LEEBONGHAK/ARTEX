package notify

// Snapshot notification_events.snapshot이라는 JSONB 컬럼의 계약이다. 쓰는 쪽은 db 계층의
// 취약점 저장 트랜잭션이고, 읽는 쪽은 server 계층의 전송 엔진과 필터 매칭이다. 정의를 이 패키지에 둔 것은 그것이
// 「알림 도메인」의 페이로드이기 때문이다:db는 직렬화만 담당하고, 필드 의미를 이해하지 않는다.
//
// 왜 취약점 필드를 중복 저장하고 렌더링 시 되조회하지 않나:취약점은 사후에 이름、레벨、상태가 바뀌는데,
// 푸시 내용은 **사건 발생 당시**의 결론을 반영해야 한다——되조회하면「사후에 low로 바뀐」
// 위험한 오도를 얻는다. 또한 fan-out과 렌더링이 그래서 findings/tasks/assets 세 표를 JOIN할 필요가 없다.
type Snapshot struct {
	// 이벤트 유형:finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed일 때만 비어있지 않음.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item 푸시 대기 중인 취약점 하나이며, 채널 렌더링에 제공된다.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets 파싱된 자산 표시 이름이다（예: 도메인/IP）. server 계층이 채운다——
	// 이 패키지는 DB를 건드리지 않아, 이름을 얻지 못한다.
	Assets []string
	// DetailURL 취약점 상세 백링크다；비면 public_base_url 미설정을 의미하고, 렌더링 시 생략한다.
	DetailURL string
	// 상태 변경 이벤트 전용；두 항목 모두 비어있지 않으면「처리 대기 → 수정됨」으로 렌더링된다.
	FromStatus string
	ToStatus   string
}

// IsStatusChange 이 항목이 상태 변경 이벤트인지 보고한다.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title 항목의 표시 제목을 반환한다:수동 명명한 name을 우선하고, 취약점 유형 vulnclass로 폴백하며,
// 둘 다 비면 플레이스홀더 하나를 쓴다——절대 빈 제목을 출력하지 않는다.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 취약점)"
}

// Message 한 번의 채널 발송의 완전한 내용이다.
type Message struct {
	// 단건 푸시 시 길이는 1；요약 푸시（digest） 시에는 한 배치 전체.
	// 빈 슬라이스는 부적법하고, 호출 측은 최소 한 건을 보장해야 한다.
	Items []Item
	// Batch=true Batch=true일 때 요약 메시지로 렌더링한다（제목 교체、시간 창과 건수 포함）.
	Batch bool
	// WindowMinutes 요약 주기다（분）, Batch=true일 때만 문구「최근 N분」에 쓴다.
	// 의도적으로 설정에서 명시적으로 전달하고 렌더링 시 time.Since로 계산하지 않는다:렌더링이 결정적이어야 테스트하기 좋다.
	WindowMinutes int
	// HomeURL 플랫폼 패널 주소다（전역 public_base_url）；비면 패널 입구를 포함하지 않는다.
	HomeURL string
}
