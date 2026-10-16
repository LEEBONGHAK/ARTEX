package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 이 파일은 범용 Webhook 템플릿의 **능력 경계**를 고정한다.
//
// 이곳은 패키지 전체에서 유일하게 「사용자가 제공한 문자열이 코드로 평가되는」 곳이라, 무엇을 할 수 있고
// 없는지 명확히 하고 이 성질을 테스트로 고정해야 함——아니면 나중에 누군가 템플릿 컨텍스트에
// 메서드를 슬쩍 추가하거나 FuncMap에 readFile을 넣으면 능력 면이 조용히 확대되는데, diff는
// 무해한 작은 함수 하나로만 보임.

// TestTemplateContextHasNoMethods 가장 중요한 항목.
//
// text/template은 내보낸 메서드를 호출함({{.Foo}}는 필드도 메서드도 가져옴). 그래서 템플릿 컨텍스트가
// 내보낸 메서드를 가진 **어떤** 타입이든 접촉할 수 있으면, 그 메서드를 템플릿 작성자에게 노출하는 셈.
// 이 기능의 컨텍스트는 의도적으로 전부 순수 데이터(내보낸 필드만, 메서드 0개).
//
// 이게 실패하면: 누군가 webhookTemplateData / webhookItem에 메서드를 추가했다는 뜻.
// 통과시키기 전에, 그 메서드가 노출하고 싶지 않은 것을 템플릿으로 읽는 데 쓰일 수 있는지 먼저 따져볼 것.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s가 메서드 %d개(%s)를 노출: text/template이 호출할 수 있어, "+
				"이 메서드들의 능력을 템플릿 작성자에게 개방하는 셈", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal 템플릿에 노출되는 함수 집합을 고정.
//
// FuncMap에 함수가 하나 늘 때마다 능력도 하나 는다. 현재는 json / jsons만 있고, 값을 JSON 조각으로
// 직렬화하는 역할——파일 읽기 불가、요청 전송 불가、명령 실행 불가.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("템플릿 함수 집합이 바뀜: 실제 %v, 기대 %v. 함수 추가 전 능력 면을 확대하지 않는지 확인할 것"+
			"(파일 읽기/쓰기 불가、네트워크 요청 불가、명령 실행 불가)", got, want)
	}
}

// TestTemplateCannotReachUnknownData 템플릿의 범위 밖 접근을 커버:
// 존재하지 않는 것에 접근하면 반드시 실패해야지 뭔가를 되비쳐선 안 됨; 실패 정보가 내부 데이터를 흘려선 안 됨.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("존재하지 않는 필드 접근은 에러여야 함")
	}
	// 에러에 템플릿 컨텍스트의 실제 내용(취약점 제목/요약)이 나와선 안 됨.
	for _, leak := range []string{"SQL注入", "参数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("템플릿 에러가 메시지 내용 %q를 누출: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently 템플릿 오작성은 구성 오류라 재시도로 자연 복구 안 됨.
// 재시도 가능으로 판정되면, 나쁜 템플릿 하나가 매 전송마다 세 번의 백오프를 헛돌게 함.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("템플릿 문법 오류는 저장 시점에 막혀야 함")
	}
	// 검증을 우회해 직접 전송해도 반복 재시도가 아니라 영구 실패로 판정해야 함.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("나쁜 템플릿은 영구 실패로 판정해야 하는데 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON 「템플릿 렌더 결과는 반드시 유효한 JSON」 제약을 커버.
// 덤으로 「템플릿으로 순수 텍스트를 생성해 다른 프로토콜을 트리거」하는 류의 사용을 막는다.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 합법 템플릿은 통과.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("합법 템플릿은 검증을 통과해야 함: %v", err)
	}
	// 비 JSON으로 렌더되면 반드시 거부(원문 그대로 보내지 않음).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("비 JSON 렌더는 영구 실패로 판정해야 하는데 %v", err)
	}
	if !strings.Contains(err.Error(), "유효하지 않은 JSON") {
		t.Errorf("에러 메시지는 JSON 문제임을 설명해야 하는데 %v", err)
	}
}
