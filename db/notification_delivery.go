package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 이 파일은 전달 작업의 할당과 상태 전이를 다룬다.
//
// 할당은 긴 트랜잭션이 아니라 「선점 기한」으로 한다. 행을 sending으로 바꾸고
// next_attempt_at을 미래로 밀어 선점 기한 만료 시각으로 삼은 뒤, 트랜잭션을 커밋하고
// 나서 네트워크 전달을 한다. 그래서 전달하는 동안 DB 잠금을 쥐고 있지 않다. 네트워크
// 요청은 몇 초 걸릴 수 있고(클라이언트 시간 초과 15초), 행 잠금을 계속 쥐고 있으면 같은
// DB의 다른 쓰기가 느려진다.
//
// 대가는 프로세스가 전달 도중 죽으면 행이 sending에 머문다는 것이다. 이것은 **스스로
// 복구된다**. 선점 기한이 지나면 next_attempt_at이 과거가 되고, 다음 할당이 같은 행을
// 다시 가져간다(할당 조건의 state IN ('pending','sending') 참고). 재시도 횟수는 할당할
// 때 이미 1 늘리므로 죽어도 무한 재시도가 되지 않는다. MaxNotifyAttempts번을 다 쓰면
// failed가 되어 사람의 처리를 기다린다.

// MaxNotifyAttempts 는 전달 하나의 최대 시도 횟수다(첫 시도 포함).
// 전달 엔진이 아니라 여기에 두는 것은 상태 기계 자체의 정책이기 때문이다. 엔진은
// 실행만 한다.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize 는 다이제스트 배치 하나가 한 번에 합치는 전달의 최대 개수다.
//
// 이 값이 있는 이유는 자원이다. 다이제스트 주기 하나에 취약점이 수만 개 나오면(전체
// 스캔 한 번이면 충분히 그렇게 된다), 상한이 없을 때 할당이 모든 행을 메모리에 읽어
// 아주 긴 메시지 하나로 렌더링하고, 그 메시지는 알림 채널의 길이 상한에 걸려 대부분
// 잘린다. 메모리를 낭비할 뿐 아니라 잘린 취약점이 **조용히 사라진다**. 상한을 두면
// 넘친 부분은 DB에 남아 다음 배치가 되고, 다음 주기에 자연스럽게 나가므로 잃지 않는다.
//
// 500으로 정한 근거: 메시지로 렌더링했을 때 WeCom의 4096바이트 상한 안에서 아직 "읽을
// 내용이 있는" 정도의 양이다. 더 키워도 잘리는 위치가 뒤로 밀릴 뿐이다.
const MaxDigestBatchSize = 500

