package llmauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	// deviceCodeTimeout 은 사용자가 코드를 입력하기를 기다리는 상한이다. Codex CLI 와 같다.
	deviceCodeTimeout = 15 * time.Minute
	// defaultPollInterval 은 서버가 interval 을 주지 않을 때의 폴링 간격이다.
	defaultPollInterval = 5 * time.Second
)

var (
	// ErrDeviceCodeDisabled 는 서버나 계정에서 디바이스 코드 로그인이 꺼져 있다는 뜻이다.
	// 브라우저 로그인을 써야 한다.
	ErrDeviceCodeDisabled = errors.New("llmauth: device code login is not enabled")
	// ErrDeviceCodeTimeout 은 15분 안에 사용자가 코드를 입력하지 않았다는 뜻이다.
	ErrDeviceCodeTimeout = errors.New("llmauth: device code login timed out")
)

// DeviceCode 는 디바이스 코드 로그인을 시작한 결과다. 사용자에게 VerificationURL 과 UserCode 를 보여 준다.
type DeviceCode struct {
	DeviceAuthID    string
	UserCode        string
	VerificationURL string
	// Interval 은 서버가 정한 폴링 간격이다. 0 이하면 5초를 쓴다.
	Interval time.Duration
}

// StartDeviceCode 는 디바이스 코드 로그인을 시작한다.
// 서버가 404 를 돌려주면 ErrDeviceCodeDisabled 를 올린다.
func (c *Client) StartDeviceCode(ctx context.Context) (DeviceCode, error) {
	const op = "start device code"
	payload, err := json.Marshal(map[string]string{"client_id": ClientID})
	if err != nil {
		return DeviceCode{}, fmt.Errorf("llmauth: %s: encode request: %w", op, err)
	}
	status, raw, err := c.do(ctx, op, "/api/accounts/deviceauth/usercode", "application/json", bytes.NewReader(payload))
	if err != nil {
		return DeviceCode{}, err
	}
	if status == http.StatusNotFound {
		return DeviceCode{}, ErrDeviceCodeDisabled
	}
	if status < 200 || status > 299 {
		return DeviceCode{}, &TokenError{Op: op, StatusCode: status, Code: errorCode(raw)}
	}
	var body struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		// 서버에 따라 user_code 대신 usercode 를 쓴다.
		UserCodeAlt string `json:"usercode"`
		// 서버는 interval 을 "5" 같은 문자열로 준다. 숫자도 받는다.
		Interval json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return DeviceCode{}, fmt.Errorf("llmauth: %s: decode response: invalid json", op)
	}
	if body.UserCode == "" {
		body.UserCode = body.UserCodeAlt
	}
	if body.DeviceAuthID == "" || body.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("llmauth: %s: response has no device_auth_id or user_code", op)
	}
	interval, err := parseInterval(body.Interval)
	if err != nil {
		return DeviceCode{}, fmt.Errorf("llmauth: %s: %w", op, err)
	}
	return DeviceCode{
		DeviceAuthID:    body.DeviceAuthID,
		UserCode:        body.UserCode,
		VerificationURL: c.issuer() + "/codex/device",
		Interval:        interval,
	}, nil
}

func parseInterval(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return defaultPollInterval, nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		text = string(raw)
	}
	seconds, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, errors.New("interval is not a whole number of seconds")
	}
	if seconds == 0 {
		return defaultPollInterval, nil
	}
	return time.Duration(seconds) * time.Second, nil
}

// WaitDeviceCode 는 사용자가 코드를 입력할 때까지 폴링한 뒤 토큰으로 교환한다.
// 서버의 403·404 는 아직 입력 전이라는 뜻이라 기다린다. 15분이 지나면 ErrDeviceCodeTimeout 을 올린다.
func (c *Client) WaitDeviceCode(ctx context.Context, dc DeviceCode) (Tokens, error) {
	const op = "poll device code"
	payload, err := json.Marshal(map[string]string{
		"device_auth_id": dc.DeviceAuthID,
		"user_code":      dc.UserCode,
	})
	if err != nil {
		return Tokens{}, fmt.Errorf("llmauth: %s: encode request: %w", op, err)
	}
	interval := dc.Interval
	if interval <= 0 {
		// 호출자가 DeviceCode 를 직접 만들어 간격을 비우면 쉬지 않고 폴링하게 된다.
		interval = defaultPollInterval
	}
	deadline := c.clock().Add(deviceCodeTimeout)
	for {
		status, raw, err := c.do(ctx, op, "/api/accounts/deviceauth/token", "application/json", bytes.NewReader(payload))
		if err != nil {
			return Tokens{}, err
		}
		if status >= 200 && status <= 299 {
			return c.exchangeDeviceCode(ctx, raw)
		}
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return Tokens{}, &TokenError{Op: op, StatusCode: status, Code: errorCode(raw)}
		}
		remaining := deadline.Sub(c.clock())
		if remaining <= 0 {
			return Tokens{}, ErrDeviceCodeTimeout
		}
		if err := c.wait(ctx, min(interval, remaining)); err != nil {
			return Tokens{}, fmt.Errorf("llmauth: %s: %w", op, err)
		}
	}
}

func (c *Client) exchangeDeviceCode(ctx context.Context, raw []byte) (Tokens, error) {
	var body struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Tokens{}, errors.New("llmauth: poll device code: decode response: invalid json")
	}
	if body.AuthorizationCode == "" || body.CodeVerifier == "" {
		return Tokens{}, errors.New("llmauth: poll device code: response has no authorization_code or code_verifier")
	}
	return c.ExchangeCode(ctx, body.AuthorizationCode, body.CodeVerifier, c.issuer()+"/deviceauth/callback")
}

func (c *Client) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
