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
		t.Fatalf("마스킹된 값이 자격 증명 전체를 유출함: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("마스킹된 값이 주소 본체를 드러내면 안 됨: %q", got)
	}
	// 끝 6자리를 남겨야 사용자가 어느 봇인지 알아본다.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("끝 6자리를 식별 힌트로 남겨야 함: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("마스킹된 값은 IsMasked가 알아봐야 함: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 짧은 자격 증명까지 끝 6자리를 드러내면 자격 증명 전체를 드러내는 셈이다.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("길이 %d인 자격 증명은 끝부분 힌트를 주면 안 됨, 실제값 %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("마스킹된 값에 원래 값이 들어 있음: %q", got)
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
			t.Errorf("%s은(는) 마스킹되어야 함, 실제값 %q", k, s)
		}
	}
	// 자격 증명이 아닌 필드는 그대로 두어야 한다. 그러지 않으면 UI가 보여 줄 수 없다.
	if masked["port"] != float64(587) {
		t.Errorf("자격 증명이 아닌 필드 port를 바꾸면 안 됨: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// 알림 채널 유형을 알아보지 못하면 UI가 빈 설정을 보이게 하고, 자격 증명이 들어 있을 수 있는 원래 내용을 돌려주지 않는다.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("모르는 알림 채널 유형은 빈 설정을 돌려줘야 함, 실제값 %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// 마스킹은 표시 계층의 동작이라 거꾸로 DB의 실제 값을 바꾸면 안 된다.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig가 입력 인자를 바꿈. 실제 자격 증명이 마스킹된 값으로 덮어써진다")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// 사용자는 method만 바꿨고, 브라우저는 마스킹된 값 + 새 method를 제출한다.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("마스킹된 필드는 DB의 원래 값을 유지해야 함, 실제값 %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("바꾼 필드가 적용되어야 함, 실제값 %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("빈 문자열은 그 필드를 비워야 함, 실제값 %v", got)
	}
	// 언급하지 않은 필드는 유지한다(부분 갱신 의미).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("언급하지 않은 필드는 유지해야 함, 실제값 %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("언급하지 않은 필드는 유지해야 함, 실제값 %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("언급한 필드는 갱신해야 함, 실제값 %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap 은 이 패키지에서 가장 중요한 보안 불변식이다.
// **대상 주소를 바꾸면서 기존 자격 증명을 가져가면 안 된다**.
//
// 이 테스트는 바로 공격 형태의 입력(주소만 바꾸고 자격 증명은 언급하지 않음)을 쓴다.
// '방어 로직의 올바른 입력'만 테스트하면 방어가 작동하지 않아도 모두 통과한다.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing 은 이름이 불려야 할 자격 증명 키다.
		wantMissing string
	}{
		{
			name: "범용 Webhook 주소를 바꾸고 Authorization 헤더를 계속 쓰려 함",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram base_url을 바꿔 Bot Token을 자기 엔드포인트로 보내려 함",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "이메일 SMTP 호스트를 바꿔 비밀번호를 넘기려 함",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "이메일 TLS를 꺼도 비밀번호를 다시 입력해야 함",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// 마스킹된 값 = '기존 자격 증명 계속 쓰기'라서 주소 변경 상황에서는 마찬가지로 거부해야 한다.
			name:        "마스킹된 자격 증명 돌려보내기 + 새 주소",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk Webhook을 바꾸고 서명 키를 계속 쓰려 함",
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
				t.Fatalf("주소를 바꾸고 자격 증명을 다시 입력하지 않았으면 거부해야 함. 실제 설정 %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("API가 고칠 방법을 안내할 수 있게 전용 오류 타입을 돌려줘야 함, 실제값 %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("빠진 자격 증명 키 %q의 이름을 밝혀야 함, 실제값 %v", tc.wantMissing, target.Missing)
			}
			// 오류 메시지가 조작하는 사람에게 고칠 방법을 알려 줘야 한다.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("오류 메시지에 %q이(가) 있어야 함: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits 는 반대 방향 테스트다. 정상 편집을 잘못 막으면 안 된다.
// 그러지 않으면 이 방어가 '너무 귀찮아서' 우회되거나 지워진다.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "이름만 바꿈(설정은 그대로 돌려보냄)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "요청 메서드만 바꾸고 주소와 자격 증명은 그대로",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "주소를 바꾸면서 **함께** 새 자격 증명을 줌",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "주소를 바꾸고 자격 증명이 더는 필요 없다고 명시",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram chat_id 변경(목적지가 아님)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "이메일 받는 사람 변경(목적지가 아님)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("정상 편집을 잘못 막음: %v", err)
			}
			if merged == nil {
				t.Fatal("병합 결과를 돌려줘야 함")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance 는 잘못 판정하기 쉬운 세부 사항을 다룬다.
// 프런트가 제출한 포트는 JSON number(float64)이고 DB에서 읽은 것도 float64지만,
// 두 값의 타입이 다를 수 있다(int vs float64 등). == 로 비교하면 '바뀌지 않음'을 '바뀜'으로 판정해,
// 이름만 바꾼 사용자에게 '비밀번호를 다시 입력하세요'가 뜬다. 거짓 경고는 사람들이 이 방어를 믿지 않게 만든다.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 같은 포트를 int 형태로 제출한다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("포트 값이 같고 타입만 다르면 주소 변경으로 판정하면 안 됨: %v", err)
	}
	// 포트를 정말 바꿨으면 막아야 한다.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("포트 변경은 막혀야 함")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination 은 '비워 둘 수 있는
// 목적지 필드' 경로를 다룬다. Telegram의 base_url을 비워 두면 공식 API 주소를 쓴다.
//
// 예전에는 이 경로 때문에 알림 채널이 두 번째 저장부터 영원히 저장에 실패했다.
//
//	만들 때 DB에 base_url:""을 저장(생성 경로는 프런트가 제출한 config를 그대로 저장하고 MergeConfig를 거치지 않음)
//	→ 첫 저장에서 MergeConfig가 빈 문자열을 명시적 비우기로 보고 그 키를 delete
//	→ 두 번째 저장에서 incoming은 여전히 ""인데 stored에는 이 키가 없어 '주소가 바뀜'으로 판정
//	→ bot_token은 마스킹되어 돌아온 값 → 400 '대상 주소가 바뀌었습니다. 자격 증명 필드도 다시 입력하세요'
//
// 사용자는 아무것도 바꾸지 않았는데 Bot Token을 다시 붙여 넣지 않는 한 그 뒤로 저장할 수 없었다.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// 프런트 buildConfig()는 이 알림 채널의 필드 정의마다 값을 하나씩 제출한다. 자격 증명은 마스킹된 값을 다시 채우고,
	// 빈 텍스트 상자는 빈 문자열을 제출한다. 여기서는 '바뀐 키'만이 아니라 그 출력을 그대로 재현한다.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 첫 저장: 알림 채널 이름만 바꾸고 config는 그대로 돌려보낸다.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("첫 저장을 잘못 막음: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("전제가 바뀜: 빈 문자열은 MergeConfig가 지워야 한다. 이 테스트가 다루려는 것이 바로 '키가 사라진 뒤' 단계다")
	}

	// 두 번째 저장: 제출 내용이 지난번과 같고 사용자는 아무것도 바꾸지 않았다.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("두 번째 저장을 잘못 막음(사용자는 아무것도 바꾸지 않음): %v", err)
	}
	// 세 번째: '한 번만 틀리는' 것이 아니라 계속 저장할 수 있는지 확인한다.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("세 번째 저장을 잘못 막음: %v", err)
	}
	// 자격 증명은 끝까지 유지되어야 하고, 빈 문자열 로직에 함께 지워지면 안 된다.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token은 원래 값을 계속 써야 함, 실제값 %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges 는 앞 테스트의 짝이 되는