// NotificationDelivery 는 전달 작업 하나다. 렌더링에 필요한 알림 채널 설정과 이벤트
// 스냅숏을 담는다.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 함께 불러온 렌더링 컨텍스트. JSON에는 넣지 않는다(DTO는 server 계층이 만든다).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind는 이벤트에서 가져온다. 기록 목록에서 취약점 상세로 바로 이동하는 데 쓴다.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind는 목록 표시용 중복 필드다. 프런트엔드가 한 번 더 조회하지 않게 한다.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery 는 전달 행을 읽는 공통 형태다. 전달 + 이벤트 스냅숏 + 알림 채널
// 설정. 메시지 하나를 렌더링하려면 셋이 모두 필요하고, 따로 조회하면 왕복이 세 번 생긴다.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery 는 할당 한 번을 나타낸다. 먼저 sel로 후보를 골라 잠그고, 그것들을
// sending으로 바꾸며 선점 기한을 늘린다. sel 안의 lease 자리는 호출자가 $n 자리표로
// 두고 직접 인자를 넘긴다.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries 는 알림 채널 하나에서 시간이 된 실시간 전달을 최대 limit개
// 할당받는다.
//
// 일부러 「전체에서 한 묶음을 할당받고 골라 보내기」가 아니라 **알림 채널 하나씩**
// 할당받는다. 발송 속도 제한은 전달 엔진이 알림 채널별로 관리한다. 이번에 이 채널이 몇
// 건 더 보낼 수 있는지 먼저 알고 그만큼만 할당받아야 속도 제한이 재시도 횟수를 쓰지
// 않는다. 거꾸로 먼저 할당받고 버리면 속도 제한에 막힌 행은 이미 attempts가 한 번
// 세어진 상태라, 3번의 기회가 기다리기만 하다 바닥나 결국 failed가 된다.
//
// 조건에 「선점 기한이 지난 sending」이 들어 있다. 프로세스가 죽은 뒤 스스로 복구되는
// 경로다. lease는 전달 한 번에 걸리는 최악의 시간(알림 채널 HTTP 클라이언트 시간 초과
// 15초)보다 충분히 커야 한다. 그러지 않으면 같은 행을 두 dispatcher가 동시에 전달한다.
// 꺼진 알림 채널도 함께 걸러 낸다. 끄는 동작이 이미 남은 전달을 skipped로 표시했지만,
// 끄기와 할당이 동시에 일어날 때 빠져나가는 행이 없게 여기서 한 번 더 막는다.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue 는 이 알림 채널에 보낼 때가 된 배치가 모였는지 알려 준다. 발송 대기
// 전달이 있고, **가장 오래된 것**의 나이가 다이제스트 주기에 이르렀으면 참이다.
//
// 벽시계가 아니라 가장 오래된 전달의 나이로 판정한다. 그래야 막 만든 알림 채널이 정각에
// 맞춰졌다는 이유로 한 건짜리 「다이제스트」를 바로 내보내지 않고, 오래 쌓인 배치도 한
// 주기를 헛되이 더 기다리지 않는다.
//
// ClaimDigestBatch와 나눈 것은 뜻이 다르기 때문이다. 이 함수는 「보낼 때인가」에만
// 답하고, 할당은 이 알림 채널의 발송 대기 행을 **모두**(아직 나이가 덜 찬 것 포함)
// 가져간다. 그러지 않으면 한 주기가 여러 메시지로 쪼개져 다이제스트의 의미가 없어진다.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch 는 알림 채널 하나에서 지금 보낼 때가 된 발송 대기 전달을 다이제스트
// 배치 하나로 할당받는다. 배치 하나는 최대 MaxDigestBatchSize개다.
//
// 같은 배치의 전달은 모두 batch_id를 공유하고, 집합에서 가장 작은 id를 배치 번호로
// 쓴다(안정적이고 읽기 쉽고 별도 시퀀스가 필요 없다). 재시도할 때 COALESCE로 원래 배치
// 번호를 유지해, 여러 번 재시도한 뒤에도 「이 N건은 함께 보냈다」가 그대로 성립한다.
//
// 무작위가 아니라 id 오름차순으로 앞의 N개를 가져간다. 가장 먼저 생긴 전달이 가장
// 먼저 나가므로, 밀린 알림이 있어도 「새 취약점이 먼저 나가고 옛 취약점은 영원히 뒤로
// 밀리는」 기아가 생기지 않는다.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit은 **메모리 상한**이고 호출자는 MaxDigestBatchSize를 넘긴다. 호출자가 더 큰
	// 값을 넘기지 못하게 여기서 한 번 더 자른다.
	//
	// 「발송 속도 제한 허용량」을 배치 크기로 받지 않는 것은 일부러다. 속도 제한의 단위는
	// 메시지 건수다(배치 하나는 메시지 하나를 보내고 토큰 하나를 쓰며, server 계층의
	// takeTokens가 차감한다). 「배치 하나에 취약점을 몇 개 담는가」와는 다른 양이다.
	// 예전에 rate_per_min을 digest에도 적용하려고 매번의 요청 허용량을 배치 크기로
	// 넘겼더니, rate=20/min인 알림 채널은 배치마다 취약점을 1개만 담았고 digest가 요약
	// 문구만 붙은 실시간 알림 발송으로 전락했다. 속도 제한을 바꾸려면 takeTokens의 want를
	// 고치고 여기는 건드리지 않는다.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries 는 「고르기 + sending으로 바꾸며 선점 기한 늘리기 + 전체 행 읽기」를
