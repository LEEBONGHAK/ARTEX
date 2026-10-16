package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// 이 패키지에서 가장 중요한 불변식. WeCom은 **바이트** 기준 길이 제한, 중국어는 3바이트/자라,
	// 바이트로 하드 컷하는 구현은 한자를 반 토막 내 비합법 UTF-8을 만들어 플랫폼이 거부한다.
	// 길이가 서로소인 여러 중·영 혼합 입력으로 가능한 모든 컷 지점을 때린다.
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4바이트 emoji, 잘못 자르면 더 명확
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("입력 %q max=%d: 비합법 UTF-8 산출 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("입력 %q max=%d: 결과 %d바이트가 상한 초과", in, max, len(got))
			}
			// 절단되지 않았을 때 내용을 바꿔서는 안 됨.
			if len(in) <= max && got != in {
				t.Fatalf("입력 %q max=%d: 상한 미초과인데 내용이 바뀜 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0은 무제한을 의미해야 함")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0은 무제한을 의미해야 함")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max가 생략부호 자체보다 작을 때, 생략부호 추가로 오히려 상한을 넘겨선 안 됨.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1일 때 결과 %q 길이 %d 상한 초과", got, len(got))
	}
	// 정상적으로는 생략부호가 붙어야 함.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("생략부호를 기대했으나 %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes와의 기준 차이는 반드시 유지: Telegram은 문자 기준 길이 제한,
	// 바이트 기준을 쓰면 중국어 메시지를 3분의 1만 남게 자름.
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("5개 문자를 기대했으나 %d개 (%q)", n, got)
	}
	// 같은 문자열을 바이트 기준으로 하면 눈에 띄게 더 짧아야 함.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("바이트 기준이 문자 기준과 같은 문자 수를 산출해선 안 됨")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("第一行\n\n第二行\t带制表   多空格", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("모든 공백을 접어야 하는데 %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("연속 공백을 남겨선 안 되는데 %q", got)
	}
	// 절단 후에도 가독·합법이어야 함.
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("4문자를 기대했으나 %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML을 그냥 절단하면 `<a href="htt` 같은 잔편이 생겨 플랫폼이 메시지 전체를 거부함.
	s := `<b>标题</b>正文正文正文<a href="https://example.com/very/long/path">查看详情</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 결과 %d문자 상한 초과", max, n)
		}
		// 꼬리에 닫히지 않은 `<`가 있으면 안 됨(마지막 조각에 `<`는 있고 `>`는 없는 경우).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 꼬리 태그가 잘림 -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("자산 없으면 빈 문자열을 반환해야 하는데 %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a、b" {
		t.Fatalf("상한 미초과면 전부 나열해야 하는데 %q", got)
	}
	// 상한 초과 시 총수를 반드시 표기, 아니면 독자가 나열 안 된 자산 수를 모름.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "등 5개") {
		t.Fatalf("총수 5를 표기해야 하는데 %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("빈 등급 서수는 0, 어떤 임계값에도 막혀야 함")
	}
	if !AtLeast("critical", "") {
		t.Fatal("빈 임계값은 통과시켜야 함")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("알 수 없는 상태 매핑, %q", got)
	}
	// 알 수 없는 상태는 원문 그대로 반환, 라벨을 지어내지 않음.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("알 수 없는 상태는 원문 그대로여야 하는데 %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity 감사에서 지적된 누락 한 곳을 커버: 절단은 반 토막 태그뿐 아니라
// 잘린 HTML 엔티티도 피해야 한다.
//
// `&amp;`가 `&amp`로 잘리면, 엔티티만 인식하는 파서가 **메시지 전체**를 거부할 수 있음——
// 초장문 요약 메시지는 원래 흔해서 대가가 너무 큼.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 꼬리에 「&는 있고 대응하는 ;는 없는」 엔티티 잔편이 나와선 안 됨.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 꼬리에 엔티티 잔편 %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 비정상 엔티티 발생", max)
		}
	}
}
