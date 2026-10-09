package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 전역 설정 키(settings 키-값 테이블에 두므로 테이블을 따로 만들지 않는다).
const (
	// settingNotifyEnabled 는 알림 발송 전체 스위치다. 기본값은 켜짐이다. 운영 중 문제가 생겼을 때 한 번에 멈추기 위한 것이지
	// 기능을 쓰는 조건이 아니다. 실제로 쓰는 조건은 '알림 채널을 설정했는가'다.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL 은 취약점 상세 링크를 만들 때 쓰는 외부 접속 주소다
	// (예: https://artex.example.com). 비워 두면 메시지에 상세 링크 버튼을 넣지 않는다.
	// 프로젝트에 재사용할 외부 주소 설정이 없어서 여기에 새로 둔다.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes 는 다이제스트 모드의 주기(분)다.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick 은 전달 엔진의 폴링 간격이다. 3초가 이 엔진이 낼 수 있는 실시간성의 한계이고,
	// '취약점 저장'부터 '메시지가 메신저에 도착'까지 걸리는 지연의 주된 원인이기도 하다.
	notifyTick = 3 * time.Second
	// notifyLease 는 전달을 할당받을 때의 선점 기한이다. 전달 한 번의 최악 소요 시간
	// (notify 패키지 HTTP 클라이언트의 시간 초과 15초)보다 충분히 길어야 한다. 그러지 않으면 같은 행을
	// dispatcher 두 개가 동시에 전달한다.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick 은 한 회차에 분배하는 이벤트 수를 제한한다. 알림 채널을 처음 켤 때
	// 쌓여 있던 과거 이벤트가 한꺼번에 전달 작업으로 펼쳐지지 않게 한다.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes 는 다이제스트 주기의 기본값이다.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick 은 알림 채널에 발송 속도 제한이 없을 때 한 회차의 전달 상한이다.
	// '발송 속도 제한 없는 알림 채널 + 한 번에 찾은 취약점 수천 개'가 한 회차의 루프를
	// 오래 막지 않게 하려고 둔다.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick 은 알림 채널 하나가 한 회차에 전달하는 최대 건수다.
	//
	// 이 상한은 **선점 기한**에서 거꾸로 계산한다. 할당받을 때 행에 선점 기한(notifyLease = 3분)을 건다.
	// 한 회차에 차례로 전달하는 건수가 많아 최악 소요 시간이 선점 기한을 넘으면, 뒤쪽 건은 보내기도 전에 선점 기한이 끝난다.
	// 프로세스 하나 안에서는 상관없다(Run은 goroutine 하나에서 차례로 돌고 tick이 겹치지 않는다). 하지만 **두
	// 프로세스가 같은 DB에 붙으면** 상대가 선점 기한이 지난 행을 다시 할당받아 중복 발송하고,
	// attempts를 두 번 올리며, 원래 프로세스가 아직 전달 중인데 실패로 판정한다.
	//
	// 값: 선점 기한 3분 / 한 번의 시간 초과 30초 = 6은 **선점 기한을 꼭 맞게 다 쓰는** 값이라 여유가 없어
	// 쓸 수 없다. 5로 두면 최악 소요 시간 150초에 30초 여유가 남는다. 이 관계는
	// TestNotifyTickBudgetFitsWithinLease가 고정한다. notifyLease,
	// notifySendTimeout, 이 값 중 어느 하나를 바꿔도 그 단언이 실패한다.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout 은 전달 한 번의 시간 초과다. 위 상수의 값도 이것으로 정해지며,
	// 둘을 곱한 값이 notifyLease를 넘으면 안 된다. TestNotifyTickBudgetFitsWithinLease 참고.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff 는 실패 재시도의 백오프 순서이고, 인덱스는 이미 시도한 횟수다.
// 기회 3번(첫 시도 포함)은 db.MaxNotifyAttempts와 맞물리므로 둘을 함께 바꿔야 한다.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier 는 취약점 알림 발송의 전달 엔진이다.
//
// Scheduler와 나란히 독립된 goroutine으로 돈다(server.New 참고). Scheduler의
// tick을 일부러 함께 쓰지 않는다. 알림 발송에 필요한 실시간성(3초)은 트리거의 업무 주기와 다르고,
// 둘의 실패가 서로 번지지 않아야 한다. 알림 발송이 막혀도 agent 트리거에 영향을 주면 안 된다.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu 는 buckets를 지킨다. 알림 채널 수가 적고 경합이 낮아 뮤텍스 하나로 충분하며,
	// 더 잘게 나눈 구조를 들일 가치가 없다.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket 은 알림 채널 하나의 토큰 버킷이다.
//
// '분마다 세고 0으로 되돌리는' 창 방식 대신 토큰 버킷을 쓰는 이유는 창 방식의 경계 효과가 나쁘기 때문이다.
// 창 끝에서 20건을 다 보내고 바로 다음 순간 다시 20건을 보내면, 플랫폼 입장에서는 1초 안에 40건이라
// 속도 제한에 걸린다. 토큰 버킷은 일정한 속도로 채워져 이런 몰림을 자연스럽게 피한다.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run 은 ctx가 끝날 때까지 돈다. server.New가 한 번 띄운다.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step 은 한 회차를 돈다. 먼저 새 이벤트를 분배하고, 그다음 시간이 된 작업을 전달한다.
//
// 어느 단계가 실패해도 로그만 남기고 루프를 끊지 않는다. 알림 시스템의 장애가 프로세스 수준 문제로 커지면 안 된다.
// tick마다 독립적이라 다음 회차에서 자연히 재시도한다.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] 이벤트 분배 실패: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] 알림 채널 읽기 실패: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// 토큰 버킷의 단위는 취약점 수가 아니라 **메시지 수**(HTTP 요청 수와 같다)다.
		// 실시간 모드에서는 둘이 같다(취약점 하나에 메시지 하나). 다이제스트 모드에서는 취약점 한 배치를
		// 메시지 하나로 합치므로 토큰을 하나만 쓴다.
		//
		// 두 모드 모두 토큰 버킷에 먼저 묻고 받은 양만큼 할당받는다. 순서를 바꾸면 발송 속도 제한에 막힌
		// 전달이 이미 재시도 횟수를 써 버린다.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan 은 다이제스트 알림 채널의 이번 회차 토큰 사용량과 배치 크기 상한을 돌려준다.
//
// 두 반환값은 **단위가 서로 다른 양**이다. 바로 이 때문에 함수로 따로 뺐다.
//
//   - tokens는 메시지 수다. 취약점 한 배치를 메시지 하나로 합쳐 HTTP 요청을 한 번 보내므로 늘 1이다.
//     그래서 rate_per_min은 digest에도 계속 적용된다(분당 다이제스트 메시지 최대 그만큼).
//   - claimLimit은 이 배치에 담을 최대 취약점 수다. 메모리 상한의 제약만 받고 요청 한도와는 상관없다.
//
// 예전에는 rate_per_min을 digest에 적용하려고 회차당 요청 한도
// (notifyMaxSendsPerChannelPerTick, 선점 기한에서 거꾸로 계산한 값)를 그대로 배치 크기로 넘겼다.
// 그 결과 rate_per_min=20인 알림 채널은 3초 tick에서 토큰이 1개만 채워져, 다이제스트
// 메시지마다 취약점이 1개만 담겼다. digest가 '다이제스트 문구만 붙은 실시간 알림 발송'으로 전락해 독자는
// '최근 30분 새 취약점 1개'를 줄줄이 받았고, db.MaxDigestBatchSize에는 영영 닿지 않았다.
//
// 이 증상은 종단 간 테스트로는 찾기 어렵다(기존 사례는 모두 충분히 큰 limit을 직접
// stepDigest에 넘겨 step 안의 허용량 계산을 건너뛴다). 그래서 결정을 여기에 모으고
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget로 직접 고정한다.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime 은 알림 채널 하나의 실시간 작업을 할당받아 전달한다. 취약점 하나에 메시지 하나다.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 실시간 전달 할당 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("알림 채널 유형 %q이(가) 등록되어 있지 않습니다", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 렌더링 실패는 로컬 데이터 문제라 재시도해도 나아지지 않는다.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest 는 배치 시간이 되면 알림 채널 하나의 발송 대기 전달을 메시지 하나로 모아 보낸다.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 다이제스트 배치 판단 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 다이제스트 배치 할당 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("알림 채널 유형 %q이(가) 등록되어 있지 않습니다", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// 스냅숏이 깨져 메시지에 들어가지 못한 전달은 명시적으로 실패 처리해야 한다. 그러지 않으면 이들은
	// included 밖에 남아 메시지에도 실패 목록에도 들어가지 않는다. 발송이 성공하면 이들의 상태는
	// 뒤이은 일괄 표시에서 빠져, 선점 기한이 지나 거듭 다시 할당될 때까지 sending에 영원히 머문다.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "이벤트 스냅숏을 파싱하지 못해 이 취약점을 메시지로 만들 수 없습니다"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 깨진 스냅숏 전달 실패 표시 실패 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 스냅숏을 파싱하지 못한 전달 %d건 건너뜀 channel=%d", len(skipped), ch.ID)
	}
	// 메시지에 들어간 것만 send에 넘긴다. included[i]와 msg.Items[i]는 정확히 대응하고,
	// send는 이 대응으로 '알림 채널이 앞 K건까지 담았다고 회신'한 결과를 올바른 전달 행에 반영한다.
	n.send(ctx, channel, cfg, msg, included)
}

// send 는 전달하고 결과에 따라 상태를 옮긴다.
//
// 같은 배치의 전달(다이제스트 모드에서는 수십 건일 수 있다)은 발송 결과 하나를 함께 쓴다. 전달되거나 배치 전체를 재시도한다.
// 한 건씩 재시도하지 않는다. 다이제스트 메시지는 하나라서 일부만 다시 보내면 배치의 의미가 흐트러진다.
//
// 유일한 예외는 **알림 채널 길이 상한 때문에 나뉘는 경우**다. 알림 채널이 실제로 앞 K건만 담았다고 회신하면
// K+1번째부터는 함께 성공으로 표시하지 말고 다음 배치로 남겨야 한다. 그러지 않으면 잘려 나간
// 취약점은 메시지에도 실패 목록에도 없이 완전히 사라진다.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 전달 한 번에 상한을 둔다. 어떤 알림 채널이 막혀 이번 회차의 나머지 알림 채널을 모두 붙잡지 않게 한다.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 알림 채널이 회신한 건수는 전달 수보다 클 수 없다. 실제로 그렇다면 렌더링 계층의 계산이 틀린 것이다.
			// 전부 전달된 것으로 처리하고 문제를 기록하는 편이 기록을 엉망으로 만드는 것보다 낫다.
			log.Printf("[notify] 알림 채널이 회신한 전달 건수 %d가 전달 수 %d를 넘음 channel=%s, 전부 전달된 것으로 처리",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 전달됨 표시 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 이 메시지가 알림 채널 길이 상한에 닿았다. 나머지는 바로 대기열로 돌려 다음 tick이 이어서 보낸다.
			// RescheduleDeliveries 대신 DeferDeliveries를 쓴다. 이것은 실패가 아니므로
			// 재시도 한도를 쓰면 안 된다(할당받을 때 미리 +1 했으므로 거기서 되돌린다).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("이 메시지가 알림 채널 길이 상한에 닿아 앞 %d건만 전달했습니다. 나머지는 다음 배치로 보냅니다", delivered)); err != nil {
				log.Printf("[notify] 나눠 보낼 나머지 대기열 등록 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 알림 채널이 오류도 내지 않고 몇 건 전달했는지도 말하지 않았다. 실패로 처리해(백오프를 거쳐)
		// 이 전달이 거듭 다시 할당되면서 끝내 표시되지 않는 일을 막는다.
		err = fmt.Errorf("알림 채널이 전달 건수를 알려 주지 않음(delivered=%d)", delivered)
	}

	// 실패 처리는 배치 전체의 최대 시도 횟수가 아니라 **한 건씩** 정한다.
	//
	// 예전에는 `if maxAttempts(deliveries) >= MaxNotifyAttempts`로 배치 전체를 실패시켰지만, 배치 안의
	// 건마다 시도 횟수가 다르다. 이미 두 번 재시도한 오래된 전달(attempts=2)이 같은 배치의
	// 새 전달(attempts=1)까지 failed로 끌고 간다. 새 취약점이 재시도 한 번 못 해 보고 영구히 사라져,
	// '오래된 행이 새 행을 끌어내리지 않게 한다'는 처음 의도와 정반대가 된다.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("%d번 재시도했지만 실패했습니다: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 지연 시간별로 묶어 다시 예약한다. 백오프가 3단계뿐이라 묶음 수가 자연히 적으므로, 건마다
	// UPDATE를 따로 보낼 필요가 없다(그러면 500건짜리 배치가 왕복 500번을 만든다).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 전달 재예약 실패 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 전달 실패 channel=%d kind=%s 영구실패=%d 재시도소진=%d 재시도대기=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries 는 all 중 keep에 없는 것을 돌려준다(포인터가 같은지로 비교한다).
// '메시지에 들어가지 못한' 전달을 찾는 데 쓴다. 이들은 명시적으로 처리해야 하며 애매한 상태로 남기면 안 된다.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt 는 알림 채널 구현을 찾고 그 설정을 파싱한다.
// ok=false면 유형이 등록되지 않은 것이므로, 전달을 끝없이 재시도하지 말고 바로 실패 처리해야 한다.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 설정 파싱에 실패하면 빈 map을 준다. 알림 채널의 Validate가 '어느 필드가 빠졌는지'를 알려 주며,
		// 그 오류가 JSON 파싱 오류보다 사용자가 고치는 데 더 도움이 된다.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle 은 취약점 하나의 메시지를 렌더링한다.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch 는 다이제스트 메시지를 렌더링한다. 스냅숏을 한 건씩 파싱해, 하나가 깨지면 그 건만 건너뛰고
// 배치 전체 다이제스트를 망치지 않는다.
//
// 반환값 included와 msg.Items는 **정확히 일대일로 대응**한다(i번째 전달 ↔ i번째 항목).
// 이 대응은 반드시 지켜야 한다. 호출자는 '알림 채널이 앞 K건까지 담았다'는 회신으로 앞 K개 전달을
// 전달됨으로 표시한다. 여기서 깨진 스냅숏을 건너뛰고도 그 전달을 included에서 빼지 않으면
// 인덱스가 어긋난다. 실패해야 할 깨진 항목이 전달됨으로 표시되고, 정상 항목은 전달 안 됨으로 잘못 판정된다.
// 깨진 항목은 호출자가 명시적으로 실패 처리한다. stepDigest 참고.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 깨진 스냅숏은 메시지에도 included에도 넣지 않는다. 처리는 호출자가 맡는다
			// ('전달됨'에 섞여 슬쩍 넘어가지 않고 명시적으로 실패 처리한다).
			log.Printf("[notify] 다이제스트 배치에서 파싱하지 못한 스냅숏 건너뜀 delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("다이제스트 배치의 전달 %d건 모두 파싱할 수 없음", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor 는 이벤트 스냅숏을 보낼 항목으로 렌더링하면서 자산 이름과 상세 링크도 구한다.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 자산 이름을 구하지 못해도 알림 발송을 막으면 안 된다. 이름을 못 읽는 것이 알림을 못 받는 것보다 훨씬 가볍다.
		// 메시지에서 자산 한 줄이 빠질 뿐이다.
		log.Printf("[notify] 자산 이름 조회 실패 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 상세 페이지 경로는 web/src/app/(main)/function/findings/detail/page.tsx 참고.
		// 그 페이지는 query 파라미터 id에서 취약점 id를 읽는다.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens 는 알림 채널의 토큰 버킷에서 토큰을 **최대 want개** 꺼내고 실제로 꺼낸 개수를 돌려준다.
//
// 토큰 하나 = 메시지 하나(HTTP 요청 한 번). 실시간 모드에서는 호출자가 필요한 건수만큼 넘기고,
// 다이제스트 모드에서는 취약점 한 배치를 메시지 하나로만 보내므로 1을 넘긴다.
//
// 버킷 용량은 이 알림 채널의 분당 상한이고 일정한 속도로 채워진다. ratePerMin<=0이면 발송 속도 제한이 없다는 뜻이며,
// 유한하지만 충분히 큰 값을 돌려줘 끝없는 적체가 한 회차의 루프를 붙잡지 않게 한다.
//
// want 상한은 꼭 필요하다. 이것이 없으면 버킷을 통째로 비울 수밖에 없는데, 호출자에게는 따로 회차당 상한이 있어
// 더 꺼낸 토큰은 쓰이지도 못하고 다음에 채워지기 전에 그냥 사라진다. 그러면 모아 둔 몰림 용량에는 영영 닿지 못하고,
// '이번 회차에 보낼 전달이 하나도 없음'일 때도 똑같이 토큰이 깎인다.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 실제로 흐른 시간만큼 채운다. 속도는 초당 ratePerMin/60이다.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 아주 작은 epsilon을 더한 뒤 정수로 바꾼다. 토큰 수는 부동소수점으로 더해 가므로, 두 번에 나눠 가득 채우면
	// 0.5 + 0.5가 0.9999999999가 될 수 있고, 그대로 int()하면 0으로 잘린다.
	// 수학적으로는 가득 찬 버킷에서 토큰을 꺼내지 못하게 된다. 1e-9는 토큰 하나보다 훨씬 작아 실제로 모자란 양을 놓치지 않는다.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled 는 전체 스위치를 읽는다.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL 은 상세 링크에 쓸 외부 주소를 끝의 슬래시를 떼고 돌려준다.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval 은 다이제스트 주기를 돌려준다. 잘못됐거나 설정 안 됐으면 기본값을 쓴다.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot 은 전달에 해당하는 이벤트의 스냅숏을 파싱한다.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("전달 %d의 이벤트 스냅숏이 비어 있음", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("전달 %d의 이벤트 스냅숏 파싱 실패: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// 이벤트 유형은 이벤트 행을 기준으로 한다. 스냅숏 안의 값은 이전 버전이 썼을 수 있다.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
