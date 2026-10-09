package llmauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Claims 는 토큰 JWT 에서 꺼낸 값이다.
type Claims struct {
	// AccountID 는 chatgpt_account_id 클레임이다. 없으면 비어 있다.
	AccountID string
	// ExpiresAt 은 exp 클레임이다. 없으면 제로값이다.
	ExpiresAt time.Time
	// PlanType 은 chatgpt_plan_type 클레임을 소문자로 바꾼 값이다(예: "plus", "pro").
	// 없거나 플랜 이름 모양이 아니면 비어 있다.
	PlanType string
}

// planTypePattern 은 플랜 이름 모양이다. 화면에 그대로 보이는 값이라 이 모양이 아니면 버린다.
var planTypePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ParseClaims 는 JWT의 payload 에서 계정과 만료 시각을 꺼낸다.
// 서명은 검증하지 않는다. 토큰은 인증 서버에서 TLS로 직접 받은 것이고, 쓰임새는 만료 판단과
// 헤더 값뿐이라서다. JWT 모양이 아니거나 payload가 JSON이 아니면 오류를 올린다.
// 오류에는 토큰 원문을 싣지 않는다.
func ParseClaims(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("jwt: not three segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Claims{}, errors.New("jwt: payload is not base64url")
	}
	var body struct {
		Exp *json.Number `json:"exp"`
		// OpenAI는 계정 정보를 이 이름의 클레임 객체에 넣는다.
		// 클레임 경로는 openai/codex codex-rs/login/src/token_data.rs의 AuthClaims
		// (chatgpt_account_id, chatgpt_plan_type)와 같다(커밋 f326857c).
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			// 플랜은 표시용이라 문자열이 아닌 값이 와도 토큰 전체를 거절하지 않게 원문으로 받는다.
			PlanType json.RawMessage `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return Claims{}, errors.New("jwt: payload is not a json object")
	}
	claims := Claims{AccountID: body.Auth.AccountID, PlanType: parsePlanType(body.Auth.PlanType)}
	if body.Exp != nil {
		seconds, err := body.Exp.Float64()
		if err != nil {
			return Claims{}, errors.New("jwt: exp is not a number")
		}
		claims.ExpiresAt = time.Unix(int64(seconds), 0)
	}
	return claims, nil
}

// parsePlanType 은 플랜 클레임을 소문자 플랜 이름으로 바꾼다. Codex CLI 도 대소문자를 가리지 않고
// 읽는다(codex-rs/protocol/src/auth.rs PlanType::from_raw_value). 문자열이 아니거나 모양이
// 맞지 않으면 빈 값이다. 플랜은 표시에만 쓰므로 "모름"으로 두고 로그인은 막지 않는다.
func parsePlanType(raw json.RawMessage) string {
	var plan string
	if json.Unmarshal(raw, &plan) != nil {
		return ""
	}
	plan = strings.ToLower(plan)
	if !planTypePattern.MatchString(plan) {
		return ""
	}
	return plan
}
