package llmauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Claims 는 토큰 JWT 에서 꺼낸 값이다.
type Claims struct {
	// AccountID 는 chatgpt_account_id 클레임이다. 없으면 비어 있다.
	AccountID string
	// ExpiresAt 은 exp 클레임이다. 없으면 제로값이다.
	ExpiresAt time.Time
}

// ParseClaims 는 JWT 의 payload 에서 계정과 만료 시각을 꺼낸다.
// 서명은 검증하지 않는다. 토큰은 인증 서버에서 TLS 로 직접 받은 것이고, 쓰임새는 만료 판단과
// 헤더 값뿐이라서다. JWT 모양이 아니거나 payload 가 JSON 이 아니면 오류를 올린다.
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
		// OpenAI 는 계정 정보를 이 이름의 클레임 객체에 넣는다.
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return Claims{}, errors.New("jwt: payload is not a json object")
	}
	claims := Claims{AccountID: body.Auth.AccountID}
	if body.Exp != nil {
		seconds, err := body.Exp.Float64()
		if err != nil {
			return Claims{}, errors.New("jwt: exp is not a number")
		}
		claims.ExpiresAt = time.Unix(int64(seconds), 0)
	}
	return claims, nil
}
