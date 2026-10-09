package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 개요 화면 '목표 관리'의 수동 CRUD API다. 에이전트 쪽 set_goals 도구와 같은 goal 노드를 쓰지만,
// 진입점은 사람이 UI에서 직접 추가·삭제·수정하는 것이다. 추가·수정 뒤에는 '작업 되살리기' 로직(admitTask resume:
// 종료 상태→running, 일시 중지 해제, 필요하면 대기열에 넣기)을 재사용하고, 삭제는 되살리지 않는다(제품 결정).
// 변경 handler는 모두 beginTaskOperation/decInflight를 거쳐 작업 삭제와의 경쟁 상태를 피한다(의도 CRUD와 같다).

// listGoals 는 이 작업의 목표 전체를 text/vulnclass/state로 나눠 돌려준다. 목표 관리 카드가 그린다.
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal 은 사람이 목표 하나를 추가한다. 저장(작업 루트의 spawns 아래에 단다) → '목표 추가' 트리거를 기록해
// 플래너를 깨움 → 작업 되살리기 순서로, 플래너가 새 목표를 기준으로 달성 여부를 다시 판단하게 한다.
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 목표를 추가할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "목표 내용은 비워 둘 수 없습니다")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // '사람이 목표를 추가함: …' 트리거를 기록하고 플래너를 깨운다
	s.reviveTask(t)              // 완료·일시 중지된 작업을 실행 상태로 되돌려 계속 돌린다
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "목표를 저장한 뒤 다시 읽지 못했습니다")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal 은 사람이 목표 텍스트(와 vulnclass)를 고친다. DB 수정 → '사용자가 목표를 old에서 new로 바꿈' 트리거를
// 기록해 플래너를 깨움 → 작업 되살리기 순서로, 플래너가 새 목표에 맞춰 방향을 바꾸게 한다.
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 목표를 수정할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "목표 내용은 비워 둘 수 없습니다")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "목표가 없습니다")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // '사람이 목표를 old에서 new로 바꿈' 트리거를 기록하고 플래너를 깨운다
	s.reviveTask(t)                   // 추가와 같다. 작업을 되살려 새 목표로 다시 판단하게 한다
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "목표를 수정한 뒤 다시 읽지 못했습니다")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal 은 사람이 목표 하나를 지운다(영구 삭제, 간선·앵커는 연쇄 삭제). DB 삭제 → '사용자가 목표 X를 삭제함'
// 트리거를 기록해 플래너가 남은 목표를 다시 판단하게 한다. 제품 결정에 따라 삭제는 작업을 되살리지 **않는다**.
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "작업을 삭제하는 중이라 목표를 삭제할 수 없습니다")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "목표가 없습니다")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // '사람이 목표를 삭제함: …' 트리거를 기록하고 플래너를 깨운다(작업은 되살리지 않는다)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
