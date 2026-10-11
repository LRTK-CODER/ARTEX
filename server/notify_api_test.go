package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 알림 발송 기능을 끝에서 끝까지 검사한다: 취약점 저장 → 이벤트 → 분배 → 실제 HTTP 발송.
//
// 보안상 주의할 점: 이 테스트 케이스들은 **전역 Notifier.step()을 부르지 않고**, 자기가 만든
// 알림 채널에만 stepRealtime/stepDigest를 부른다. step()은 DB의 사용 중인 알림 채널을 모두 돈다.
// 실제 DingTalk/WeCom 봇이 설정된 개발 DB에서 테스트를 돌리면 전역 step이 테스트 중에
// 생긴 취약점을 그 그룹들로 실제로 보낸다. 알림 채널별로 부르면 영향 범위가 테스트가 만든 가짜 수신 측으로 엄격히 한정된다.
//
// 정리: 테스트 케이스가 끝나면 그 케이스가 만든 이벤트(전달은 연쇄 삭제)와 알림 채널을 지워, 실제 알림 채널에 밀린 알림을 남기지 않는다.
//
// 검사 기준: stepRealtime/stepDigest는 값을 돌려주지 않고 안에서 로그만 남긴다. 그래서 여기서는
// 함수의 반환값이 아니라 **밖에서 관찰할 수 있는 동작**(가짜 수신 측이 무엇을 받았는지, 전달 행이 어떤 상태가 됐는지)을
// 검사한다. 반환값을 스텁으로 바꿔 검사하는 것보다 실제 호출 경로에 가깝다.

// notifyFixture 는 이 파일 테스트 케이스들이 함께 쓰는 픽스처다.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 직접 만든 task/exploration. 테스트 케이스가 취약점을 여기에 기록해 다른 테스트 케이스의 데이터와 분리한다.
	taskID int64
	expID  int64
	// cleanupMark 뒤에 생긴 이벤트는 정리할 때 함께 지운다.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 이 파일의 가짜 수신 측은 모두 127.0.0.1에서 돌지만, 전달은 기본적으로 루프백 주소를 거부한다
	// (SSRF가 같은 기기의 서비스와 클라우드 메타데이터에 닿지 않게). 테스트는 이 스위치를 명시적으로 켠다.
	// '기본 거부' 동작은 notify 패키지의 ssrf_test.go가 검사한다.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// task를 직접 만든다. 공용 픽스처 trafficEvidenceServer가 만든 task로는 exploration id를 얻을 수 없는데,
	// 취약점을 기록하려면 그 값이 있어야 한다.
	task, err := s.m.CreateTask("알림 발송 테스트", "알림 발송 동작 검증", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 픽스처가 스스로 닫힌 상태가 되게 한다. 픽스처를 만들기 전부터 있던 이벤트를 한 번에 분배됨으로 표시한다.
	//
	// 꼭 해야 하는 이유: FanOutPendingEvents는 **전역**이라 DB의 분배되지 않은 이벤트를 모두
	// 일치하는 알림 채널 전체로 펼친다. 공용 픽스처 trafficEvidenceServer도 스스로 취약점 하나를 기록하고
	// (바로 그것이 돌려주는 첫 finding이다), 다른 테스트 케이스가 남긴 것도 있을 수 있다. 분리하지 않으면
	// 이 떠도는 이벤트가 이 테스트 케이스의 알림 채널로 분배돼 '전달이 N건이어야 함' 같은 검사가
	// 될 때도 있고 안 될 때도 있다. 게다가 어떻게 틀리는지가 테스트 케이스 실행 순서에 달려 있어, 바로 실패하는 것보다 찾기 어렵다.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("알림 이벤트 정리 실패: %v", err)
		}
	})
	// 전체 스위치는 켜져 있어야 한다(다른 테스트 케이스가 껐을 수 있다).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record 는 실제 증거 쓰기 경로로 취약점 하나를 저장하고 finding id를 돌려준다.
