package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일의 테스트는 모두 실제 PostgreSQL에 연결한다(DB가 없으면 건너뛴다). 여기 SQL은
// FOR UPDATE SKIP LOCKED, make_interval, JSONB, 여러 행 IN(...) 자리표 이어 붙이기를
// 쓴다. 모두 「컴파일은 되지만 실행 중 오류가 날 수 있는」 형태라 실제로 돌려 봐야
// 검증했다고 할 수 있다.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel 은 알림 채널 하나를 만들고 테스트가 끝나면 자동으로 지운다.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "테스트 채널-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("알림 채널 만들기 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent 는 이벤트 하나를 finding 없이 바로 쓴다. 분배와 전달 테스트에 쓴다.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("이벤트 쓰기 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 세 종류의 자산은 표시 기준이 저마다 다르다. 도메인, IP, URL.
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 넘기는 순서를 일부러 섞고, 없는 id도 하나 넣는다.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("자산 이름 조회 실패: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("자산 이름 = %v, 기대값 %v (개수가 다름)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("자산 이름 = %v, 기대값 %v (순서나 값이 다름)", got, want)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure 는 세이브포인트 동작의 핵심 테스트다.
// 트랜잭션 안에서 notification_events 쓰기가 반드시 실패하게 만들고(늘 false인 제약을
// 잠시 더한다), ① 이 함수가 false를 돌려주고 ② 트랜잭션이 aborted 상태가 되지 않아
// 다음 문장을 계속 실행할 수 있음을 확인한다.
//
// 세이브포인트가 없으면 PostgreSQL은 트랜잭션 전체를 무효로 만들고, 그 뒤 모든 문장이
// "current transaction is aborted"로 실패한다. 그것이 바로 「알림 테이블 문제 때문에
// 취약점이 저장되지 않는」 장애 경로다.
//
// 여기서는 일부러 **COMMIT이 아니라 ROLLBACK으로 끝낸다**. PG에서 ALTER TABLE은
// 트랜잭션 안에서 동작하므로, 커밋하면 그 임시 제약이 스키마에 영구히 남아 뒤의 모든
// 테스트를 깨뜨린다. 롤백하면 DDL이 자동으로 취소되어 손으로 정리할 필요가 없다.
// 확인할 것은 「트랜잭션이 아직 살아 있다」뿐이라 실제로 커밋할 필요는 없다.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 방어적 정리: 예전 실행이 이 제약을 남겼으면 먼저 없앤다.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 임시 제약을 취소한다. 함수 주석 참고

	// NOT VALID: 이후에 쓰는 행만 제약하고 DB에 이미 있는 이벤트는 검사하지 않는다
	// (그러지 않으면 기존 행이 위반이라 제약을 더할 수 없다).
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("임시 제약 추가 실패: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("반드시 실패하는 제약 아래에서 쓰기 성공을 보고함")
	}
	// 핵심 확인: 트랜잭션을 아직 쓸 수 있다.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("트랜잭션이 오염됨(세이브포인트가 동작하지 않음): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("롤백 실패: %v", err)
	}
	// DDL이 롤백과 함께 취소됐는지 확인해 뒤 테스트에 문제를 남기지 않는다.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("임시 제약이 롤백으로 취소되지 않음. 뒤 테스트를 오염시킨다")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL 인젝션"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("분배 실패: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"전체 수신 채널이 high를 받음", highSQL, all.ID, true},
		{"전체 수신 채널이 low를 받음", lowXSS, all.ID, true},
		{"치명 전용 채널이 high를 건너뜀", highSQL, onlyCritical.ID, false},
		{"치명 전용 채널이 critical을 받음", criticalXSS, onlyCritical.ID, true},
		{"SQL 전용 채널이 SQL을 받음", highSQL, sqlOnly.ID, true},
		{"SQL 전용 채널이 XSS를 건너뜀", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("전달 있음 = %v, 기대값 %v", exists, tc.want)
			}
		})
	}

	// 한 번 더 분배해도 전달이 중복되면 안 된다(fanned_out 멱등성).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("분배된 이벤트를 다시 처리함: events=%d deliveries=%d, 기대값 0", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel 은 「이벤트가 어떤 알림 채널과도 일치하지
