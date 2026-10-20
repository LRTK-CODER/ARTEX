package agent

import (
	"os"
	"testing"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/norma/llm"
)

// TestFromEnvUsesValuesTakenAtStartup 는 시작할 때 비밀 환경 변수를 지운 뒤에도 LLM 설정이
// 읽어 둔 값으로 만들어지는지 확인한다.
func TestFromEnvUsesValuesTakenAtStartup(t *testing.T) {
	// TakeSecretEnv는 실제 비밀 환경 변수도 지우므로 먼저 t.Setenv로 걸어 테스트 뒤 되돌린다.
	for name, value := range config.ReadSecretEnv() {
		t.Setenv(name, value)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-fake-openai-key")
	t.Setenv("ARTEX_LLM_PROVIDER", "openai")
	t.Setenv("ARTEX_LLM_MODEL", "gpt-fake")
	t.Setenv("ARTEX_LLM_PROXY", "http://proxy.test:3128")
	t.Setenv("ARTEX_LLM_STREAM", "false")

	secrets := config.TakeSecretEnv()

	if _, ok := FromEnv(os.Getenv); ok {
		t.Fatal("지운 뒤 프로세스 환경에서 LLM 설정이 만들어졌다")
	}
	cfg, ok := FromEnv(secrets.Get)
	if !ok {
		t.Fatal("읽어 둔 값으로 LLM 설정을 만들지 못했다")
	}
	want := Config{Format: llm.FormatOpenAI, APIKey: "sk-fake-openai-key", Model: "gpt-fake", Proxy: "http://proxy.test:3128", Stream: false}
	if cfg.Format != want.Format || cfg.APIKey != want.APIKey || cfg.Model != want.Model || cfg.Proxy != want.Proxy || cfg.Stream != want.Stream {
		t.Fatalf("FromEnv(읽어 둔 값) = %+v, want %+v", cfg, want)
	}
}
