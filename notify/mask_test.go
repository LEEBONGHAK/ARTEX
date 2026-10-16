package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("마스킹 값이 완전한 자격 증명을 누출: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("마스킹 값이 주소 본체를 노출하면 안 됨: %q", got)
	}
	// 끝 6자리를 보존해야 사용자가 어느 봇인지 알아봄.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("식별 힌트로 끝 6자리를 보존해야 하는데: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("마스킹 값은 IsMasked로 식별 가능해야 하는데: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 짧은 자격 증명이 끝 6자리까지 노출하면 전체 자격 증명을 노출하는 셈.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("길이 %d 자격 증명은 꼬리 힌트를 주면 안 되는데 %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("마스킹 값이 원값을 포함: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s는 마스킹돼야 하는데 %q", k, s)
		}
	}
	// 비자격 증명 필드는 원문 보존해야 함, 아니면 UI가 표시 못 함.
	if masked["port"] != float64(587) {
		t.Errorf("비자격 증명 필드 port는 바뀌면 안 됨: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// 채널 유형을 식별 못 하면 차라리 UI에 빈 구성을 보여줄지언정, 자격 증명이 든 원본 내용을 되뱉지 않음.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("알 수 없는 채널 유형은 빈 구성을 반환해야 하는데 %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// 마스킹은 표시 계층 동작이라, 거꾸로 DB의 실제 값을 바꾸면 안 됨.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig가 입력 파라미터를 수정함, 실제 자격 증명이 마스킹 값으로 덮일 수 있음")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// 사용자가 method만 바꿈, 브라우저는 마스킹 값+새 method를 제출.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("마스킹 필드는 DB 원값을 보존해야 하는데 %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("수정된 필드는 적용돼야 하는데 %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("빈 문자열은 해당 필드를 비워야 하는데 %v", got)
	}
	// 언급 안 된 필드는 보존(부분 업데이트 의미).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("언급 안 된 필드는 보존돼야 하는데 %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("언급 안 된 필드는 보존돼야 하는데 %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("언급된 필드는 업데이트돼야 하는데 %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap 이 패키지에서 가장 중요한 보안 불변식:
// **목표 주소 변경 시 기존 자격 증명을 가져가면 안 됨**.
//
// 이 케이스들은 바로 공격 형태의 입력을 쓴다(주소만 바꾸고 자격 증명은 언급 회피),
// 「방어 로직의 올바른 입력」이 아님——후자만 테스트하면 방어가 작동 안 해도 전부 초록.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing 지목될 것으로 예상되는 자격 증명 키.
		wantMissing string
	}{
		{
			name: "범용 Webhook이 주소를 바꾸며 Authorization 헤더를 사용하려 함",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram이 base_url을 바꿔 Bot Token을 자기 엔드포인트로 보내려 함",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "이메일이 SMTP 호스트를 바꿔 비밀번호를 넘기려 함",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "이메일이 TLS를 꺼도 비밀번호를 다시 표명해야 함",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// 마스킹 값 = 「기존 자격 증명 사용」, 주소 변경 맥락에서도 마찬가지로 거부해야 함.
			name:        "마스킹 자격 증명 회신 + 새 주소",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk이 Webhook을 바꿔 서명 키를 사용하려 함",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("주소를 바꾸고도 자격 증명을 다시 표명하지 않음, 거부돼야 하는데; 구성 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("인터페이스가 실행 가능한 안내를 주도록 전용 에러 타입을 반환해야 하는데 %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("누락된 자격 증명 키 %q를 지목해야 하는데 %v", tc.wantMissing, target.Missing)
			}
			// 에러 메시지는 운영자가 어떻게 고칠지 안내해야 함.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("에러 메시지는 %q를 언급해야 하는데: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits 역방향 케이스: 정상 편집이 오탐 차단되면 안 됨,
// 아니면 이 방어가 「너무 귀찮다」는 이유로 우회되거나 삭제됨.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "이름만 변경(구성 원문 회신)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "요청 메서드만 변경, 주소와 자격 증명은 그대로",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "주소 교체 + **동시에** 새 자격 증명 제공",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "주소 교체 + 자격 증명이 더 이상 필요 없다고 명시 선언",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram chat_id 변경(목적지 아님)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "이메일 수신자 변경(목적지 아님)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("합법 편집이 오탐 차단됨: %v", err)
			}
			if merged == nil {
				t.Fatal("병합 결과를 반환해야 함")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance 오판하기 쉬운 세부를 커버:
// 프런트가 제출하는 포트는 JSON number(float64), DB에서 읽어온 것도 float64지만,
// 두 값의 타입이 다를 수 있음(예: int vs float64). == 비교는 「안 바꿈」을 「바꿈」으로 판정해,
// 이름만 바꾼 사용자에게 「비밀번호를 다시 입력하세요」를 띄움——허위 경보는 이 방어에 대한 신뢰를 잃게 함.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 같은 포트를 int 형태로 제출.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("포트 값이 같으면(타입만 다름) 주소 변경으로 판정하면 안 됨: %v", err)
	}
	// 실제로 포트를 바꿨으면 반드시 막아야 함.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("포트 변경은 막혀야 함")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination 「비울 수 있는
// 목적지 필드」경로를 커버: Telegram의 base_url을 비우면 공식 API 주소를 쓴다는 뜻.
//
// 예전엔 여기서 채널이 두 번째 저장부터 영구 저장 실패했음:
//
//	신규 생성 시 DB에 base_url:""를 저장(생성 경로는 프런트 제출 config를 바로 저장, MergeConfig 안 거침)
//	→ 첫 저장, MergeConfig가 빈 문자열을 명시적 비우기로 보고 해당 키를 delete
//	→ 두 번째 저장, incoming은 여전히 ""인데 stored엔 이미 이 키가 없어 「주소가 바뀜」으로 판정
//	→ bot_token은 마스킹 회신 값 → 400「목표 주소가 변경됨, 자격 증명 필드를 함께 다시 입력하세요」
//
// 사용자는 아무것도 안 바꿨는데 이후로 Bot Token을 다시 붙여넣기 전엔 영영 저장 못 함.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// 프런트 buildConfig()는 이 채널의 모든 필드 정의에 값을 하나씩 제출: 자격 증명은 마스킹으로 채우고,
	// 빈 텍스트 박스는 빈 문자열 제출. 여기서 그 출력을 완전히 재현, 「바뀐 키」만 제출하는 게 아님.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 첫 저장: 채널 이름만 바꿈, config 원문 회신.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("첫 저장이 오탐 차단됨: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("전제가 바뀜: 빈 문자열은 MergeConfig가 삭제해야 함——이 케이스가 커버하려는 게 바로 「키 사라진 뒤」 그 단계")
	}

	// 두 번째 저장: 제출 내용이 지난번과 완전히 동일, 사용자는 아무것도 안 바꿈.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("두 번째 저장이 오탐 차단됨(사용자는 아무것도 안 바꿈): %v", err)
	}
	// 세 번째, 「한 번만 틀린」게 아니라 안정적으로 저장 가능한지 확인.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("세 번째 저장이 오탐 차단됨: %v", err)
	}
	// 자격 증명은 끝까지 보존돼야 하며, 빈 문자열 로직에 함께 지워지면 안 됨.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token은 원값을 사용해야 하는데 %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges 앞 케이스의 짝 단언:
// 빈 문자열과 「키 없음」을 동등하게 보더라도, **진짜 주소 변경을 함께 눈감으면 안 됨**.
// 이 두 방향 모두 실제 자격 증명 외부 전송 경로——Telegram의 Bot Token은 URL 경로에 들어가므로,
// base_url을 바꾸면 Token을 새 주소로 보내는 셈.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 방향 1: 「비움」(공식 주소)에서 자체 구축 주소로 교체.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("공식 주소에서 자체 구축 주소로 교체 시 Token 재입력을 요구해야 함")
	}

	// 방향 2: 자체 구축 주소를 비우는 것(= 공식 API로 복귀)도 마찬가지로 주소 변경.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("자체 구축 주소 비우기(공식 API 복귀)도 마찬가지로 주소 변경이라 Token 재입력을 요구해야 함")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys와 동일: 채널이 목적지 키 선언을 잊으면 PrepareConfigUpdate가 보호하지 못함.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("채널 %s가 목적지 키를 선언 안 함, 주소 변경으로 자격 증명 유출 방어가 무효", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s가 자격 증명 키를 선언 안 함", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// 컴파일러가 이미 모든 채널에 SecretKeys 구현을 강제하지만, 여기서 「마스킹에서 백지
	// 낸 채널이 없음」을 한 번 더 확인——빈 슬라이스를 반환하는 채널은 자격 증명이 평문으로 브라우저에 되비침을 의미.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("채널 %s가 테스트에 마스킹 예상을 등록 안 함", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("채널 %s가 어떤 자격 증명 필드도 선언 안 함, 그 구성이 평문으로 되비침", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer 감사에서 지적된 구멍 하나를 커버:
// 마스킹 센티넬을 **비문자열** 구조(예: webhook.headers는 객체)에 끼워 넣으면,
// MergeConfig는 「문자열이고 접두가 있는 것」만 마스킹으로 인식하므로, 리터럴 "__masked__"가
// 실제 헤더 값으로 DB에 저장됨——이후 인증이 조용히 무력화되고 어떤 에러도 없음.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// 객체 내부에 마스킹 센티넬을 끼워 넣음.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("구조체 내부에 끼운 마스킹 센티넬은 거부돼야 함(아니면 리터럴이 DB에 저장됨)")
	}
	// 객체 전체 제출(실제 새 값)은 평소대로 수락.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("정상적인 새 요청 헤더 제출은 막히면 안 됨: %v", err)
	}
}
