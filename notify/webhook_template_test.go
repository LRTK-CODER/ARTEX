package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 이 파일은 범용 웹훅 템플릿의 **능력 경계**를 고정한다.
//
// 이 패키지에서 '사용자가 준 문자열을 코드로 평가하는' 유일한 곳이라, 무엇을 할 수 있고
// 무엇을 할 수 없는지 분명히 하고 테스트로 그 성질을 고정한다. 그러지 않으면 나중에 누가 무심코 템플릿 컨텍스트에
// 메서드를 더하거나 FuncMap에 readFile을 더해도 능력의 범위가 소리 없이 넓어지고, diff로는
// 무해한 작은 함수 하나로만 보인다.

// TestTemplateContextHasNoMethods 가 가장 중요하다.
//
// text/template는 내보낸 메서드를 호출한다({{.Foo}}는 필드도 읽고 메서드도 부른다). 그래서 템플릿 컨텍스트가
// 내보낸 메서드를 가진 **어떤** 타입에라도 닿으면 그 메서드를 템플릿 작성자에게 드러내는 셈이다.
// 이 기능의 컨텍스트는 일부러 순수 데이터만 둔다(내보낸 필드만 있고 메서드는 없다).
//
// 이것이 실패하면 누가 webhookTemplateData / webhookItem에 메서드를 더한 것이다.
// 허용하기 전에 그 메서드로 템플릿이 드러내고 싶지 않은 것을 읽을 수 있는지 먼저 따져 본다.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s이(가) 메서드 %d개(%s)를 드러냄: text/template가 이것을 호출할 수 있어 "+
				"이 메서드의 능력을 템플릿 작성자에게 여는 셈이다", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal 은 템플릿에 드러내는 함수 집합을 고정한다.
//
// FuncMap에 함수가 하나 늘 때마다 능력이 하나 는다. 지금은 json / jsons뿐이고, 값을 JSON 조각으로
// 직렬화하는 일만 한다. 파일을 읽거나, 요청을 보내거나, 명령을 실행할 수 없다.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("템플릿 함수 집합이 바뀜: 실제값 %v, 기대값 %v. 함수를 더하기 전에 능력의 범위를 넓히지 않는지 확인하세요"+
			"(파일 읽기·쓰기, 네트워크 요청, 명령 실행이 불가해야 함)", got, want)
	}
}

// TestTemplateCannotReachUnknownData 는 템플릿의 범위 밖 접근을 다룬다.
// 없는 것에 접근하면 무엇이든 돌려주지 말고 실패해야 하며, 실패 메시지에 내부 데이터가 실려 나가면 안 된다.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("없는 필드에 접근하면 오류여야 함")
	}
	// 오류에 템플릿 컨텍스트의 실제 내용(취약점 제목/요약)이 나오면 안 된다.
	for _, leak := range []string{"SQL 인젝션", "파라미터 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("템플릿 오류가 메시지 내용 %q을(를) 유출함: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently 템플릿을 잘못 쓴 것은 설정 오류라 재시도해도 저절로 낫지 않는다.
// 재시도 가능으로 판정하면 잘못된 템플릿 하나 때문에 전달할 때마다 백오프 세 번을 헛돈다.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("템플릿 문법 오류는 저장할 때 막혀야 함")
	}
	// 검사를 건너뛰고 바로 전달하더라도 계속 재시도하지 말고 영구 실패로 판정해야 한다.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("잘못된 템플릿은 영구 실패여야 함, 실제값 %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON 은 '템플릿 렌더링 결과는 올바른 JSON이어야 한다'는 제약을 다룬다.
// 이것은 '템플릿으로 평문을 만들어 다른 프로토콜을 트리거하는' 쓰임새도 함께 막는다.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 올바른 템플릿은 통과한다.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("올바른 템플릿은 검사를 통과해야 함: %v", err)
	}
	// JSON이 아닌 것을 렌더링하면 그대로 보내지 말고 거부해야 한다.
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON이 아닌 것을 렌더링하면 영구 실패여야 함, 실제값 %v", err)
	}
	if !strings.Contains(err.Error(), "올바른 JSON") {
		t.Errorf("오류 메시지가 JSON 문제라고 밝혀야 함, 실제값 %v", err)
	}
}
