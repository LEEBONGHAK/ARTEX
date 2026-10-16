package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel 하나의 알림 채널 어댑터다. 구현은 반드시 **무상태**여야 한다:같은 인스턴스가 여러 채널에
// 설정에 의해 동시 재사용되므로, 자격 증명은 일률적으로 cfg 파라미터로 전달한다.
type Channel interface {
	// Kind 채널 유형 식별자를 반환하며, 레지스트리의 키와 일치해야 한다.
	Kind() string
	// Validate 설정 저장 시 호출되며, 필수 필드와 형식을 검증한다. 반환되는 에러는
	// 설정자에게 바로 표시되므로, 문구는「어느 필드가 빠졌는지」를 설명해야 하고 막연한「설정 무효」가 아니어야 한다.
	Validate(cfg map[string]any) error
	// Send 메시지를 한 번 전송하고, **실제로 송달된 항목 수**와 에러를 반환한다.
	//
	// 왜 건수를 반환하나:각 플랫폼은 메시지 길이 상한이 있어, 요약 메시지가 전체 배치를 담지 못하면 잘린다.
	// 호출 측이 무조건 전체 배치를 송달됨으로 표시하면, 잘려 나간 그 항목들은 사라진다——메시지에서도 안 보이고、
	// 전송 히스토리에도 성공으로 표시돼, 취약점이 한 번도 발송되지 않았음을 발견할 수 있는 곳이 아무 데도 없다. kept를 반환한 뒤,
	// 호출 측은 앞 kept건만 표시하고, 나머지는 다음 배치로 남긴다.
	//
	// 에러 반환은 전송 실패를 나타내고, 그중 *PermanentError는 재시도하지 말아야 함을 나타낸다.
	// 실패 시 kept는 의미가 없으니, 호출 측은 그것을 무시해야 한다.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin 해당 채널이 공식적으로 권장하는 분당 전송 상한을 반환하며, 새 채널 인스턴스 생성 시
	// 의 기본 레이트 리밋 값으로 쓴다. 0을 반환하면 알려진 제한이 없음을 뜻한다.
	DefaultRatePerMin() int
	// SecretKeys 해당 채널 설정에서 자격 증명에 속하는 키 이름을 반환한다. API 되비침 시 이 키들의 값은 마스킹되고,
	// 업데이트 시 마스킹 값을 받으면 DB의 원래 값을 보존한다. 어느 필드가 자격 증명인지는 구현 자신만 명확히 알므로
	// （위챗워크의 전체 Webhook 주소가 곧 자격 증명이고, 딩톡은 그중 secret만 해당）,
	// 그래서 이 지식은 반드시 채널이 제공해야 하고, 상위 계층이 추측해선 안 된다.
	SecretKeys() []string
	// DestinationKeys 해당 채널 설정에서「메시지를 어디로 보낼지」를 결정하는 키 이름을 반환한다.
	//
	// SecretKeys와 마찬가지로 보안 관련이다:대상 주소와 자격 증명은 서로 독립된 두 세트의 필드이고,
	// 「주소만 바꾸고、자격 증명은 원본 그대로 보존」을 허용하면, 채널 설정을 바꿀 수 있는 누구든 DB의 진짜 자격 증명을
	// 자신이 제어하는 서버로 보낼 수 있어, 채널 설정의 마스킹이 완전히 의미를 잃는다.
	// 자세한 내용은 PrepareConfigUpdate 참조.
	DestinationKeys() []string
}

// registry 채널 레지스트리다. 의도적으로 init() 자가 등록이 아니라 명시적 리터럴을 쓴다:이렇게 하면「어떤 채널이 있는지」가
// 한 곳에서 다 볼 수 있고, 채널 추가 시 누락이 컴파일 타임에 드러나며, 런타임 부작용에 기대지 않는다.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get 유형별로 채널 구현을 가져온다.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind kind가 지원되는 채널 유형인지 보고한다.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds 지원되는 모든 채널 유형을 반환하며, 사전순으로 정렬한다（UI 드롭다운이 안정적으로 표시하도록）.
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError 재시도하지 말아야 할 전송 실패를 표시한다:자격 증명 오류、대상 거부、요청 본문 부적법 등.
// 재시도는 일시적 장애（네트워크 흔들림、레이트 리밋、대상단 5xx）에만 의미 있다；영구 실패에 반복 백오프 재시도는
// 성공하지도 않고, 진짜 에러를 재시도 로그에 묻어버린다.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent err을 영구 실패로 표시한다. err이 nil이면 nil을 반환해,
// `return Permanent(someCheck())`로 쓰기 편하게 한다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent err 체인에 영구 실패 표시가 있는지 보고한다.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 설정 읽기helper ----
//
// 채널 설정은 데이터베이스의 JSONB 컬럼에서 오며, encoding/json으로 역직렬화하면 map[string]any이고,
// 수치는 일률적으로 float64、배열은 []any다. 아래 helper들이 이 변환 계층을 통일하고, 사용자가
// UI에서 비워 둬 생기는 타입 편차를 허용한다（예: 포트를 문자열로 입력）.

// cfgString 문자열 설정 항목을 가져오고, 앞뒤 공백을 일률적으로 잘라낸다——웹 폼에서 복사-붙여넣기하면 쉽게 딸려 온다.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt 정수 설정 항목을 가져오며, float64（JSON 기본）와 문자열 두 종류 출처를 호환한다.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool 불리언 설정 항목을 가져오며, 문자열 "true"/"1"을 호환한다.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings 문자열 배열 설정 항목을 가져오며, 자동으로 공백을 자르고 빈 문자열을 버린다.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 단일 문자열도 받아들여, 값이 하나뿐일 때의 폼 제출을 편하게 한다.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap 문자열 매핑 설정 항목을 가져오며（예: 커스텀 HTTP 헤더）, 키-값 모두 공백을 자르고 빈 키를 버린다.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