// 단언이다. 빈 문자열과 '키 없음'을 같게 보더라도 실제 주소 변경까지 함께 통과시키면 **안 된다**.
// 두 방향 모두 실제 자격 증명 유출 경로다. Telegram의 Bot Token은 URL 경로에 실리므로,
// base_url을 바꾸면 Token을 새 주소에 보내는 셈이다.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 방향 1: '비어 있음'(공식 주소)에서 자체 구축 주소로 바꾼다.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("공식 주소에서 자체 구축 주소로 바꾸면 Token을 다시 입력하도록 요구해야 함")
	}

	// 방향 2: 자체 구축 주소를 비우는 것(= 공식 API로 되돌리기)도 주소 변경이다.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("자체 구축 주소를 비우는 것(공식 API로 되돌리기)도 주소 변경이라 Token을 다시 입력하도록 요구해야 함")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys 와 같은 이유로, 알림 채널이 목적지 키 선언을 잊으면 PrepareConfigUpdate가 그 채널을 지키지 못한다.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("알림 채널 %s이(가) 목적지 키를 선언하지 않아, 주소를 바꿔 자격 증명을 빼내는 것에 대한 방어가 듣지 않음", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("알림 채널 %s이(가) 자격 증명 키를 선언하지 않음", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// 컴파일러가 이미 모든 알림 채널에 SecretKeys 구현을 강제하지만, 여기서 '마스킹을 빈칸으로 낸
	// 알림 채널이 없는지' 한 번 더 확인한다. 빈 슬라이스를 돌려주는 알림 채널은 자격 증명이 평문으로 브라우저에 돌아간다는 뜻이다.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("알림 채널 %s의 마스킹 기대값이 테스트에 등록되지 않음", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("알림 채널 %s이(가) 자격 증명 필드를 하나도 선언하지 않아 설정이 평문으로 돌아감", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer 는 감사에서 지적된 틈 하나를 다룬다.
// 마스킹 센티넬을 **문자열이 아닌** 구조(webhook.headers는 객체다) 안에 넣으면,
// MergeConfig는 '문자열이면서 접두사가 있는' 것만 마스킹으로 알아보므로 리터럴 "__masked__"가
// 실제 헤더 값으로 DB에 저장된다. 이후 인증이 아무 오류 없이 소리 없이 무력해진다.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// 객체 안에 마스킹 센티넬을 끼워 넣는다.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("구조체 안에 마스킹 센티넬을 끼워 넣으면 거부해야 함(그러지 않으면 리터럴이 DB에 저장됨)")
	}
	// 객체 전체를 제출하면(실제 새 값) 그대로 받아들인다.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("새 요청 헤더를 정상 제출하면 막으면 안 됨: %v", err)
	}
}
