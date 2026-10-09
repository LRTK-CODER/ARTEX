package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 IM 알림 발송의 알림 채널 설정과 이벤트 계층이다. 전달 작업의 할당과
// 상태 전이는 db/notification_delivery.go에 있다.
//
// 이 파일을 고칠 때 반드시 지킬 불변식 두 가지:
//
//  1. 취약점을 쓰는 트랜잭션(RecordFindingTx)은 InsertNotificationEventTx로 한 번
//     무조건 삽입만 하고, 알림 관련 테이블을 읽거나 필터 일치를 검사하지 않는다.
//     여기에 읽기를 넣으면 사용자가 잘못 설정한 필터 조건 때문에 취약점 쓰기
//     트랜잭션이 오염되거나 중단될 수 있다.
//  2. 필터 일치 검사는 절대 오류를 내지 않는다. 형식이 잘못된 설정은 모두 '일치'로
//     처리한다(notify.Match 참고). 알림을 빠뜨리느니 더 보내는 편이 낫다.

// ErrNotificationChannelNotFound 는 알림 채널이 없을 때 올린다.
var ErrNotificationChannelNotFound = errors.New("알림 채널이 없습니다")

// 전달 상태.
const (
	NotifyStatePending = "pending" // 발송 대기
	NotifyStateSending = "sending" // 어느 dispatcher가 할당받았고 선점 기한이 아직 남음
	NotifyStateSent    = "sent"    // 전달됨
	NotifyStateFailed  = "failed"  // 재시도를 모두 썼거나 영구 실패. 수동으로 재발송할 수 있음
	NotifyStateSkipped = "skipped" // 알림 채널이 꺼져 더 보내지 않음
)

// 알림 발송 모드.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode 는 알림 발송 모드를 허용 목록으로 검사한다(findings.status와 같은
// 이유로 DB CHECK를 쓰지 않는다. 나중에 늘리기 쉽게 하려는 것이다).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel 은 알림 채널 하나의 설정이다. Config와 Filter는 원래 JSON 그대로
// 두고 파싱은 notify 패키지에 맡긴다. db 계층은 그 필드의 뜻을 모른다.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled를 포인터로 두는 것은 '필드를 보내지 않음'과 '명시적으로 false를 보냄'을
	// 구분하기 위해서다. 프런트엔드 토글은 바뀐 필드만 제출한다.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled 는 알림 채널이 사용 중인지 돌려준다. Enabled가 nil(불러오지 않음)이면 사용 중으로 본다.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent 는 일어난 이벤트 하나의 기록이다.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels 는 모든 알림 채널을 돌려준다. 사용 중인 채널이 앞에 오고,