// 않는」 경우를 다룬다. 이런 이벤트도 분배됨으로 표시해야 한다. 그러지 않으면 분배 대기
// 집합에 영원히 남아 tick마다 다시 훑게 된다.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["절대 일치하지 않는 유형"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("전달 수 = %d, 기대값 0", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("알림 채널과 일치하지 않은 이벤트도 분배됨으로 표시해야 함. 그러지 않으면 끝없이 다시 훑는다")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 실시간 할당은 realtime 채널의 전달만 가져가고 digest 채널의 것은 건드리지 않아야 한다.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("할당 실패: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("할당받은 개수 = %d, 기대값 1", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("할당 뒤 state=%s attempts=%d, 기대값 sending, 1", got[0].State, got[0].Attempts)
	}
	// 함께 불러온 렌더링 컨텍스트가 모두 있어야 한다(알림 채널 설정 + 이벤트 스냅숏 + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("할당 결과에 알림 채널 설정이 없음. 렌더링이 실패한다")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id = %d, 기대값 3001 (이벤트에서 가져오지 못함)", got[0].FindingID)
	}

	// 선점 기한이 남았으므로 두 번째 할당은 비어 있어야 한다. 이것이 「같은 행을 두
	// dispatcher가 동시에 전달하지 않는다」는 보장이다.
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("선점 기한 안의 재할당 개수 = %d, 기대값 0", len(again))
	}

	// digest 채널의 전달은 실시간 할당이 건드리면 안 된다.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("실시간 할당이 가져간 digest 채널 전달 수 = %d, 기대값 0", len(left))
	}
}