// 이 경로는 **같은 트랜잭션** 안에서 알림 발송 이벤트를 등록한다. 바로 이 기능이 걸리는 지점이다.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 요약",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("취약점 기록 실패: %v", err)
	}
	return out.FindingID
}

// channel 은 알림 채널 설정을 다시 읽는다(알림 채널별로 stepX를 부를 때 쓴다).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("알림 채널 읽기 실패: %v", err)
	}
	return ch
}

// deliver 는 이벤트를 분배하고 지정한 알림 채널에만 전달을 한 번 돌린다.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel 은 HTTP API로 알림 채널을 만든다. 그러면서 API 자체의 검사 경로도 함께 검사한다.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("알림 채널 만들기 실패 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("알림 채널 만들기 응답이 이상함: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook 은 받은 요청 본문을 기록하는 가짜 수신 측이다.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("가짜 수신 측이 받은 요청은 %d건뿐이라 %d번째를 꺼낼 수 없음", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("가짜 수신 측이 받은 요청이 없음")
	}
	return f.body(t, f.count()-1)
}

// markdownText 는 요청 본문에서 메시지 본문을 꺼낸다. 플랫폼마다 다른 필드 이름을 모두 받는다.
// DingTalk markdown은 `text`, ActionCard는 `text`, WeCom markdown은 `content`를 쓴다.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("요청 본문에 알아볼 수 있는 메시지 본문이 없음: %v", body)
	return ""
}

