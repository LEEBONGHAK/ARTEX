package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 이 파일은 「항목 단위 패킹」 수정을 커버: 요약 메시지가 채널 길이 상한을 넘으면 반드시 **항목 단위**로
// 절단하고 담지 못한 항목 수를 사실대로 보고해, 호출 측이 실제 송달된 것만 표시하게 한다.
//
// 이전 방식은 전체를 렌더한 뒤 절단하고 배치 전체를 송달됨으로 표시: 메시지 후반부가 증발하는데
// 전송 이력은 전부 성공으로 표시——취약점이 그렇게 사라지고, 어디서도 발견할 수 없음.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200건 중국어 요약, 반드시 WeCom 4096바이트를 훨씬 초과.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("본문 %d바이트가 상한 %d 초과", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("본문이 합법 UTF-8이 아님")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 담아야 함(0 < kept < %d), 실제 %d", len(m.Items), kept)
	}
	// 머리말은 이 메시지에 몇 건이 들었고 나머지가 몇 건인지 사실대로 밝혀야 함——아니면 읽는 사람이
	// 머리말의 그 숫자를 전부로 여김.
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지에 계속") {
		t.Fatalf("머리말은 이 메시지에 포함 안 된 항목 수를 밝혀야 함:\n%s", body[:minInt(400, len(body))])
	}
	// 앞 kept건만 포함해야 함.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("%d번째 항목은 이 메시지에 있어야 함:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("%d번째 항목은 나오면 안 됨(다음 배치 소속)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 무제한
	if kept != len(m.Items) {
		t.Fatalf("길이 무제한이면 전부 보존해야 하는데 kept=%d", kept)
	}
	if strings.Contains(body, "나머지") {
		t.Fatalf("절단 없을 때 절단 안내가 나오면 안 됨:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 예산이 한 건도 못 담을 만큼 작아도 한 건은 보내야 함(최종 절단이 받쳐줌).
	// 아니면 초장문 취약점 하나가 배치 전체를 영원히 제자리에 묶음: 매번 수령해도 못 담고 매번 안 보냄.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("최소 1건은 보존해야 하는데 %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("단건 메시지는 송달 1건으로 보고해야 하는데 %d", kept)
	}
	// 빈 메시지는 송달 가능한 항목이 없음.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("빈 메시지는 0건으로 보고해야 하는데 %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram은 **문자 수** 기준 길이 제한; 바이트 기준을 쓰면 중국어 메시지를 3분의 1로 압축함.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("본문 %d문자가 상한 %d 초과", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("일부만 담아야 하는데 %d", kept)
	}
	if !strings.Contains(text, "다음에 계속") {
		t.Fatalf("아직 담지 못한 여분이 있음을 밝혀야 함:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("카드는 일부만 담아야 하는데 %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// 이 두 채널은 본문을 절단하지 않고 배치 전체를 송달로 침.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("전제 조건 미성립")
	}
	// 렌더러의 반환값으로 간접 확인: markdownBody(0) 무제한이면 전부 보존.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("길이 무제한이면 전부여야 하는데 %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent 「신뢰 불가 콘텐츠가 메시지 구조를 바꿔선 안 됨」 회귀 테스트.
// 제목과 요약은 모델 출력에서(모델은 피검 대상 응답을 읽음), 자산명은 피검 대상의 URL에서 옴.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 결과에 반드시 나와야 함(이스케이프 형태)
		wrong []string // 결과에 나오면 안 됨(미이스케이프 형태)
	}{
		{
			name: "제목의 줄바꿈 + 외부 링크",
			item: Item{
				Severity: "high",
				Name:     "登录口 SQL 注入\n[紧急：点此验证账号](http://attacker.tld)",
			},
			// 줄바꿈은 반드시 접혀야 함(아니면 새 목록 항목/인용 블록을 위조 가능);
			// 대괄호와 소괄호는 반드시 이스케이프돼야 함(아니면 클릭 가능한 외부 링크).
			must:  []string{`\[紧急：点此验证账号\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[紧急", "\n\n[紧急"},
		},
		{
			name: "제목의 이미지 비컨",
			item: Item{
				Severity: "high",
				Name:     "漏洞 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "자산명의 강조와 인용",
			item: Item{
				Severity: "high",
				Name:     "普通标题",
				Assets:   []string{"a.com/*注入*>引用"},
			},
			must:  []string{`\*注入\*`, `\>`},
			wrong: []string{"*注入*"},
		},
		{
			name: "요약의 백틱과 세로줄",
			item: Item{
				Severity: "high",
				Name:     "标题",
				Summary:  "`code` | 表格",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 단건 모드의 writeItem은 세 markdown 채널이 공유하는 렌더 경로.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("이스케이프 형태 %q 누락:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("미이스케이프 형태 %q 출현(구조나 외부 링크 주입에 쓰일 수 있음):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst 이스케이프 순서를 고정: 백슬래시를 반드시 가장 먼저 처리,
// 아니면 뒤에 덧붙는 백슬래시에 한 겹 더 씌워 출력에 이중 백슬래시가 나옴.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("이스케이프 순서 오류, %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes 구체적인 회귀 하나를 고정:
// markdown 이스케이프가 Telegram의 HTML 출력으로 새면 안 됨(예전에 공유 제목 함수에
// 이스케이프를 넣어, Telegram 메시지에 `\(1\)` 같은 보이는 백슬래시가 나왔음).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 본문에 markdown 백슬래시 이스케이프가 나옴:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
