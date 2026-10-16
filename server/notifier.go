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

// 전역 설정 키(settings 키값 표에 있음, 표 생성 불필요).
const (
	// settingNotifyEnabled settingNotifyEnabled는 푸시 총 스위치. 기본 켜짐: 유지보수 시 원클릭 지혈용,
	// 기능 활성 조건이 아님 —— 진짜 활성 조건은 「채널 설정 여부」.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL settingNotifyPublicBaseURL은 취약점 상세 돌아가기 링크를 생성하는 외부 접근 주소
	// （예: https://artex.example.com）. 비우면 메시지에 돌아가기 버튼 없음.
	// 프로젝트에 재사용 가능한 외부 주소 설정이 없어 여기에 한 항목 추가.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes settingNotifyDigestMinutes는 요약 모드 주기(분).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick notifyTick은 전송 엔진의 폴링 간격. 3초는 이 엔진 실시간성의 상한,
	// 또한 「취약점 DB 저장」에서 「메시지 IM 도달」 사이 주요 지연원.
	notifyTick = 3 * time.Second
	// notifyLease notifyLease는 전송 수령 시 리스 시간. 단건 전송 최악 소요보다 충분히 커야 함
	// （notify 패키지 HTTP 클라이언트 타임아웃 15초), 아니면 같은 행이 두
	// dispatcher dispatcher에 동시 전송됨.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick notifyFanOutPerTick은 라운드마다 분배하는 이벤트 수 제한, 채널 첫 활성 시
	// 역사적 백로그를 한 번에 전송 작업으로 펼치는 것 방지.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes notifyDefaultDigestMinutes는 요약 주기 기본값.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick notifyUnlimitedBurstPerTick은 채널이 속도 제한 미설정일 때 라운드당 전송 상한.
	// 존재 의의는 「채널을 무제한으로 설정 + 한 번에 수천 취약점 스캔」이
	// 단일 라운드 루프를 장시간 블로킹하는 것 방지.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick notifyMaxSendsPerChannelPerTick은 단일 채널이 라운드당 최대 몇 건 전송.
	//
	// 이 상한은 **리스 시간**에서 역산: 수령 시 행에 리스를 검(notifyLease = 3분),
	// 한 라운드 직렬 전송 건수가 많아 최악 소요가 리스를 초과하면 뒤 몇 건은 다 못 보내고 리스 만료.
	// 단일 프로세스 내에서는 무관(Run은 단일 goroutine 직렬, tick 재진입 없음), 단 **두
	// 프로세스가 같은 DB**에 붙으면, 상대가 리스 만료 행을 다시 수령해 중복 전송, 또한
	// attempts attempts를 두 배로 증가, 원 프로세스가 아직 전송 중일 때 실패로 판정.
	//
	// 값: 3분 리스 / 30초 단건 타임아웃 = 6은 **리스를 꽉 채움**, 여유 0,
	// 쓰면 안 됨; 5로 하면 최악 소요 150초로 30초 여유. 이 관계는
	// TestNotifyTickBudgetFitsWithinLease TestNotifyTickBudgetFitsWithinLease가 고정 —— notifyLease,
	// notifySendTimeout notifySendTimeout 또는 이 값 중 하나만 바꿔도 그 단언이 실패함.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout notifySendTimeout은 단건 전송 타임아웃. 위 상수의 값도 결정,
	// 둘의 곱이 notifyLease를 넘으면 안 됨, TestNotifyTickBudgetFitsWithinLease 참조.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff notifyBackoff는 실패 재시도의 백오프 시퀀스, 인덱스는 시도 횟수.
// 3 3회 기회(첫 회 포함)는 db.MaxNotifyAttempts에 대응, 둘은 함께 바꿔야 함.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier Notifier는 취약점 푸시 전송 엔진.
//
// Scheduler와 병렬로 독립 goroutine 실행(server.New 참조). 의도적으로
// Scheduler Scheduler의 tick 재사용 안 함: 푸시 실시간성 요구(3초)는 트리거 업무 리듬과 다름,
// 둘의 실패는 서로 영향 없음 —— 푸시가 막혀도 agent 트리거에 영향 주면 안 됨.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu mu는 buckets 보호. 채널 수 적고 경쟁 낮아 뮤텍스 하나로 충분,
	// 더 세밀한 구조를 도입할 가치 없음.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket notifyBucket은 단일 채널의 토큰 버킷.
//
// 「분당 카운트 후 0 리셋」 슬라이딩 윈도우 대신 토큰 버킷을 쓰는 이유는 후자의 경계 효과가 나쁨:
// 윈도우 끝에 20건 보내고 다음 순간 또 20건이면 플랫폼엔 1초에 40건,
// 속도 제한됨; 토큰 버킷은 일정 속도로 보충해 이런 버스트를 자연히 방지.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run Run은 ctx가 끝날 때까지 루프. server.New가 한 번 시작.
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

// step step은 한 라운드 실행: 먼저 새 이벤트 분배, 그다음 만기 작업 전송.
//
// 어느 단계든 실패해도 로그만 남기고 루프 중단 안 함 —— 알림 시스템 장애가 프로세스 레벨 문제로 확대되면 절대 안 됨.
// 각 tick은 독립, 다음 라운드가 자연히 재시도.
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
		log.Printf("[notify] 채널 읽기 실패: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// 토큰 버킷의 계량 단위는 **메시지 건수**(HTTP 요청 수와 동등), 취약점 건수 아님.
		// 실시간 모드에선 둘이 같음(취약점 하나당 메시지 하나); 요약 모드에선 한 배치 취약점이 합쳐져
		// 메시지 하나라 토큰 하나만 소비.
		//
		// 두 모드 모두 토큰 버킷을 먼저 묻고 그다음 할당량만큼 수령 —— 순서 못 바꿈, 아니면 속도 제한에 막힌
		// 전송이 이미 재시도 횟수를 소비.
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

// digestTickPlan digestTickPlan은 요약 채널의 이번 라운드 토큰 소비와 배치 크기 상한 반환.
//
// 두 반환값은 **서로 다른 차원**, 이것이 독립 함수로 둔 이유:
//
//   - tokens   - tokens는 메시지 건수. 한 배치 취약점이 메시지 하나로 합쳐져 HTTP 요청 한 번이라 항상 1.
//     rate_per_min     rate_per_min이 따라서 digest에도 유효(분당 최대 이만큼의 요약 메시지).
//   - claimLimit   - claimLimit은 이 배치에 최대 몇 건 담을지. 메모리 상한에만 제약, 요청 예산과 무관.
//
// 예전엔 rate_per_min을 digest에 유효하게 하려고 라운드당 요청 예산
// (notifyMaxSendsPerChannelPerTick, 리스에서 역산)을 배치 크기로 그대로 전달.
// 결과는 rate_per_min=20 채널이 3초 tick에 토큰 1개만 보충돼 요약 메시지마다
// 취약점 1개만 담음 —— digest가 「요약 문구 붙은 실시간 푸시」로 퇴화, 독자는
// 「최근 30분 취약점 1건 추가」 연속을 받고, db.MaxDigestBatchSize는 영원히 도달 불가.
//
// 이 증상은 E2E 테스트에서 발견 어려움(기존 케이스는 충분히 큰 limit을
// stepDigeststepDigest에 수동 전달해 step의 할당량 계산을 우회), 그래서 결정을 여기에 모아
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget TestDigestTickPlanDecouplesBatchSizeFromSendBudget로 직접 고정.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime stepRealtime은 채널의 실시간 작업을 수령·전송, 취약점 하나당 메시지 하나.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 실시간 전송 수령 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q 미등록", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 렌더링 실패는 로컬 데이터 문제라 재시도해도 나아지지 않음.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest stepDigest는 배치 만기 시 채널의 대기 전송을 메시지 하나로 집계해 보냄.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 요약 배치 판단 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 요약 배치 수령 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("채널 유형 %q 미등록", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// 스냅샷이 깨져 메시지에 못 들어간 전송은 명시적으로 실패 판정해야 함. 안 그러면
	// included included 밖에 남아 메시지에도 실패 목록에도 안 들어가 —— 전송 성공 시 상태가
	// 이후 일괄 표시에서 누락, 리스 만료까지 sending에 영원히 머물며 반복 수령됨.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "이벤트 스냅샷 파싱 불가, 이 취약점을 메시지로 렌더링할 수 없음"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 깨진 스냅샷 전송 실패 표시 오류 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 스냅샷 파싱 불가 전송 %d건 건너뜀 channel=%d", len(skipped), ch.ID)
	}
	// 메시지에 들어간 것만 send에 넘김: included[i]와 msg.Items[i]가 엄격히 대응,
	// send send는 이 대응으로 「채널이 앞 K건 담았음」을 올바른 전송 행에 반영.
	n.send(ctx, channel, cfg, msg, included)
}

