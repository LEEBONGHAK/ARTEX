package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes s를 max 바이트를 넘지 않게 자르고, 결과가 합법적 UTF-8이며 문자를 끊지 않도록 보장한다.
//
// 왜 반드시 문자 경계로 잘라야 하나:위챗워크 그룹 봇의 markdown은 4096 **바이트** 하드 상한이 있다（문자
// 수가 아니라), 중국어 한 글자는 3바이트다. 바이트로 직접 슬라이스하면 한자 하나를 반으로 잘라, 부적법한
// UTF-8——플랫폼 측이 전체를 거부하거나, 깨진 글자 사각형으로 표시한다. 여기서의 방식은 먼저 예산 위치에서
// 앞으로 가장 가까운 rune 시작 바이트까지 되돌린다（utf8.RuneStart가 연속 바이트 0b10xxxxxx를 판정）.
//
// max<=0 제한 없음을 나타낸다. 절단 후 말줄임표를 덧붙인다, max가 말줄임표를 담지 못할 만큼 작지 않은 한.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max max가 말줄임표보다도 짧음:말줄임표를 포기하고, 순수 절단해, 결과가 오히려 max를 초과하는 것을 피한다.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine 여러 줄 텍스트를 단일 행으로 압축한다:모든 공백을 접고, 다시 문자 수로 자른다.
// IM 메시지의 제목 행에 쓴다——요약에는 줄바꿈이 흔한데, 표/제목에 바로 쑤셔 넣으면 레이아웃이 깨진다.
// max<=0 길이 제한 없음을 나타낸다.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes s를 max 문자 수를 넘지 않게 자르고（바이트가 아님）, 초과 시 말줄임표를 덧붙인다.
// max<=0 제한 없음을 나타낸다.
//
// TruncateBytes와의 차이는 플랫폼 기준에 있다:위챗워크는 바이트로 길이 제한, Telegram은 문자 수로 길이 제한.
// 기준을 잘못 쓰면 에러는 안 나고, 메시지가 예상보다 훨씬 짧게 잘릴 뿐이다（중국어 1자 = 3바이트,
// 바이트로 4096을 자르면 약 1365자만 남음）, 그래서 두 함수를 모두 보존하고、채널별로 골라 써야 한다.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML 문자 수로 HTML 조각을 자르고, 반 토막 태그가 생기지 않도록 보장한다.
//
// HTML에 바로 문자 절단을 하면 `<a href="htt` 같은 불완전 태그가 잘려 나와, 플랫폼 파서가
// 에러를 내 전체를 거부하거나、후속 본문을 속성 값으로 삼켜버린다. 여기서의 방식은:먼저 문자로 자르고,
// 그다음 꼬리에 닫히지 않은 `<`가 있는지 검사해, 있으면 그 앞으로 되돌린다.
//
// 태그 균형 맞추기는 하지 않는다（</b> 같은 것을 보완）:Telegram의 HTML 파서가 닫히지 않은 태그를 자동으로 닫고,
// 직접 균형 맞추기를 구현하면 속성 안의 따옴표、주석、자체 닫힘 태그를 처리해야 해서, 복잡도와 이득이 비례하지 않는다.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 꼬리가 `<`로 시작하는 조각이면（마지막 `<` 이후에 `>`가 없음）, `<` 앞으로 되돌린다.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 꼬리가 잘린 HTML 엔티티면（예: `&amp;`가 `&amp`로 잘림）, 마찬가지로 되돌려야 한다.
	// 엔티티 조각은 엔티티만 인식하는 파서에서 **전체 메시지**를 거부당하게 할 수 있다——길이 상한을
	// 초과한 요약 메시지는 원래 흔하니, 이 때문에 전체 알림을 버릴 가치는 없다.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount 예산 내에 **완전하게** 몇 건을 담을 수 있는지 계산하며, 요약 메시지가 건 단위로 패킹하도록 제공한다.
//
// 왜 전체 편을 렌더링한 뒤 자르지 않고 건 단위로 하나:절단은 뒷부분 항목을 허공으로 사라지게 하고,
// 그런데 그것들의 전송 기록은 여전히 송달됨으로 표시된다——메시지에서도 안 보이고、전송 히스토리에서도 안 보여,
// 취약점이 그렇게 사라진다. 건 단위로 패킹한 뒤에는, 담지 못한 항목이 DB에 남아 다음 배치가 되고,
// 호출 측이 받는 kept가 바로 이 메시지가 실제로 송달한 건수다.
//
// 파라미터:maxSize<=0은 제한 없음을 나타냄；reserve는 메시지 헤더/푸터에 예약하는 양；
// size size는 계량을 담당（플랫폼마다 기준 다름:위챗워크/딩톡은 바이트, Telegram은 문자 수————
// 기준을 잘못 쓰면 에러는 안 나고, 중국어 메시지가 상한보다 훨씬 작게 압축될 뿐이다）；
// render render는 idx번째 건을 그 실제 텍스트로 렌더링한다——길이는 내용에 따라 다르니, 추정에 기댈 수 없다.
//
// 최소 1을 반환한다（항목이 남아 있기만 하면）. 단일 건이 극단적으로 길어도 이 건은 발송해야 하며、호출 측의
// 최종 절단이 폴백한다, 아니면 초장문 취약점 하나가 전체 배치를 제자리에 영구히 막는다.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize packItemCount의 두 가지 계량 기준이며, 이름을 붙인 것은 호출처에
// 맨 func(s string) int 클로저가 나타나는 것을 피하기 위함이다, 아니면 어느 기준을 쓰는지 한눈에 보기 어렵다.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine 자산 목록을 한 줄 표시 텍스트로 렌더링하고, limit개를 초과하면 나머지를 생략하고 총수를 표기한다.
// 한 취약점이 수십 개 자산에 앵커링될 수 있어, 전부 나열하면 메시지가 터진다.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, "、")
	}
	return strings.Join(assets[:limit], "、") + " 등 " + itoa(len(assets)) + "개"
}

// itoa strconv.Itoa의 짧은 별칭으로, 표시 텍스트 조합에만 쓰며, 여기저기 import strconv하는 것을 피한다.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