// TestClaimExpiredLeaseRecovers 는 프로세스가 죽은 뒤의 자동 복구를 다룬다. 전달 도중
// 프로세스가 죽으면 sending 행이 남는다. 선점 기한이 지나면 다시 할당받을 수 있어야
// 한다. 그러지 않으면 이 전달은 영원히 멈춘다.
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("첫 할당 실패: %v (%d개)", err, len(first))
	}
	// 선점 기한을 손으로 과거로 밀어 「선점 기한 지남」을 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("선점 기한이 지난 sending 행의 재할당 개수 = %d, 기대값 1", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("재할당 뒤 attempts = %d, 기대값 2", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 끄면 남아 있던 발송 대기 전달도 함께 skipped로 표시된다.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("꺼진 알림 채널의 남은 전달 state = %s, 기대값 skipped", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("꺼진 알림 채널에서 할당받은 개수 = %d, 기대값 0", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 배치가 막 생겨 나이가 0이므로 30분 주기에서는 보낼 때가 아니어야 한다.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("배치 발송 시점 판정 실패: %v", err)
	}
	if due {
		t.Fatal("막 생긴 배치를 바로 보낼 때로 판정함")
	}

	// 전달 세 개의 생성 시각을 함께 과거로 밀어 주기를 채운 배치를 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("주기를 넘긴 배치를 보낼 때로 판정하지 않음")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("다이제스트 배치 할당 실패: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("다이제스트 할당 개수 = %d, 기대값 3 (한 번에 모두)", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("다이제스트 배치에 batch_id가 없음. 기록에서 함께 보낸 것인지 알 수 없다")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("batch_id = %v, 기대값 %d (같은 배치는 공유해야 함)", dl.BatchID, firstBatchID)
		}
	}

	// 이 배치를 **통째로** 실패시켜 다시 예약한 뒤 다시 할당받는다. batch_id는 원래 값을
	// 유지해야 한다(COALESCE의 역할). 그러지 않으면 재시도 한 번에 「이 배치는 함께
	// 보냈다」는 사실이 지워진다.
	//
	// 하나만이 아니라 배치 전체를 다시 예약해야 한다. 전달 엔진이 다이제스트 메시지를
	// 보낼 때 그렇게 처리한다(메시지 하나가 배치 전체를 대표하고 성패를 함께한다).
	// 하나만 다시 예약하면 나머지는 아직 선점 기한 안이라 재할당이 그 하나만 가져간다.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "실패 흉내"); err != nil {
		t.Fatal(err)
	}
	// 선점 기한을 과거로 밀어 백오프 시간이 지난 것을 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("재할당 개수 = %d, 기대값 3", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("재시도 뒤 batch_id = %v, 기대값 %d", reclaimed[0].BatchID, firstBatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("할당 실패: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "네트워크 일시 오류"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "네트워크 일시 오류" {
		t.Fatalf("다시 예약한 뒤 state=%s err=%q, 기대값 pending과 기록된 원인", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "재시도 소진"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("state = %s, 기대값 failed", state)
	}

	// 수동 재발송은 재시도 횟수를 0으로 만들고 바로 보낼 수 있게 해야 한다. 그러지 않으면 옛 실패 횟수를 물려받는다.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("재발송 실패: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("재발송 뒤 state=%s attempts=%d, 기대값 pending, 0", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("재발송한 전달을 바로 할당받을 수 없음")
	}

	// 전달된 전달은 재발송할 수 없어야 한다.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("전달된 전달의 재발송을 허용함")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "페이지 나누기 테스트"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if total != 5 {
		t.Fatalf("전체 개수 = %d, 기대값 5", total)
	}
	if len(page1) != 2 {
		t.Fatalf("페이지 크기 = %d, 기대값 2", len(page1))
	}
	// 최신 것이 앞에 온다. 첫 페이지 첫 항목의 id가 둘째 페이지 첫 항목보다 커야 한다.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("페이지 순서가 최신순이 아님: page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// 렌더링 컨텍스트가 기록과 함께 와야 한다. 그러지 않으면 목록이 「무엇을 보냈는지」 보여 주지 못한다.
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("기록 항목에 표시 필드가 없음: %+v", page1[0])
	}

	// 상태로 거르기: pending은 없다.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending 전달 수 = %d (total=%d), 기대값 0", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("알림 상태 변경 테스트", "대상", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 인젝션", Name: "상태 변경 테스트 항목",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 저장할 때 finding_created 이벤트 하나가 이미 등록됐으므로 먼저 세어 기준값으로 삼는다.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("취약점 저장 트랜잭션에서 알림 발송 이벤트가 등록되지 않음")
	}

	// 같은 상태로 바꾸면 이벤트가 생기면 안 된다(같은 값을 다시 제출해 불필요한 알림이 생기지 않게).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("상태가 바뀌지 않았는데 알림 발송 이벤트를 등록함")
	}
	if from != "pending" {
		t.Fatalf("변경 전 상태 = %q, 기대값 pending", from)
	}

	// 실제로 바꾸면 이벤트를 등록하고 from/to를 기록해야 한다.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("상태 설정 실패: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("상태가 실제로 바뀌었는데 알림 발송 이벤트를 등록하지 않음")
	}
	if from != "pending" {
		t.Fatalf("from = %q, 기대값 pending", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("상태 변경 이벤트를 찾지 못함: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("스냅숏의 상태 전이 = %s → %s, 기대값 pending → fixed", snap.FromStatus, snap.ToStatus)
	}
	// 스냅숏에 렌더링에 필요한 필드가 있어야 한다. 그러지 않으면 상태 변경 메시지가 빈 껍데기가 된다.
	if snap.VulnClass != "SQL 인젝션" || snap.Severity != "high" || snap.Name != "상태 변경 테스트 항목" {
		t.Fatalf("스냅숏에 렌더링 필드가 없음: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("status = %s, 기대값 fixed", status)
	}

	// 없는 취약점: found=false이고 오류가 없어야 한다.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("없는 취약점: found=%v err=%v, 기대값 found=false, 오류 없음", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("집계 실패: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("알림 채널 개수가 맞지 않음: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("발송 대기 전달이 집계되지 않음: %+v", stats)
	}
	// 막 만든 전달의 밀린 전달 나이는 음수나 아주 큰 값이 아니라 0에 가까워야 한다.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("밀린 전달 나이가 잘못됨: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 왕복",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("새로 만들기 실패: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("읽기 실패: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 왕복" {
		t.Fatalf("왕복한 필드가 다름: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("기본값이 사용 중이 아님")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("설정이 제대로 저장되지 않음: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("필터 조건이 제대로 저장되지 않음: %+v", filter)
	}

	// 고친 뒤 다시 읽는다.
	got.Name = "이름 바꿈"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "이름 바꿈" || after.IsEnabled() {
		t.Fatalf("변경이 적용되지 않음: %+v", after)
	}

	// 삭제한 뒤에는 조용히 성공하지 말고 「없음」을 알려야 한다.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("오류 = %v, 기대값 ErrNotificationChannelNotFound", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("다시 삭제한 오류 = %v, 기대값 ErrNotificationChannelNotFound", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate 는 예전에 잘못 썼던 곳을 고정한다.
// **0은 올바른 설정이고 「발송 속도 제한 없음」을 뜻한다. db 계층이 이것을 「지정하지
// 않음」으로 보고 기본값으로 덮어쓰면 안 된다**.
//
// 예전 버그: SaveNotificationChannel에 `if RatePerMin <= 0 { 기본값 사용 }`이 있었다.
// 문서, UI 안내, takeTokens는 모두 「0=제한 없음」으로 해석하는데 DB에 쓰는 이 계층만
// 몰래 20(DingTalk/WeCom/Telegram)이나 100(Feishu(Lark))으로 바꿨다. 운영자는 제한을
// 풀었다고 생각했지만 실제로는 묶여 있었고 아무 안내도 없었다. 「지정하지 않음」과
// 「명시적 0」의 차이는 요청 본문만 나타낼 수 있으므로 기본값은 server 계층이 채우고
// (notifyCreateChannel 참고) db 계층은 저장만 한다.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 명시적 0(제한 없음)은 그대로 저장해야 한다.
	unlimited := &NotificationChannel{
		Name: "속도 제한 없음", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("RatePerMin = %d, 기대값 0 (명시적 0은 제한 없음이라 그대로 저장해야 함)", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("기본 모드 = %s, 기대값 realtime", got.Mode)
	}

	// 음수는 잘못된 입력이므로 몰래 다른 값으로 바꾸지 말고 거부해야 한다.
	bad := &NotificationChannel{
		Name: "음수 속도 제한", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("음수 발송 속도 제한을 거부하지 않음")
	}
}

// TestDeleteChannelCascadesDeliveries 는 외래 키 동작을 고정한다. 알림 채널을 삭제하면
// 그 전달 기록도 함께 사라지지만(설정이 없으면 기록을 해석할 수 없다), 이벤트 자체는
// 남아야 한다. 다른 알림 채널이 아직 참조할 수 있기 때문이다.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("전제 조건 불충족: 전달이 생기지 않음")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("알림 채널 삭제 뒤 남은 전달 수 = %d, 기대값 0 (연쇄 삭제)", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("알림 채널을 삭제하면서 이벤트까지 삭제함")
	}
}