// 트랜잭션 하나 안에서 한다. postClaim은 선택적인 추가 단계다(다이제스트 배치가
// batch_id를 쓸 때 쓴다).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋에 성공한 뒤에는 no-op이다

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending으로 바꾸고 next_attempt_at을 미래로 민다. 그 미래 시각이 곧 선점 기한
	// 만료 시각이므로 「선점 기한이 남음」과 「아직 재시도 시각이 아님」을 같은 조건식으로
	// 나타낼 수 있고, 열을 새로 둘 필요가 없다.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent 는 전달 묶음을 전달됨으로 표시한다.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries 는 전달 묶음을 pending으로 되돌리고 재시도 시각을 뒤로 민다.
//
// 새 중간 상태를 두지 않고 pending으로 되돌리는 것은 「기회가 몇 번 남았나」를 한곳
// (MaxNotifyAttempts)에서만 나타내, 재시도 정책에 따라 상태 기계의 분기가 늘어나지
// 않게 하기 위해서다.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries 는 전달 묶음을 pending으로 되돌려 바로 다시 할당받을 수 있게 하고,
// **할당할 때 센 시도 한 번을 되돌린다**.
//
// 쓰는 곳은 하나뿐이다. 다이제스트 메시지를 알림 채널의 길이 상한에 맞춰 나눠 보낼 때,
// 이번 메시지에 들어가지 못한 항목은 다음 배치로 남겨야 한다. 그것은 실패가 아니므로
// 재시도 기회를 쓰면 안 된다. 할당할 때 attempts를 낙관적으로 이미 1 늘렸으니 여기서
// 반드시 되돌린다. 그러지 않으면 밀린 알림 500건이 20건씩 25개로 나뉘고, 뒤쪽 항목은 한 번도
// 오류가 난 적이 없는데도 셋째 묶음에서 MaxNotifyAttempts에 걸려 failed가 된다.
//
// GREATEST(...,0)는 「누가 수동으로 재발송해 attempts를 0으로 만든 뒤 여기로 온」
// 경우를 받아 내, 횟수가 음수가 되지 않게 한다.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries 는 전달 묶음을 최종 실패로 표시한다. 사람이 전달 기록에서 재발송하기를 기다린다.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 자리표는 $3부터 시작한다. $1은 state, $2는 last_error다.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery 는 전달 하나를 수동으로 재발송한다. pending으로 되돌리고
// 재시도 횟수를 0으로 만들고 바로 보낼 수 있게 한다. 횟수를 0으로 만드는 것은
// 일부러다. 사람이 「재발송」을 눌렀다면 앞선 실패의 원인은 이미 처리됐다는 뜻이므로,
// 옛 횟수로 제한할 이유가 없다.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("전달 %d이(가) 없거나 지금 상태에서는 재발송할 수 없습니다", id)
	}
	return nil
}

// NotificationDeliveryFilter 는 전달 기록의 조회 조건이다.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries 는 전달 기록을 페이지 단위로 돌려준다. 최신 것이 앞에 온다.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError 는 오류 메시지를 열이 받을 수 있는 길이로 자른다. 알림 채널이
// 돌려주는 응답 본문은 길 수 있고(일반 Webhook을 직접 운영하는 서비스로 보낼 때
// 특히 그렇다), 자르지 않으면 기록 목록의 응답 크기가 불어난다.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 문자 경계까지 물러나 UTF-8 문자 반쪽이 남아 프런트엔드에서 깨져 보이지 않게 한다.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders 는 IN (...)에 쓸, start부터 시작하는 $n 자리표 문자열과 그 인자를 만든다.
// 예: start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