// send send는 전송하고 결과에 따라 상태 전이.
//
// 같은 배치 전송(요약 모드에선 수십 건)은 하나의 전송 결과 공유: 송달되거나 배치 전체 재시도.
// 건별 재시도 안 함 —— 요약 메시지는 하나라 일부만 재전송하면 배치 의미가 어긋남.
//
// 유일한 예외는 **채널 길이 상한에 의한 분할**: 채널이 실제 앞 K건만 담았다고 회보,
// 그러면 K+1건째부터 다음 배치로 남겨야 함, 함께 성공 표시하면 안 됨. 아니면 잘린
// 그 취약점들이 메시지에도 실패 목록에도 없이 완전히 사라짐.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 단건 전송에 상한을 둬 어떤 채널이 막혀 이번 라운드 나머지 채널 전부 막는 것 방지.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 채널 회보 건수가 전송 수를 초과할 수 없음; 실제 그러면 렌더링 계층 계산 오류,
			// 전부 송달 처리하고 문제를 기록, 기록을 뒤죽박죽 쓰는 것보다 나음.
			log.Printf("[notify] 채널 회보 송달 건수 %d가 전송 수 %d 초과 channel=%s, 전부 송달 처리",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 송달 표시 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 이 메시지가 채널 길이 상한 도달: 나머지는 즉시 큐에 복귀, 다음 tick이 이어서 전송.
			// RescheduleDeliveries 대신 DeferDeliveries 사용 —— 이건 실패 아님,
			// 재시도 예산 소비하면 안 됨(수령 시 이미 낙관적 +1, 거기서 되돌림).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("이 메시지가 채널 길이 상한 도달, 앞 %d건만 송달, 나머지는 다음 배치로", delivered)); err != nil {
				log.Printf("[notify] 분할 이어서 전송 큐 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 채널이 오류도 송달 건수도 보고 안 함. 실패 처리(백오프 경유),
		// 이 전송이 반복 수령되는데 영원히 표시 안 되는 것 방지.
		err = fmt.Errorf("채널이 송달 건수 미보고（delivered=%d）", delivered)
	}

	// 실패 처치는 **건별** 결정, 배치 전체의 최대 시도 횟수로 판단 안 함.
	//
	// 예전엔 `if maxAttempts(deliveries) >= MaxNotifyAttempts` 배치 전체 판정이었으나, 배치 내
	// 각 건의 시도 횟수가 다름: 이미 두 번 재시도한 오래된 전송(attempts=2)이 같은 배치의
	// 새 전송(attempts=1)을 함께 failed로 끌어들임 —— 새 취약점이 재시도 한 번 못 쓰고 영구 유실,
	// 「오래된 행이 새 행을 물귀신처럼 끌어내리지 않게」라는 취지와 정반대.
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
		reason := fmt.Sprintf("%d회 재시도 후에도 실패: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 지연별로 그룹 재배치: 백오프가 3단계뿐이라 그룹 수가 자연히 작음, 건별로 UPDATE 한 번씩
	// UPDATE할 필요 없음(500건 배치가 500번 왕복하게 됨).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 전송 재배치 실패 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 전송 실패 channel=%d kind=%s 영구 실패=%d 재시도 소진=%d 재시도 대기=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries excludeDeliveries는 all 중 keep에 없는 것 반환(포인터 신원으로 비교).
// 「메시지에 못 들어간」 전송을 찾는 데 사용 —— 명시적으로 처치해야 하고 회색 지대에 두면 안 됨.
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

// adapt adapt는 채널 구현을 가져와 설정을 파싱.
// ok=false는 유형 미등록 의미, 전송을 무한 재시도 말고 바로 실패 판정해야 함.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 설정 파싱 실패 시 빈 map 제공: 채널 자체 Validate가 「어느 필드 누락」을 보고,
		// 그 오류가 JSON 파싱 오류보다 수정에 더 도움.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renderSingle은 단건 취약점 메시지 렌더링.
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

// renderBatch renderBatch는 요약 메시지 렌더링. 건별로 스냅샷 파싱 —— 하나 깨져도 그것만 건너뜀,
// 배치 전체 요약을 날리지 않음.
//
// 반환값 included와 msg.Items는 **엄격히 일대일 대응**(i번째 전송 ↔ i번째 항목).
// 이 대응은 하드 요구: 호출자가 「채널이 앞 K건 담았음」으로 앞 K개 전송을
// 송달 표시 결정. 여기서 깨진 스냅샷을 건너뛰면서 건너뛴 전송을 included에서 제외 안 하면,
// 인덱스가 어긋남 —— 실패해야 할 나쁜 항목이 송달로 표시되고, 좋은 항목이 미송달로 오판됨.
// 깨진 것들은 호출자가 명시적으로 실패 표시, stepDigest 참조.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 깨진 스냅샷은 메시지에도 included에도 안 들어감 —— 처치는 호출자 책임
			// (명시적 실패 표시, 「송달」에 섞어 얼버무리지 않음).
			log.Printf("[notify] 요약 배치에서 파싱 불가 스냅샷 건너뜀 delivery=%d: %v", dl.ID, err)
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
		return notify.Message{}, nil, fmt.Errorf("요약 배치 %d건 전송 전부 파싱 불가", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor itemFor는 이벤트 스냅샷을 푸시 항목으로 렌더링, 더불어 자산명과 상세 돌아가기 링크 파싱.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 자산명 파싱 실패가 푸시를 막으면 안 됨: 이름을 못 읽는 게 알림을 못 받는 것보다 훨씬 가벼움,
		// 메시지에 자산 한 줄 빠질 뿐.
		log.Printf("[notify] 자산명 파싱 실패 finding=%d: %v", snap.FindingID, err)
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
		// 상세 페이지 라우트는 web/src/app/(main)/function/findings/detail/page.tsx 참조,
		// query 파라미터 id에서 취약점 id를 읽음.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takeTokens는 채널 토큰 버킷에서 **최대 want개** 토큰을 가져감, 실제 가져간 수 반환.
//
// 토큰 하나 = 메시지 하나(HTTP 요청 한 번). 실시간 모드에선 호출자가 필요한 만큼 전달;
// 요약 모드에선 한 배치 취약점이 메시지 하나라 1 전달.
//
// 버킷 용량은 그 채널 분당 상한, 일정 속도로 보충. ratePerMin<=0은 무제한,
// 유한하지만 충분히 큰 값 반환, 단일 라운드 루프가 무한 백로그에 막히는 것 방지.
//
// want want 상한은 필수: 없으면 버킷을 통째로 비울 수밖에 없는데 호출자도 라운드당 상한이 있어,
// 더 가져온 토큰은 쓰지도 못하고 다음 보충 전에 사라짐 —— 쌓인 버스트 용량은 영원히 도달 불가,
// 「이번 라운드 대기 전송 없음」조차 한 몫 까임.
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
	// 경과한 실제 시간에 따라 보충, 속도는 ratePerMin/60 매초.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 극소 epsilon 더한 뒤 정수화: 토큰 수는 부동소수 누적이라 두 번에 나눠 채우면
	// 0.5 + 0.5 0.5 + 0.5가 0.9999999999가 될 수 있고, 바로 int()면 0으로 잘림 —— 0——
	// 수학적으로 꽉 찬 버킷인데 토큰을 못 꺼냄. 1e-9는 토큰 하나보다 훨씬 작아 진짜 부족분을 놓치지 않음.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled enabled는 총 스위치 읽음.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL publicBaseURL은 돌아가기 링크용 외부 주소 반환, 끝 슬래시 제거.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval digestInterval은 요약 주기 반환, 무효거나 미설정 시 기본값으로 폴백.
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

// parseSnapshot parseSnapshot은 전송 대응 이벤트의 스냅샷 파싱.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("전송 %d의 이벤트 스냅샷이 비어 있음", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("전송 %d의 이벤트 스냅샷 파싱 실패: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// 이벤트 유형은 이벤트 행 기준, 스냅샷의 것은 구버전이 썼을 수 있음.
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