// TestClaimDigestBatchHonorsCallerLimit 는 감사에서 지적된 빈틈 하나를 다룬다.
// 다이제스트 알림 채널은 예전에 토큰 버킷을 완전히 우회했다. takeTokens가 allow를
// 차감했지만 아무도 쓰지 않아 rate_per_min이 digest 모드에 아무 효과가 없었다. 이제
// limit도 제약에 참여한다.
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3: 3개만 할당받고 나머지는 DB에 남는다.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("할당 실패: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("할당받은 개수 = %d, 기대값 3 (호출자 허용량)", len(got))
	}
	// limit=0은 이번 허용량을 다 썼다는 뜻이다. 하나도 할당받지 않고 오류도 없어야 한다.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("허용량 0일 때 할당 개수 = %d err=%v, 기대값 0, 오류 없음", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange 는 감사에서 지적된 완전성 빈틈 하나를 다룬다.
// 재검사 결론이 「수정됨」이면 상태는 실제로 바뀌지만, 그 UPDATE는 DB에 바로 써서
// 알림이 붙은 버전을 우회했다. 그래서 on_status_change를 설정한 알림 채널은 이런 상태
// 전이 알림을 전혀 받지 못했다. 화면의 상태는 조용히 바뀌었고 운영자는 플랫폼을 열어
// 봐야 알았다.
//
// 이 테스트는 「상태를 바꾸는 모든 경로가 상태 변경 이벤트를 등록한다」를 고정한다.
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("재검사 알림 발송 테스트", "대상", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL 인젝션", Name: "재검사 대상 항목",
		Severity: "high", Summary: "요약",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 재검사 기록을 하나 만들고 바로 완료 상태로 보낸다.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "재확인")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("재검사에 연결된 대화가 없음")
	}
	// 재검사는 먼저 running이 되어야 결론을 저장할 수 있다(실제 흐름과 같다).
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("재검사 시작 실패: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "수정됨", "증거"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("재검사 종료 실패: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("재검사가 수정됨으로 판정한 뒤 status = %s, 기대값 fixed", status)
	}

	// 핵심 확인: 상태 변경 이벤트가 하나 있고 from/to가 맞아야 한다.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("재검사가 수정됨으로 판정했는데 상태 변경 알림 발송 이벤트가 없음(on_status_change를 설정한 알림 채널이 받지 못한다): %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("스냅숏의 상태 전이 = %s → %s, 기대값 pending → fixed", snap.FromStatus, snap.ToStatus)
	}
	// 스냅숏에 렌더링에 필요한 필드가 있어야 한다. 그러지 않으면 보낸 알림이 빈 껍데기가 된다.
	if snap.Name != "재검사 대상 항목" || snap.Severity != "high" {
		t.Fatalf("스냅숏에 렌더링 필드가 없음: %+v", snap)
	}
}
