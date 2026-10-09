package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func (s *Server) listClaudeModels(w http.ResponseWriter, r *http.Request, stored *db.LLMProfile, proxy string) {
	fail := func(message string) {
		writeJSON(w, 200, map[string]any{"ok": false, "models": []string{}, "error": message})
	}
	if stored == nil || stored.AuthType != db.AuthClaudeOAuth {
		fail("Claude 구독 프로필을 먼저 저장한다")
		return
	}
	src, err := s.oauth.connectedSourceForAuth(r.Context(), stored.ID, db.AuthClaudeOAuth)
	if err != nil {
		fail(claudeLoginRequiredMessage)
		return
	}
	client, err := agent.ClaudeOAuthHTTPClient(proxy, src)
	if err != nil {
		fail("모델 목록 조회용 프록시 설정이 올바르지 않다")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(s.claudeURL(), "/")+"/v1/models?limit=100", nil)
	if err != nil {
		fail("모델 목록 요청을 만들지 못했다")
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		fail("Claude 모델 목록을 읽지 못했다")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail("Claude 모델 목록을 읽지 못했다. 로그인 상태와 구독을 확인한다")
		return
	}
	const maxModelsResponseBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsResponseBytes+1))
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err != nil || len(raw) > maxModelsResponseBytes || json.Unmarshal(raw, &data) != nil {
		fail("Claude 모델 목록 응답 형식이 올바르지 않다")
		return
	}
	models := make([]string, 0, len(data.Data))
	for _, m := range data.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "models": models})
}
