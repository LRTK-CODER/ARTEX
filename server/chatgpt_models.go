package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/llmauth"
)

// codexClientVersion 은 Codex 모델 목록 요청의 client_version 이다. 백엔드는 이 값으로 클라이언트가
// 받을 수 있는 모델을 고른다. Codex CLI rust-v0.160.1(2026-10-05 릴리스)의 버전이다.
// 출처: openai/codex codex-rs/models-manager/src/lib.rs client_version_to_whole,
// codex-rs/codex-api/src/endpoint/models.rs append_client_version_query.
const codexClientVersion = "0.160.1"

const (
	// codexModelsTimeout 은 모델 목록 요청 한 번의 상한이다. 다른 모델 목록 경로와 같다.
	codexModelsTimeout = 30 * time.Second
	// maxCodexModelsBytes 는 모델 목록 응답을 읽을 상한이다.
	maxCodexModelsBytes = 4 << 20
)

// 사용자에게 보이는 고정 문구. 오류 원문(토큰·응답 본문이 섞일 수 있다)은 로그에도 응답에도 싣지 않는다.
const (
	chatGPTSaveFirstMessage              = "ChatGPT 구독 프로필은 저장하고 로그인한 뒤 쓸 수 있다"
	chatGPTLoginRequiredMessage          = "ChatGPT 구독 로그인이 필요하다"
	chatGPTCredentialsUnavailableMessage = "저장된 ChatGPT 구독 자격 증명을 읽지 못했다"
	chatGPTModelsFailedMessage           = "ChatGPT 모델 목록을 가져오지 못했다"
)

// errCodexUnauthorized 는 모델 목록 요청이 401 을 받았다는 뜻이다. 토큰은 버려 다음 호출이 갱신한다.
var errCodexUnauthorized = errors.New("codex models: unauthorized")

// codexModelVisibilityList 는 Codex 가 모델 선택 목록에 보이는 모델의 visibility 값이다.
// 출처: openai/codex codex-rs/protocol/src/openai_models.rs ModelVisibility(list·hide·none).
const codexModelVisibilityList = "list"

// fetchCodexModels 는 Codex 백엔드의 계정별 모델 목록에서 선택 목록에 보이는 모델 이름(slug)을 돌려준다.
// 응답은 확인된 필드(models[].slug, models[].visibility)만 읽는다. 오류에는 토큰도 응답 본문도 싣지 않는다.
func fetchCodexModels(ctx context.Context, client *http.Client, baseURL string, tokens agent.OAuthTokenSource) ([]string, error) {
	accessToken, accountID, err := tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex models: token: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, codexModelsTimeout)
	defer cancel()
	endpoint := baseURL + "/models?" + url.Values{"client_version": {codexClientVersion}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("codex models: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("originator", llmauth.Originator)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex models: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		tokens.Invalidate(accessToken)
		return nil, errCodexUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex models: status %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCodexModelsBytes)).Decode(&body); err != nil {
		return nil, errors.New("codex models: response is not the expected json")
	}
	models := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		if m.Slug != "" && m.Visibility == codexModelVisibilityList {
			models = append(models, m.Slug)
		}
	}
	return models, nil
}

// codexModelsBaseURL 은 모델 목록을 물을 Codex 백엔드 주소다. 비어 있으면 Codex 기본 주소이고,
// 응답 엔드포인트까지 적은 주소("/responses")도 받아 준다.
func codexModelsBaseURL(baseURL string) string {
	b := strings.TrimRight(strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/responses"), "/")
	if b == "" {
		return agent.CodexBaseURL
	}
	return b
}

// codexModelsErrorMessage 는 모델 목록 실패를 사용자에게 보일 고정 문구로 바꾼다.
func codexModelsErrorMessage(err error) string {
	if errors.Is(err, errCodexUnauthorized) || needsChatGPTLogin(err) {
		return chatGPTLoginRequiredMessage
	}
	return chatGPTModelsFailedMessage
}

// needsChatGPTLogin 은 갱신으로는 풀리지 않아 다시 로그인해야 하는 토큰 오류인지 알려 준다.
func needsChatGPTLogin(err error) bool {
	if errors.Is(err, llmauth.ErrNoTokens) {
		return true
	}
	var tokenErr *llmauth.TokenError
	return errors.As(err, &tokenErr) && tokenErr.NeedsLogin()
}
