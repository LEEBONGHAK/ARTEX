// Package notify 취약점 발견의 IM / 메일 푸시 채널 어댑터 계층을 구현한다.
//
// 계층 구조:이 패키지는 **리프 패키지**이고, 표준 라이브러리만 의존한다. DB를 모르고、server를 모른다. 채널 설정은
// map[string]any로 전달되고（notification_channels.config라는 JSONB 컬럼에 대응）,
// 푸시 대상 내용은 Message로 전달된다. 이렇게 분리하는 이점은:서명 계산、UTF-8 절단、필터 매칭 같은
// 진짜 오류 나기 쉬운 부분을 PostgreSQL에서 분리해 단위 테스트할 수 있고, 호스트는 server 측에서 편성만 하면 된다.
//
// 동시성 약정:Channel의 구현은 반드시 **무상태**여야 한다. 같은 Channel 인스턴스가 여러 채널 설정
// （심지어 같은 채널의 여러 봇 인스턴스）에 의해 동시 재사용되므로, 모든 자격 증명은 일률적으로 cfg 파라미터로 전달하고,
// webhook URL 같은 것을 구현 자체의 필드에 캐시하는 것을 허용하지 않는다.
package notify

// 채널 유형 식별자다. 값은 동시에 notification_channels.kind의 합법 집합이고, server 측이
// 화이트리스트 검증한다（findings.status와 같은 이치로, DB CHECK를 쓰지 않아, 이후 채널 추가가 편하다）.
const (
	KindDingTalk = "dingtalk" // 딩톡 커스텀 봇
	KindFeishu   = "feishu"   // 페이슈(Lark 포함) 커스텀 봇
	KindWeCom    = "wecom"    // 위챗워크 그룹 봇
	KindWebhook  = "webhook"  // 범용 Webhook:커스텀 메서드/헤더/JSON 템플릿
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP SMTP 메일
)

// 이벤트 유형, notification_events.kind에 대응.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind config에서 비어있는 kind의 폴백 값이다.
const InitKind = KindDingTalk

// severityRank 취약점 레벨을 비교 가능한 서수로 매핑한다. 알 수 없는 레벨은 0을 반환하므로, 어떤
// min_severity min_severity 설정도 알 수 없는 레벨을 밖으로 막는다——의심스러울 때 보내지 않아, 오탐 도배를 피한다.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank 레벨의 서수를 반환한다；알 수 없는 레벨은 0을 반환한다.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel emoji가 붙은 한국어 레벨명을 반환하며, 메시지 제목과 카드 배색에 쓴다.
// 알 수 없는 레벨은 원본 그대로 되비치고, 지어내지 않는다.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 심각"
	case "high":
		return "🟠 고위험"
	case "medium":
		return "🟡 중위험"
	case "low":
		return "🔵 저위험"
	default:
		return severity
	}
}

// StatusLabel 처리 상태를 한국어로 번역하며, 상태 변경 메시지에 쓴다.
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

// AtLeast severity가 min 문턱에 도달하는지 판단한다. min이 비면 문턱 미설정을 의미하고, 일률적으로 통과한다.
// 알 수 없는 severity의 서수는 0이라, 비어있지 않은 어떤 min에도 거부됨에 주의（severityRank 주석 참조）.
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
