package server

import (
	"fmt"
	"net/http"

	"github.com/Autumn-27/artex/db"
)

// 작업 단위 자산 차단·허용 규칙의 CRUD. 규칙은 task_id에 속하고 그 작업에만 적용된다.
// action=block은 차단(테스트 금지), action=allow는 허용(허용 목록)이다. 실제 판정은 db.EvaluateAssetGate에 있다.

type taskInterceptRuleReq struct {
	Enabled bool   `json:"enabled"`
	Action  string `json:"action"` // block | allow
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Note    string `json:"note"`
}

// validateTaskInterceptRuleReq 는 요청을 정규화하고 검사한다. 전역 규칙의 kind/pattern 검사기를 재사용한다.
func validateTaskInterceptRuleReq(req *taskInterceptRuleReq) error {
	if req.Action == "" {
		req.Action = "block"
	}
	if req.Action != "block" && req.Action != "allow" {
		return fmt.Errorf("action은 block 또는 allow여야 합니다")
	}
	v := assetInterceptRuleReq{Enabled: req.Enabled, Kind: req.Kind, Pattern: req.Pattern, Note: req.Note}
	if err := validateAssetInterceptRuleReq(&v); err != nil {
		return err
	}
	req.Pattern = v.Pattern // 앞뒤 공백을 이미 지웠다
	return nil
}

// buildTaskInterceptRules 는 작업을 만들 때 입력한 작업 단위 규칙을 검사하고 db 입력 형태로 바꾼다.
func buildTaskInterceptRules(reqs []taskInterceptRuleReq) ([]db.TaskInterceptRuleInput, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	out := make([]db.TaskInterceptRuleInput, 0, len(reqs))
	for i := range reqs {
		rq := reqs[i]
		if err := validateTaskInterceptRuleReq(&rq); err != nil {
			return nil, err
		}
		out = append(out, db.TaskInterceptRuleInput{
			Enabled: rq.Enabled,
			Action:  rq.Action,
			Kind:    rq.Kind,
			Pattern: rq.Pattern,
			Note:    rq.Note,
		})
	}
	return out, nil
}

func (s *Server) taskInterceptListRules(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := pathInt(r, "id")
	if !ok || taskID <= 0 {
		writeErr(w, 400, "bad task id")
		return
	}
	rules, err := pg.Assets().ListTaskInterceptRules(taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rules == nil {
		rules = []db.AssetInterceptRule{}
	}
	writeJSON(w, 200, map[string]any{"rules": rules})
}

func (s *Server) taskInterceptCreateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := pathInt(r, "id")
	if !ok || taskID <= 0 {
		writeErr(w, 400, "bad task id")
		return
	}
	var req taskInterceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateTaskInterceptRuleReq(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.Assets().CreateTaskInterceptRule(taskID, req.Action, req.Kind, req.Pattern, req.Note, req.Enabled)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rule)
}

func (s *Server) taskInterceptUpdateRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := pathInt(r, "id")
	if !ok || taskID <= 0 {
		writeErr(w, 400, "bad task id")
		return
	}
	ruleID, ok := pathInt(r, "rid")
	if !ok || ruleID <= 0 {
		writeErr(w, 400, "bad rule id")
		return
	}
	var req taskInterceptRuleReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := validateTaskInterceptRuleReq(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := pg.Assets().UpdateTaskInterceptRule(taskID, ruleID, req.Action, req.Kind, req.Pattern, req.Note, req.Enabled)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rule)
}

func (s *Server) taskInterceptDeleteRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := pathInt(r, "id")
	if !ok || taskID <= 0 {
		writeErr(w, 400, "bad task id")
		return
	}
	ruleID, ok := pathInt(r, "rid")
	if !ok || ruleID <= 0 {
		writeErr(w, 400, "bad rule id")
		return
	}
	deleted, err := pg.Assets().DeleteTaskInterceptRule(taskID, ruleID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted})
}

func (s *Server) taskInterceptToggleRule(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, ok := pathInt(r, "id")
	if !ok || taskID <= 0 {
		writeErr(w, 400, "bad task id")
		return
	}
	ruleID, ok := pathInt(r, "rid")
	if !ok || ruleID <= 0 {
		writeErr(w, 400, "bad rule id")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.Assets().ToggleTaskInterceptRule(taskID, ruleID, req.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": req.Enabled})
}