// agePendingBatch 는 이 알림 채널의 발송 대기 전달을 오래된 것으로 만든다. 다이제스트 배치의 기한이 지나는 경우를 테스트할 때 쓴다.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "실시간 알림 발송",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL 인젝션", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("보낸 메시지 수 = %d, 기대값 1", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL 인젝션", "높음", "요약"} {
		if !strings.Contains(text, want) {
			t.Fatalf("메시지 본문에 %q이(가) 없음:\n%s", want, text)
		}
	}
	// 전달은 sent로 넘어가야 한다.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("전달 뒤 sent로 표시되지 않은 건수 = %d, 기대값 0", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "마스킹 테스트 케이스",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("알림 채널 목록 조회 실패 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("API 응답에 자격 증명이 드러남: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("새로 만든 알림 채널이 목록에 없음")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("자격 증명 필드는 마스킹된 값이어야 함: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("API는 어느 필드가 자격 증명인지 프런트에 알려야 함")
	}

	// PATCH로 이름만 바꾸고 마스킹된 자격 증명을 되돌려 보낸다. 진짜 자격 증명은 그대로 유지돼야 한다.
	body, _ := json.Marshal(map[string]any{
		"name":   "바꾼 이름",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("갱신 실패 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("마스킹된 값을 되돌려 보냈더니 진짜 자격 증명을 덮어씀: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("마스킹된 값을 되돌려 보냈더니 secret을 덮어씀: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "바꾼 이름" {
		t.Fatal("이름이 갱신되지 않음")
	}

	// secret을 명시적으로 비우면 적용돼야 한다('마스킹된 값을 되돌려 보냄 = 그대로 유지'와 구분한다).
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret 비우기 실패 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("빈 문자열이면 secret을 비워야 함")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"잘못된 유형", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "잘못된 알림 채널 유형입니다"},
		{"이름 없음", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "알림 채널 이름을 입력하세요"},
		{"webhook 없음", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook 프로토콜 잘못됨", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "잘못된 Webhook 주소입니다"},
		{"모드 잘못됨", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "잘못된 알림 발송 모드입니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("상태 코드 = %d, 기대값 400: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("오류 메시지 = %s, 기대값 %q 포함", r.Body, tc.wantSub)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("없는 알림 채널 삭제의 상태 코드 = %d, 기대값 404", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "치명만",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "낮은 심각도 문제", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("기준 미만 취약점의 전달 수 = %d, 기대값 0", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("걸러진 취약점은 메시지를 보내면 안 됨")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "다이제스트 알림 발송",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("다이제스트 취약점%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 기한 전: 보내지 않는다.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("다이제스트 배치를 기한 전에 보냄")
	}

	// 배치를 오래된 것으로 만든 뒤: 세 건이 메시지 하나로 합쳐진다.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("보낸 메시지 수 = %d, 기대값 1(세 건을 다이제스트 하나로)", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "최근") || !strings.Contains(text, "새 취약점 3건") {
		t.Fatalf("다이제스트 메시지에 건수·시간 범위 문구가 없음:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("다이제스트 취약점%d", i)) {
			t.Fatalf("다이제스트 메시지에 %d번째 항목이 없음:\n%s", i, text)
		}
	}
	// 같은 배치는 batch_id를 함께 써야 한다.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("batch_id 수 = %d, 전달 수 = %d, 기대값 batch_id 1개에 전달 3건", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "사용 안 함 알림 채널",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "꺼져 있는 동안의 취약점", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("사용 안 함 알림 채널의 전달 수 = %d, 기대값 0", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 구독",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "상태 변경 테스트 케이스", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 두 건이 있어야 한다. fixed 건이 상태 변경이고, finding_created 건도 같은 차례에 나갈 수 있다.
	// 상태 변경 건이 실제로는 더 늦게 만들어지지만, 순서에 기대지 않고 전부 찾는다.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("'상태 변경 → 수정됨'이 든 메시지를 받지 못함(총 %d건)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상태 변경 구독 안 함",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "변경 구독 안 함", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("상태 변경 실패 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("상태 변경을 구독하지 않은 알림 채널의 상태 변경 전달 수 = %d, 기대값 0", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "테스트 발송",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("테스트 발송 실패 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("가짜 수신 측이 받은 테스트 메시지 수 = %d, 기대값 1", hook.count())
	}
	// 테스트 메시지는 한눈에 테스트임을 알 수 있어야 한다. 실제 취약점으로 오해받으면 안 된다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("테스트 메시지에 테스트라는 표시가 있어야 함: %s", text)
	}
	// 설정이 잘못됐으면 알림 채널의 원래 오류를 그대로 사용자에게 돌려줘야 한다.
	badID := f.createChannel(t, map[string]any{
		"name":   "잘못된 주소",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("전달 실패 시 상태 코드 = %d, 기대값 502: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 반드시 실패하는 주소를 가리켜 failed 전달을 만든다.
	chID := f.createChannel(t, map[string]any{
		"name":   "실패 재시도",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "실패할 알림 발송", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 재시도 한도를 다 쓸 때까지 계속 전달한다.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("재시도를 다 쓴 뒤 상태 = %s, 기대값 failed", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("전달 기록 조회 실패 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("실패한 전달 수 = total=%d len=%d, 기대값 1", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("전달 기록에 실패 원인이 있어야 함. 없으면 사용자가 원인을 찾을 수 없음")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("기록된 시도 횟수 = %d, 기대값 최대 시도 횟수 이상", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "실패할 알림 발송" {
		t.Fatalf("전달 기록의 취약점 제목 = %q, 기대값 \"실패할 알림 발송\"", hist.Deliveries[0].Title)
	}

	// 수동 재발송: pending으로 돌아가고 횟수가 0이 돼야 한다.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("재발송 실패 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("재발송 뒤 상태/attempts = %s/%d, 기대값 pending/0", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 조회 실패: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta의 알림 채널 수 = %d, 기대값 %d", len(meta.Kinds), len(notify.Kinds()))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("알림 채널 %s이(가) 자격 증명 필드를 알리지 않음", k.Kind)
		}
	}

	// 전역 설정 세 가지를 쓰고 다시 읽는다. 끝의 슬래시는 정규화로 없애야 한다. 그러지 않으면 상세 링크가 "//function/..."이 된다.
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("설정 쓰기 실패 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("링크 기본 주소가 정규화되지 않음: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("다이제스트 주기가 적용되지 않음: %v", payload["notify_digest_interval_min"])
	}

	// 잘못된 값은 거부돼야 한다.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s 상태 코드 = %d, 기대값 400", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL 는 상세 링크 조립을 검사한다. public_base_url을 설정하면
// 단건 메시지는 버튼이 있는 ActionCard를 써야 하고, 링크는 취약점 상세 페이지를 가리켜야 한다.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상세 링크",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "상세 링크가 있는 취약점", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("상세 링크가 있을 때 msgtype = %v, 기대값 ActionCard", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("상세 링크 = %v, 기대값 %s", card["singleURL"], want)
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL 는 반대 경우를 검사한다. 외부 주소를 설정하지 않았으면 잘못된 링크
// (localhost나 상대 경로를 가리키는 것 등)를 만들지 말고 일반 markdown으로 돌아가야 한다.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "상세 링크 없음",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "상세 링크가 없는 취약점", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("외부 주소가 없을 때 msgtype = %v, 기대값 markdown", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "상세 보기") {
		t.Fatalf("외부 주소가 없는데 상세 링크가 나옴:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder 는 '알림 없이 사라짐' 수정을 끝에서 끝까지 보여 주는 증거다.
//
// 다이제스트 메시지는 알림 채널의 길이 상한(WeCom 4096바이트)을 받는다. 배치 하나가 다 들어가지 않으면 **항목 단위로** 나눠야 한다.
// 이번 메시지에 들어간 항목은 전달됨으로 표시하고, 나머지는 큐로 돌아가 다음 메시지를 기다린다. 예전 구현은 배치 전체를
// 성공으로 표시했다. 잘린 항목은 메시지에도 실패 목록에도 없고 전달 기록에는 성공으로 나와,
// 취약점이 그대로 사라졌다.
//
// 네 가지를 검사한다: ① 실제로 들어간 건수만 표시했다 ② 나머지는 아직 발송 대기다 ③ 미룬 항목은
// **재시도 횟수를 쓰지 않았다** ④ 한 번 더 돌리면 남은 것을 보낸다(멈춰 버리지 않는다).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// WeCom을 쓴다. markdown 상한이 4096바이트로 여섯 알림 채널 중 가장 빡빡하다.
	chID := f.createChannel(t, map[string]any{
		"name":   "나눠 보내는 다이제스트",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 제목을 길게 잡아 60건이 4096바이트를 훨씬 넘게 해 반드시 나뉘게 한다.
	longName := strings.Repeat("매우 긴 취약점 이름", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("보낸 메시지 수 = %d, 기대값 1", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("전달됨으로 표시된 항목이 있어야 함")
	}
	if pending == 0 {
		t.Fatalf("한 배치 %d건이 4096바이트에 다 들어갈 수 없으므로 발송 대기가 남아야 함. sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("항목 수가 맞지 않음: sent=%d pending=%d total=%d(전달되지도 발송 대기도 아니면 사라진 것)", sent, pending, total)
	}
	// 메시지 본문은 이번 메시지에 들어가지 않은 항목이 몇 건인지 그대로 알려야 한다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("메시지에 이번에 포함하지 않은 항목이 있다는 안내가 있어야 함:\n%.400s", text)
	}

	// 미룬 항목은 재시도 한도를 쓰면 안 된다. 가져갈 때 attempts를 낙관적으로 1 올렸으므로 미룰 때 다시 빼야 한다.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("미룬 항목의 attempts = %d, 기대값 0(재시도 횟수를 쓰면 몇 번 뒤에 실패로 판정됨)", maxAttempts)
	}

	// 끝날 때까지 되풀이해 돌린다. **결국 모두 전달되고** 중간에 실제로 여러 번에 나뉘었는지를 검사한다.
	// '두 번째에 다 보냄'보다 강한 검사다. 나눠 보내기가 멈추지 않고 남은 항목을 잃지도 않는다는 것을 보인다.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("나눠 보내기가 끝나지 않음: %d번 돌렸는데 아직 %d건이 남음", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d번째 실행에서 진행이 없음. 남은 %d건이 영원히 멈춤", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096바이트 메시지 하나에 제목이 긴 취약점 %d건이 다 들어갈 수 없으므로 여러 번에 나눠 보내야 함. 실행 횟수 = %d, 기대값 2 이상", total, rounds)
	}
	// 첫 번째 뒤의 실행은 모두 **이어 보내기만** 해야 하고, 알림 채널이 거부한 항목은 없어야 한다.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("실패 항목 수 = %d, 기대값 0(가짜 수신 측은 늘 성공을 돌려줌)", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget 는 두 값이 서로 어긋나지 않게 막는 검사다.
//
// 재시도 한도(db.MaxNotifyAttempts)와 백오프 간격 표(notifyBackoff)는 서로 다른 패키지에 있다.
// 앞의 것은 상태 기계의 정책이고 뒤의 것은 엔진의 실행 간격이다. 하나만 바꾸면(예: 한도를 5회로 올리고
// 백오프 단계를 더하는 것을 잊으면) 코드는 오류를 내지 않고 4·5번째 재시도가 마지막 단계 간격을 그대로 쓴다.
// 그러면 '재시도가 이유 없이 느려짐'으로 나타나, 원인을 찾을 때 여기를 떠올리기 어렵다.
// 두 길이가 같은지 검사해 이런 어긋남이 테스트에서 바로 드러나게 한다.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("백오프 단계 수(%d)가 최대 시도 횟수(%d)와 다름. 하나를 바꾸면 다른 하나도 함께 바꿔야 함",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// 백오프 간격은 줄어들면 안 된다. 줄어들면 재시도가 갈수록 잦아져 속도 제한을 오히려 악화시킨다.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("백오프 간격이 줄어듦: %d단계 %v < %d단계 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget 는 '토큰을 먼저 얻고 나서 전달을 가져간다'는 순서를 고정한다.
// 순서가 반대면(먼저 가져가고 나서 버리면) 속도 제한에 막힌 전달이 attempts를 이미 한 번 센 상태라,
// 기다리기만 해도 한도를 다 써 결국 failed가 된다.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// 토큰 버킷 자체만 테스트하므로 Server가 필요 없다(이것 때문에 만들어서도 안 된다).
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 분당 1건: 버킷이 가득 차도 최대 1건이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("분당 1건이고 버킷이 가득 찼을 때 얻은 토큰 수 = %d, 기대값 1", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("토큰을 다 쓴 직후 얻은 토큰 수 = %d, 기대값 0", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("주기의 절반이 지났을 때 얻은 토큰 수 = %d, 기대값 0", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("한 주기가 지났을 때 얻은 토큰 수 = %d, 기대값 1", got)
	}
	// 발송 속도 제한이 없는 알림 채널도 한 번에 처리하는 수에 상한을 둔다. 끝없이 밀린 알림에 한 번의 처리가 붙잡히지 않게 한다.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("발송 속도 제한이 없을 때 얻은 토큰 수 = %d, 기대값 한 회차의 상한 %d", got, notifyUnlimitedBurstPerTick)
	}
	// 알림 채널마다 토큰 버킷은 따로다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("알림 채널 1의 버킷에서 얻은 토큰 수 = %d, 기대값 0(비어 있어야 함)", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens 는 'want개만 얻는다'는 뜻을 고정한다.
//
// 예전 구현은 버킷을 통째로 비운 뒤에야 호출자가 잘라 냈다. 그래서 rate=100/min인 알림 채널이 버킷을 가득 채워도
// 한 번에 5건만 쓰고 남은 토큰 95개를 그냥 버렸다. 그 차례에 발송 대기 전달이 없어도 똑같이 뺐다.
// 그 결과 주석이 말하는 '밀린 알림이 있으면 rate_per_min건을 한 번에 보낼 수 있다'가 어떤 경우에도 되지 않았다.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 버킷은 처음에 가득 차 있고(100), 이번에는 5개만 원한다.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5일 때 얻은 토큰 수 = %d, 기대값 5", got)
	}
	// 핵심 검사: 남은 95개는 비워져 버려지지 않고 버킷에 그대로 있어야 한다.
	// 시간을 진행하지 않아 얻은 토큰이 새로 채운 것이 아니라 남아 있던 것뿐이게 한다.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("남은 토큰에서 얻은 수 = %d, 기대값 95(다르면 버킷이 한 번에 비워진 것)", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("버킷을 다 쓴 뒤 얻은 토큰 수 = %d, 기대값 0", got)
	}
	// want<=0이면 토큰을 하나도 빼면 안 된다(보낼 것이 없는 차례는 비용이 없다).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0일 때 얻은 토큰 수 = %d, 기대값 0", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 다음에 얻은 토큰 수 = %d, 기대값 20(want=0은 토큰을 쓰면 안 됨)", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget 는 다이제스트 모드의 서로 다른 두 단위를 고정한다.
//
// 다이제스트 배치 크기를 한 회차(tick)의 요청 한도에 묶으면, rate_per_min=20인 알림 채널은
// 3초 회차마다 토큰 1개만 채워지고, 그래서 다이제스트 메시지 하나에 취약점이 1건만 들어간다. 기능으로는 다이제스트가
// 없는 것과 같은데 메시지 머리에는 '최근 30분 동안 새 취약점 1건'이라고 나온다. 이 퇴행은 오류를 내지 않고,
// 기존 끝에서 끝까지 테스트 케이스로도 보이지 않는다(그 케이스들은 stepDigest에 충분히 큰 limit를 직접 넘겨
// step 안의 한도 계산을 건너뛴다). 그래서 여기서 결정 자체를 바로 검사한다.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 배치 하나 = 메시지 하나 = 요청 한 번 = 토큰 하나. 토큰의 단위는 취약점이 아니라 메시지다.
	if tokens != 1 {
		t.Fatalf("다이제스트 배치 하나가 쓰는 토큰 수 = %d, 기대값 1(배치 하나는 메시지 하나만 보냄)", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("다이제스트 배치 크기 = %d, 기대값 메모리 상한 db.MaxDigestBatchSize=%d",
			claimLimit, db.MaxDigestBatchSize)
	}
	// 핵심 관계: 배치 크기는 한 회차의 요청 한도보다 훨씬 커야 한다. 둘이 같은 크기 수준이 되면
	// '메시지를 몇 개 보내는가'와 '배치 하나에 취약점을 몇 건 넣는가'를 다시 한 숫자로 섞은 것이다.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("다이제스트 배치 크기 %d이(가) 한 회차의 요청 한도 %d에 묶이면 안 됨. "+
			"요청 한도는 선점 기한에서 거꾸로 계산한 '요청을 몇 번 보내는가'이고, '배치 하나에 취약점을 몇 건 넣는가'와 단위가 다름",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease 도 값이 서로 어긋나지 않게 막는 검사다.
//
// 알림 채널 하나가 한 회차에 전달하는 건수의 상한(notifyMaxSendsPerChannelPerTick)은 선점 기한에서 거꾸로 계산한다.
// 한 회차 안에서 차례로 전달할 때 가장 오래 걸리는 시간은 선점 기한보다 짧아야 한다. 그렇지 않으면 뒤의 몇 건을 다 보내기 전에 기한이 지나,
// 여러 인스턴스로 배포했을 때 다른 인스턴스가 그것을 다시 가져가 두 번 보낸다. 이 세 상수는 서로 다른 곳에 있어서
// 어느 하나를 바꿔도 아무 오류 없이 관계가 깨질 수 있다. 그래서 여기서 고정한다.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("알림 채널 하나의 한 회차 최악 소요 시간 %v이(가) 선점 기한 %v 이상임"+
			"(notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v). "+
			"세 상수 중 하나를 바꾸면 나머지 둘도 함께 확인해야 함",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