// 같은 그룹 안에서는 id 순이다. 정렬을 SQL에 두는 것은 UI와 dispatcher가 같은 안정된
// 순서를 보게 하기 위해서다.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID 는 알림 채널 하나를 읽는다.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel 은 알림 채널을 새로 만들거나 고친다.
//
// 고칠 때는 호출자가 명시적으로 준 필드(nil이 아니거나 비어 있지 않은 것)만 덮어쓴다.
// 그래야 프런트엔드가 일부만 고친 편집 패널 폼을 제출할 수 있고, config에서 화면에 보이지
// 않은 필드까지 되돌려 보낼 필요가 없다. 되돌려 보내면 오히려 '마스킹 값이 실제 키를
// 덮어쓰는' 사고가 난다.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// 여기서는 일부러 0을 **손대지 않는다**. 0은 올바른 설정이고 '발송 속도 제한 없음'을 뜻한다.
	//
	// 예전에는 `if c.RatePerMin <= 0 { c.RatePerMin = 기본값 }`으로 썼다. '지정하지 않았으면
	// 안전한 기본값을 준다'는 뜻이었지만 '명시적으로 0으로 설정'까지 함께 삼켰다.
	// 문서, UI 안내, takeTokens는 모두 0을 제한 없음으로 해석하는데 여기서만 몰래
	// 20(DingTalk/WeCom/Telegram)이나 100(Feishu(Lark))으로 바꿨다. 운영자는 제한을
	// 풀었다고 생각했지만 실제로는 분당 20건에 묶였고 아무 안내도 없었다.
	//
	// '지정하지 않음'과 '명시적 0'의 차이는 호출자만 안다(요청 본문에 필드가 없음 vs
	// 0을 명시). 그래서 기본값은 필드가 없을 때 server 계층이 채운다. notifyCreateChannel 참고.
	if c.RatePerMin < 0 {
		return 0, errors.New("발송 속도 제한 값은 음수일 수 없습니다")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled 는 알림 채널을 켜거나 끈다.
//
// 알림 채널을 끌 때 아직 보내지 않은 전달도 함께 skipped로 표시한다. 그러지 않으면
// 다시 켰을 때 '꺼져 있는 동안 쌓인' 옛 취약점이 한꺼번에 도착한다. 이미 때가 지났고
// 새로 생긴 취약점으로 오해하기 쉽다.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "알림 채널이 꺼졌습니다", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel 은 알림 채널을 삭제한다. 그 전달 기록은 외래 키 연쇄 삭제로
// 함께 지워진다(채널 설정이 없으면 기록을 해석할 수 없다).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx 는 호출자의 트랜잭션 안에서 알림 발송 이벤트 하나를 **할 수
// 있는 만큼** 쓴다.
//
// 취약점 쓰기 경로에서 알림과 관련된 변경은 이것 하나뿐이다. INSERT 한 번이고, 어떤
// 테이블도 읽지 않고, 알림 채널을 모르고, 필터를 돌리지 않는다. 트랜잭션이 커밋되면
// '취약점 저장'과 '알림 발송 작업 존재'가 원자적으로 맞아떨어진다. 커밋은 됐는데
// 큐에 들어가지 않아 메시지가 영영 사라지는 틈이 없다.
//
// 핵심 설계 두 가지. 둘 다 이유가 있다.
//
//  1. **SAVEPOINT를 쓰는 이유**: PostgreSQL에서는 트랜잭션 안의 어느 문장이든 오류가
//     나면 트랜잭션 전체가 aborted 상태가 되고, 그 뒤 모든 문장(COMMIT 포함)이
//     실패한다. 그래서 '이 INSERT의 오류는 무시하고 호출자가 커밋을 이어 가게
//     한다'는 PG에서 할 수 없다. 세이브포인트로 오류를 이 한 문장에 가둘 때만 된다.
//     세이브포인트가 없으면 '전체 롤백' 하나만 남는다.
//
//  2. **전체 롤백이 틀린 이유**: 알림 발송은 편의 기능이고 취약점 기록이 제품 자체다.
//     알림 테이블의 문제(마이그레이션하지 않은 옛 DB, 디스크 일시 장애) 때문에 높음
//     등급 취약점이 저장되지 않아서는 안 된다. 그래서 여기서는 오류를 가두고 로그를
//     남기고 false를 돌려줘 취약점 쓰기는 그대로 커밋되게 한다. 대가는 이 알림 하나를
//     잃는 것이다. error 대신 bool을 돌려주는 것도 일부러다. 호출자가 이것을 쓰기
//     성패를 좌우하는 오류로 다루면 안 된다.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 알림 발송 이벤트 직렬화 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 세이브포인트 만들기 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 알림 발송 이벤트 쓰기 실패 finding=%d(취약점 기록은 영향 없음): %v", findingID, err)
		// 세이브포인트로 롤백해 트랜잭션을 aborted 상태에서 되살린다.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 세이브포인트 롤백 실패 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// 긴 트랜잭션에 쓸모없는 세이브포인트가 쌓이지 않게 세이브포인트를 해제한다.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent 는 InsertNotificationEventTx의 독립 트랜잭션 버전이다. 기존
// 트랜잭션 밖에서 부르는 곳(실제 finding이 없는 알림 채널의 '테스트 메시지 보내기'
// 등)이 쓴다.
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("알림 이벤트 스냅숏 직렬화: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents 는 아직 분배하지 않은 취약점 이벤트를 지금 사용 중인 알림 채널별
// 전달 작업으로 펼치고, 이번에 처리한 이벤트 수와 새로 만든 전달 수를 돌려준다.
//
// 한 번의 처리 전체가 트랜잭션 하나 안에서 돈다. 이벤트는 FOR UPDATE SKIP LOCKED로
// 할당받으므로 여러 프로세스가 동시에 돌아도 저마다 다른 행을 할당받는다(보관 큐의
// 할당도 같은 방식이다. db/task_archives.go의 completeNextArchiveJob 참고).
//
// 필터 일치 검사는 일부러 SQL이 아니라 Go 쪽에 둔다. 알림 채널의 필터 조건은 선택
// 필드 묶음으로 된 JSONB라서 여섯 가지 조합의 일치를 SQL로 쓰면 질의를 유지보수하기
// 어렵다. 알림 채널은 '사람이 손으로 설정한 몇 개'뿐이라 모두 불러와 메모리에서
// 하나씩 비교하는 편이 빠르고 테스트하기도 쉽다.
//
// 어떤 알림 채널과도 일치하지 않은 이벤트도 fanned_out으로 표시한다. 그러지 않으면
// 분배 대기 집합에 영원히 남아 tick마다 다시 훑게 된다.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋에 성공한 뒤에는 no-op이다

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// 스냅숏은 우리가 직접 쓴 것이라 이론상 반드시 파싱된다. 파싱에 실패해도 전달
		// 흐름을 막지 않는다. 다만 이 이벤트는 필드가 모두 비어 필터 조건이 있는 알림
		// 채널에서는 모두 건너뛴다. 알림 하나를 덜 보내더라도 잘못된 행 하나가 큐 전체를
		// 멈추게 하지 않는다.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind는 행의 값을 따른다. 스냅숏의 kind는 렌더링용 사본이라 이전 버전이 썼을 수 있다.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// 이번에 처리한 이벤트를 분배됨으로 표시한다. 어떤 알림 채널과도 일치하지 않은 이벤트도 함께 표시한다(함수 주석 참고).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx 는 트랜잭션 안에서 사용 중인 알림 채널을 읽는다.
// 개수가 적어 페이지 나누기도 캐시도 하지 않는다. 캐시를 두면 '설정을 바꾸면 언제
// 적용되는가'라는 시점 문제가 하나 더 생긴다.
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames 는 자산 id를 알림 메시지에 쓸 짧은 표시 이름으로 바꾼다.
//
// 돌려주는 순서는 인자 순서와 같고, 길이는 인자보다 짧을 수 있다(없는 id는 건너뛴다).
// 인자 순서를 지키는 것은 같은 취약점의 메시지가 여러 번 전달돼도 자산 순서가
// 그대로이게 하기 위해서다. 그러지 않으면 재시도 뒤 받은 메시지의 자산 순서가 바뀌어
// '자산이 바뀌었다'로 잘못 읽힌다.
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName 은 자산 유형에 따라 가장 알아보기 쉬운 식별자를 고른다.
// 고를 것이 없으면 빈 문자열을 돌려주고, '이름을 얻지 못한 자산'을 어떻게 보일지는
// 호출자가 정한다. 이 함수가 자리표시 이름을 지어내지 않는 것은 '자산#42' 같은
// 불필요한 문구가 알림 메시지에 섞여 읽는 사람이 실제 도메인으로 오해하지 않게 하기 위해서다.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify 는 취약점 처리 상태를 바꾸고, 같은 트랜잭션에서 상태 변경
// 알림 발송 이벤트 하나를 등록한다.
//
// from은 바꾸기 전 상태, found는 취약점이 있는지, notified는 이벤트 등록에 성공했는지다.
//
// 일부러 정한 동작 세 가지:
//   - 상태가 실제로 바뀌지 않으면 이벤트를 등록하지 않는다. 프런트엔드 편집 패널이 같은
//     값을 다시 제출하거나 자동화 스크립트가 멱등하게 다시 보내도 불필요한 알림을
//     만들지 않아야 한다.
//   - 취약점이 없으면 found=false를 돌려주고 아무것도 쓰지 않는다. 404로 바꾸는 것은
//     호출자의 몫이다.
//   - 이벤트 등록 실패는 상태 변경에 영향을 주지 않는다(RecordNotificationEventTx의
//     세이브포인트 설명 참고). 그래서 notified=false여도 상태는 이미 바뀌었고, 호출자는
//     이 때문에 오류를 내면 안 된다.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx 는 **호출자의 트랜잭션** 안에서 취약점 상태를 바꾸고 상태 변경 알림
// 발송 이벤트를 등록한다.
//
// 트랜잭션 단위 함수로 뺀 것은 상태를 바꾸는 모든 경로가 같은 동작을 쓰게 하기
// 위해서다. 예전에는 patchFinding만 알림이 붙은 버전을 썼고, **재검사 결론이
// '수정됨'일 때**(finding_retests의 `UPDATE findings SET status=...`)는 DB에 바로
// 썼다. 그래서 `on_status_change`를 설정한 알림 채널은 이런 상태 전이 알림을 전혀
// 받지 못했다. 화면의 상태는 조용히 바뀌었고 운영자는 플랫폼을 열어 봐야 알았다.
//
// from은 바꾸기 전 상태, found는 취약점이 있는지, changed는 상태가 실제로 바뀌었는지,
// notified는 이벤트 등록에 성공했는지다(등록 실패는 상태 변경에 영향을 주지 않는다.
// RecordNotificationEventTx 참고).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 상태가 실제로 바뀌지 않았으면 이벤트를 등록하지 않는다. 같은 값을 다시
		// 제출하거나 멱등하게 다시 보내도 불필요한 알림을 만들지 않아야 한다.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats 는 알림 페이지 위쪽의 요약 집계다.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 가장 오래된 발송 대기 전달이 생긴 지 지난 밀리초
}

// NotificationStatsSnapshot 은 알림 시스템의 상태를 집계한다.
// BacklogAgeMS는 '알림 발송이 멈췄는가'를 가장 바로 보여 주는 지표다. pending 개수보다
// 훨씬 쓸모 있다. 같은 3건이 밀려 있어도 3초 묵은 것과 3시간 묵은 것일 수 있기 때문이다.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
