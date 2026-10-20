package config

import (
	"os"
	"strings"
	"testing"
)

// TestTakeSecretEnvRemovesSecretsFromProcessEnv 는 시작 처리 뒤 os.Environ()에 가짜 비밀값이 없고,
// 읽어 둔 값으로 DB DSN을 정하며, 비밀이 아닌 변수는 그대로 남는지 확인한다.
func TestTakeSecretEnvRemovesSecretsFromProcessEnv(t *testing.T) {
	// TakeSecretEnv는 실제 비밀 환경 변수도 지우므로 먼저 t.Setenv로 걸어 테스트 뒤 되돌린다.
	for name, value := range ReadSecretEnv() {
		t.Setenv(name, value)
	}
	secrets := map[string]string{
		PostgresDSNEnv:       "postgres://artex:fake-pg-password@db:5432/artex",
		"ANTHROPIC_API_KEY":  "sk-ant-fake-anthropic-key",
		"OPENAI_API_KEY":     "sk-fake-openai-key",
		"ARTEX_LLM_PROXY":    "http://user:fake-llm-proxy-password@proxy:3128",
		"ARTEX_LLM_PROVIDER": "anthropic",
	}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	t.Setenv("HTTP_PROXY", "http://proxy.test:3128")
	t.Setenv("ARTEX_KEY_DIR", "/keys")

	env := TakeSecretEnv()

	for _, kv := range os.Environ() {
		for name, value := range secrets {
			if strings.Contains(kv, value) || strings.HasPrefix(kv, name+"=") {
				t.Errorf("지운 뒤에도 프로세스 환경에 %s가 있다: %q", name, kv)
			}
		}
	}
	for name, value := range secrets {
		if got := env.Get(name); got != value {
			t.Errorf("읽어 둔 %s = %q, want %q", name, got, value)
		}
	}
	for name, want := range map[string]string{"HTTP_PROXY": "http://proxy.test:3128", "ARTEX_KEY_DIR": "/keys"} {
		if got := os.Getenv(name); got != want {
			t.Errorf("비밀이 아닌 %s가 지워졌다: got %q want %q", name, got, want)
		}
	}
	// DB 연결은 지운 환경이 아니라 읽어 둔 값으로 정한다.
	if dsn, _, err := PostgresDSN(env); err != nil || dsn != secrets[PostgresDSNEnv] {
		t.Errorf("PostgresDSN(읽어 둔 값) = %q, %v", dsn, err)
	}
}
