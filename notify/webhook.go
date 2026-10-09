package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel 은 범용 웹훅 어댑터다. 사용자가 URL, 메서드, 요청 헤더, JSON 템플릿을 정한다.
// 이것이 있어서 Slack / Mattermost / Discord / 자체 시스템마다 구현을 따로 쓰지 않아도 된다.
// 그런 플랫폼은 설정할 수 있는 템플릿 하나로 다룰 수 있다.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 범용 웹훅에는 공식 제한이 없다. 0을 돌려줘 기본값은 속도 제한 없음이고, 사용자가 상대의 처리 능력에 맞춰 정한다.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url과 headers를 마스킹한다. 대상 주소에는 token이 들어 있는 경우가 많고, 사용자 지정 헤더에는 보통 인증 자격 증명이 있으며,
// 둘 다 API가 돌려주는 값에 나타나므로 모두 가려야 한다.
// 대가로, 편집할 때 헤더 하나를 바꾸려면 헤더 묶음 전체를 다시 입력해야 한다(마스킹된 값은 '원래 값 유지'로 해석된다).
// 일부러 이렇게 정했다. 한 번 더 입력하더라도 자격 증명을 브라우저로 돌려보내지 않는다.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 목적지는 url이다. url을 바꿀 때는 headers를 다시 입력해야 한다. 그러지 않으면 원래 Authorization 헤더가
// 그대로 새 주소로 보내진다. 이것이 마스킹을 우회하는 주된 경로다.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate 은 템플릿을 입력하지 않았을 때 쓰는 기본 요청 본문이다. 단순한 JSON 구조로,
// 'JSON 한 건을 받아 저장하는' 대부분의 자체 수신 측을 다룬다.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData 는 사용자 템플릿에 드러나는 컨텍스트다.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt 은 이번 전달 시각(RFC3339)이다. 수신 측이 기록하도록 준다.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel 은 상태 변경을 읽기 쉽게 적은 것이다(예: '처리 대기 → 수정됨'). 상태 변경이 아니면 비어 있다.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("대상 URL을 입력하세요")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("잘못된 대상 URL입니다: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("지원하지 않는 메서드 %s입니다(GET/POST/PUT/PATCH 사용 가능)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("요청 본문 템플릿 문법 오류: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET은 요청 본문을 싣지 않는다. 내용을 query에 넣는 것은 템플릿 능력 밖이고 GET 의미에도 맞지 않는다.
	// 그래서 GET은 '일치하면 훅을 트리거하는' 종류의 수신 측에만 맞다.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// 템플릿이 렌더링한 것은 문자열 형태의 JSON이다. 여기서 json.RawMessage로 바꿔 그대로 보내,
		// 두 번 이스케이프해서 사용자가 공들여 만든 구조가 JSON 문자열 안에 감싸이지 않게 한다.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("요청 본문 템플릿 렌더링 결과가 올바른 JSON이 아닙니다"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 덮어쓰기를 허용하되 headers 뒤에 적용해 명시한 설정이 우선하게 한다.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 범용 웹훅은 본문을 자르지 않는다(수신 측은 사용자 자신의 서비스이고 크기는 body_template가 정한다).
	// 그래서 배치 전체를 전달됨으로 친다.
	return len(m.Items), nil
}

// renderWebhookBody 는 사용자 템플릿(또는 기본 템플릿)으로 요청 본문을 렌더링한다.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 문법 오류: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("요청 본문 템플릿 렌더링 실패: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate 은 템플릿을 파싱한다.
//
// missingkey=zero는 없는 map 키를 오류 대신 제로값으로 렌더링한다. 다만 이 파일의 컨텍스트는 구조체라서
// 주된 효과는 .Items가 비어 있을 때 range가 오류를 내지 않게 하는 것이다. 실제로 막아야 할 것은 .Items가 nil인 경우다.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs 는 템플릿에 드러내는 도우미 함수다.
var webhookTemplateFuncs = template.FuncMap{
	// json은 임의의 값을 JSON으로 직렬화한다.
	//
	// 이 함수는 있으면 좋은 정도가 아니라 꼭 필요하다. 없으면 사용자는 {{.Title}}로 바로 끼워 넣을 수밖에 없는데,
	// 취약점 제목에 따옴표나 줄바꿈이 하나만 있어도 요청 본문 전체가 올바른 JSON이 아니게 된다. 수신 측은
	// 거부하고 오류 메시지는 'JSON 파싱 실패'를 가리켜, 제목에 따옴표가 있다는 것을 전혀 떠올릴 수 없다.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons는 JSON 조각을 다른 JSON 문자열 값 안에 넣을 때 쓴다(문자열 이스케이프를 한 번 더 한다).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 바깥 따옴표를 뗀다. 따옴표를 붙일지는 호출자가 정한다.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
