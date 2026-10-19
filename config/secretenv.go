package config

import (
	"os"
	"strings"
)

// PostgresDSNEnv 는 PostgreSQL DSN을 주는 환경 변수 이름이다. DB 비밀번호가 들어 있다.
const PostgresDSNEnv = "ARTEX_PG_DSN"

// 시작할 때 읽고 프로세스 환경에서 지우는 비밀 환경 변수.
// ARTEX_LLM_*는 이름 전체를 비밀로 보지는 않지만 ARTEX_LLM_PROXY에 프록시 자격 증명이,
// ARTEX_LLM_BASE_URL에 토큰이 든 주소가 올 수 있어 접두사째 지운다.
var secretEnvNames = []string{PostgresDSNEnv, "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}

const secretEnvPrefix = "ARTEX_LLM_"

// SecretEnv 는 시작할 때 프로세스 환경에서 읽어 둔 비밀 환경 변수다. 키는 대문자 이름이다.
// 제로값(nil)은 "비밀 환경 변수가 하나도 없음"이다.
type SecretEnv map[string]string

// Get 은 읽어 둔 값을 돌려준다. 없으면 빈 문자열이다. agent.FromEnv에 os.Getenv 대신 넘길 수 있다.
func (e SecretEnv) Get(name string) string {
	return e[strings.ToUpper(name)]
}

// ReadSecretEnv 는 ARTEX가 읽는 비밀 환경 변수를 프로세스 환경에서 읽기만 한다. 환경은 바꾸지 않는다.
// 서버 시작에는 TakeSecretEnv를 쓴다. 이 함수는 환경을 지우면 안 되는 곳, 곧 같은 프로세스에서 여러 번 읽어야 하는 곳에 쓴다.
func ReadSecretEnv() SecretEnv {
	env := SecretEnv{}
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if ok && isSecretEnvName(name) {
			env[strings.ToUpper(name)] = value
		}
	}
	return env
}

// TakeSecretEnv 는 ARTEX가 읽는 비밀 환경 변수를 읽어 돌려주고 자기 프로세스 환경에서 지운다.
//
// 에이전트가 띄우는 자식 프로세스(norma의 Bash·백그라운드 작업·shell_open·stdio MCP)는
// os.Environ()을 그대로 물려받는다. norma를 고치지 않고 비밀값이 자식에게 가지 않게 하려면
// 서버 자신의 환경에서 지워야 한다. 그래서 자식 프로세스를 띄우기 전, main 맨 앞에서 한 번만 부른다.
// 이후 코드는 os.Getenv가 아니라 돌려받은 값을 쓴다.
//
// 지워도 /proc/<pid>/environ의 초기 환경에는 남는다. 같은 uid의 자식은 그것을 읽을 수 있다(#164, #165에서 다룬다).
func TakeSecretEnv() SecretEnv {
	env := ReadSecretEnv()
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && isSecretEnvName(name) {
			// Unsetenv는 이름이 비었거나 '='를 담을 때만 실패하는데, os.Environ에서 온 이름은 그렇지 않다.
			_ = os.Unsetenv(name)
		}
	}
	return env
}

// isSecretEnvName 은 Windows 환경 변수 이름이 대소문자를 가리지 않으므로 대문자로 바꿔 비교한다.
func isSecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, secret := range secretEnvNames {
		if upper == secret {
			return true
		}
	}
	return strings.HasPrefix(upper, secretEnvPrefix)
}
