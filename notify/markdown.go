package notify

import (
	"fmt"
	"strings"
)

// 이 파일은「Markdown 계열」채널（딩톡、위챗워크）이 공용으로 쓰는 메시지 렌더링이다.
// 페이슈는 카드 JSON、Telegram은 HTML、메일은 HTML을 쓰며, 각자 어댑터에서 렌더링한다.

// maxAssetsShown 메시지에 최대 몇 개의 자산을 나열할지다. 한 취약점이 수십 개 자산에 앵커링될 수 있어,
// 전부 나열하면 메시지가 터지고 정보 가치도 없다——4번째 이후의 도메인은 아무도 IM에서 보지 않는다.
const maxAssetsShown = 3

// maxSummaryRunes 요약을 몇 글자로 압축할지다. IM 메시지는「상세를 보러 가라는 안내」이지,
// 리포트 본체가 아니며, 완전한 내용은 플랫폼에 있다.
const maxSummaryRunes = 120

// markdownReservedBytes 메시지 헤더（요약 행 + 레벨 분포 + 가능한 절단 안내）에 예약해 두는 것과
// 푸터（플랫폼 링크）에 예약한다. 한 건 통째로 패킹할 때 이 부분을 예산에서 빼, 머리와 꼬리가 잘리지 않도록 보장한다——
// 머리와 꼬리가 일단 잘리면, 읽는 사람은「이게 어느 배치인지、표시 안 된 게 몇 건인지」조차 알 수 없다.
const markdownReservedBytes = 320

// markdownEscape markdown 메타 문자를 이스케이프한다.
//
// 왜 반드시 해야 하나:취약점 제목、요약、유형、자산 표시 이름이 전부 **신뢰할 수 없는 출처**에서 온다——**——
// 제목과 요약은 모델 출력에서 나오고（모델이 읽는 것은 피검 대상의 응답），자산의 url은 스캔으로
// 얻은 완전한 URL이다（대상이 제어 가능한 쿼리 문자열 포함）. 이스케이프하지 않으면, 제목이
//
//	로그인 창구 SQL 인젝션\\n[긴급:여기를 눌러 계정 인증](http://attacker.tld)
//
// 인 취약점이 보안 엔지니어의 딩톡/페이슈에서 **클릭 가능한 외부 링크**로 렌더링된다；그리고
// `![](http://attacker.tld/beacon)` 는 렌더링 시 클라이언트가 가져가므로, 통보한 셈이다
// 「이 취약점을 이미 봤다」고, 그리고 읽는 사람의 IP를 유출한다. 악의 없는 내용이라도, 주입된 볼드체나
// 인용 블록도 아래의 심각한 취약점을 접힘선 밖으로 밀어낼 수 있다.
//
// 이스케이프 집합은 제목/링크/강조/목록/인용/취소선 이 몇 가지, 구조를 바꾸거나 클릭 가능
// 요소를 만드는 문자를 커버한다. `\\`는 반드시 가장 먼저 처리해야 하고, 아니면 뒤에 덧붙인 백슬래시를 다시 이스케이프한다.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText 신뢰할 수 없는 텍스트를 단일 행으로 압축하고 이스케이프해, markdown 본문에 쓰도록 제공한다.
// 단일 행화는 이스케이프 외의 다른 절반이다:줄바꿈 자체가 새로운 목록 항목이나 인용 블록을 위조할 수 있고,
// 이스케이프 문자는 그것을 막지 못한다.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle 메시지 제목을 반환한다（IM 플랫폼의 제목 바/카드 제목）, 내용은 **이스케이프하지 않은 원문**이다.
//
// 여기서는 의도적으로 이스케이프하지 않는다:이 제목은 네 가지 맥락의 렌더러가 공용으로 쓴다——markdown 본문、Telegram의
// HTML、페이슈 카드의 plain_text、그리고 범용 Webhook의 JSON과 메일 제목이다. 각 맥락의
// 이스케이프 규칙이 다 다르다（markdown 이스케이프를 HTML에 넣으면 보이는 백슬래시가 남고, JSON에 넣으면 오염
// 데이터된다）, 그래서 이스케이프는 각자의 출력단이 책임져야 한다, 참조: writeItem / feishuItemLines /
// telegramEscape. 예전에 공유 함수에 markdown 이스케이프를 추가했더니, 결과적으로 Telegram 메시지에
// `\\(1\\)` 같은 보이는 백슬래시가 나타났다.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("취약점 요약 · 총 %d건", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "취약점 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody 메시지 본문을 렌더링하고, 본문과 **실제로 쓴 항목 수**를 반환한다.
//
// 반환값 kept는 이번 전송에서 실제로 송달된 항목 수이고, 호출 측은 이에 따라 앞 kept건만
// 송달됨으로 표시한다——채널 길이 상한에 밖으로 막힌 항목은 반드시 다음 배치로 남겨야 하고, 함께 따라서
// 성공으로 표시되지 않는다. 이것이 바로「조용한 손실」의 근원이다:메시지는 잘렸는데, 전송 기록은 전부 송달됨으로 표시하고,
// 뒷부분이 한 번도 발송되지 않았음을 알아챌 수 있는 곳이 아무 데도 없다.
//
// maxBytes<=0 제한 없음을 나타낸다.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 단일 메시지는 초과 길이여도 그대로 발송한다（최종 절단이 폴백）:한 취약점의 일부 정보라도
		// 한 건도 안 보내는 것보다는 낫다.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 전체 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro 요약 메시지의 머리말을 렌더링한다:시간 창、건수와 레벨 분포.
// 이것들이 있으면, 요약을 받은 사람은 플랫폼에 들어가지 않고도 이 배치를 즉시 처리해야 하는지 판단할 수 있다.
//
// items **실제로 담은** 항목이고, total은 이 배치에 있어야 할 총수다. 둘이 다를 때는 반드시 명시해야 한다
// 「몇 건이 다음 메시지에 더 있는지」——아니면 읽는 사람은 메시지 머리에 쓰인 그 숫자가 전부라고 여기고,
// 뒤의 그 한 번도 발송되지 않은 항목들은 화면에 전혀 존재하지 않는다.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d분간 신규 %d개 취약점**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**신규 %d개 취약점**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "（이 메시지는 앞 %d건 표시, 나머지 %d건은 다음 메시지에 계속）", len(items), extra)
	}
	// 레벨별로 분포를 제시해, 읽는 사람이 심각 항목이 있는지 한눈에 보게 한다. **이 건이 실제로 포함한** 것만 집계하는
	// 항목이어야,「심각 3」과 아래에서 셀 수 있는 항목이 일치함을 보장한다.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem 단일 취약점 항목을 렌더링한다.
//
// prefix 요약 목록의 순번에 쓴다；single=true일 때 완전판을 렌더링한다（요약과 백링크 포함）,
// 요약 목록에서는 요약 한 줄만 렌더링한다——아니면 50건 요약이 한 편의 긴 문서가 된다.
//
// 외부에서 온 모든 내용（제목/유형/자산/요약）은 markdownText를 거친다:
// 단일 행화 + 이스케이프. 백링크는 관리자가 설정한 public_base_url로 조합한 것이라, 신뢰할 수 없는 내용이 아니고,
// 클릭 가능한 링크여야 하므로, 원본 그대로 출력한다.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 요약 모드:단일 행으로 표시하고, 자산과 요약을 압축해 뒤에 붙인다.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**:%s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**:%s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**:%s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**요약**:%s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[상세 보기](%s)\n", it.DetailURL)
	}
}
