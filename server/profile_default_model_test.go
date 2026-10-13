package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 모델을 비운 API 키 프로필도 실제 호출 설정(profileConfig)에는 형식별 기본 모델이 들어가야 한다.
// 화면에 보이는 기본값(profileDefaultModel)도 같은 값이어야 사용자가 본 것과 실제 요청이 어긋나지 않는다.
func TestProfileConfigEmptyModelUsesFormatDefault(t *testing.T) {
	cases := []struct {
		name   string
		format string
		model  string
		want   string
	}{
		{"anthropic 빈 모델", "anthropic", "", "claude-opus-4-8"},
		{"openai 빈 모델", "openai", "", "gpt-4o"},
		{"openai-responses 빈 모델", "openai-responses", "", "gpt-5"},
		{"형식 없음은 anthropic", "", "", "claude-opus-4-8"},
		{"공백만 있는 모델", "openai", "   ", "gpt-4o"},
		{"지정한 모델은 그대로", "openai", "gpt-test-explicit", "gpt-test-explicit"},
	}
	s := &Server{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &db.LLMProfile{Format: tc.format, Model: tc.model, APIKey: "fake-key", AuthType: db.AuthAPIKey, Streaming: true}
			cfg, ok := s.profileConfig(p)
			if !ok {
				t.Fatal("키가 있는 API 키 프로필인데 설정을 만들지 못했다")
			}
			if cfg.Model != tc.want {
				t.Fatalf("cfg.Model=%q, want %q", cfg.Model, tc.want)
			}
			if _, err := cfg.NewProvider(); err != nil {
				t.Fatalf("provider 생성 실패: %v", err)
			}
			if tc.model == "" {
				if got := profileDefaultModel(p); got != tc.want {
					t.Fatalf("profileDefaultModel=%q, want %q", got, tc.want)
				}
			}
		})
	}
}

// 구독 프로필의 기본 모델은 저장된 형식이 아니라 제공자 형식으로 정해진다.
func TestProfileDefaultModelFollowsSubscriptionFormat(t *testing.T) {
	cases := []struct {
		name     string
		authType db.AuthType
		format   string
		want     string
	}{
		{"Claude 구독은 저장 형식과 무관하게 anthropic", db.AuthClaudeOAuth, "openai", "claude-opus-4-8"},
		{"ChatGPT 구독은 저장 형식과 무관하게 openai-responses", db.AuthChatGPTOAuth, "anthropic", "gpt-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &db.LLMProfile{Format: tc.format, AuthType: tc.authType}
			if got := profileDefaultModel(p); got != tc.want {
				t.Fatalf("profileDefaultModel=%q, want %q", got, tc.want)
			}
			if got := llmProfileDTO(p, nil).DefaultModel; got != tc.want {
				t.Fatalf("DTO default_model=%q, want %q", got, tc.want)
			}
		})
	}
}

// 모델 없이 저장하고 로그인한 구독 프로필도 실제 호출 설정에 제공자 기본 모델이 들어간다.
// 실제 PostgreSQL이 필요하고(newOAuthFixture), 저장된 가짜 자격 증명만 쓴다. 요청은 보내지 않는다.
func TestProfileConfigEmptyModelSubscription(t *testing.T) {
	cases := []struct {
		name     string
		authType db.AuthType
		want     string
	}{
		{"ChatGPT 구독", db.AuthChatGPTOAuth, "gpt-5"},
		{"Claude 구독", db.AuthClaudeOAuth, "claude-opus-4-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOAuthFixture(t, nil)
			f.profile.AuthType = tc.authType
			f.profile.Model = ""
			if _, err := f.pg.SaveProfile(f.profile); err != nil {
				t.Fatal(err)
			}
			stored, err := f.pg.ProfileByID(f.profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Model != "" {
				t.Fatalf("빈 모델로 저장되지 않았다: %q", stored.Model)
			}
			f.connect(t, time.Now().Add(time.Hour))
			cfg, ok := f.s.profileConfig(stored)
			if !ok {
				t.Fatal("로그인한 구독 프로필인데 설정을 만들지 못했다")
			}
			if cfg.Model != tc.want {
				t.Fatalf("cfg.Model=%q, want %q", cfg.Model, tc.want)
			}
			if _, err := cfg.NewProvider(); err != nil {
				t.Fatalf("provider 생성 실패: %v", err)
			}
		})
	}
}

// 작업 LLM 해석 응답도 모델을 비운 프로필이면 실제로 쓰는 기본 모델을 default_model로 싣는다.
// 화면 툴팁이 빈 모델 대신 "기본값(<모델>)"을 보이는 근거다. 실제 PostgreSQL이 필요하다.
func TestTaskResolutionCarriesDefaultModel(t *testing.T) {
	f := newOAuthFixture(t, nil)
	f.profile.Model = ""
	if _, err := f.pg.SaveProfile(f.profile); err != nil {
		t.Fatal(err)
	}
	f.connect(t, time.Now().Add(time.Hour))
	p, err := f.pg.ProfileByID(f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := f.s.resolutionFromProfile(p, "agent_binding")
	if !got.Available || got.Model != "" || got.DefaultModel != "gpt-5" {
		t.Fatalf("available=%v model=%q default_model=%q, want true \"\" gpt-5", got.Available, got.Model, got.DefaultModel)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"default_model":"gpt-5"`) {
		t.Fatalf("응답에 default_model이 없다: %s", raw)
	}
}
