package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 이 파일은 전송 작업의 수령과 상태 전이이다.
//
// 수령에는 긴 트랜잭션 대신「리스」를 쓴다:행을 sending으로 두고 next_attempt_at을 미래로 밀어
// 리스 만료 시각으로 삼고, 트랜잭션 커밋 후에 네트워크 전송을 수행한다. 이렇게 하면 전송 중에는 DB 락을 쥐지 않는다————
// 네트워크 요청은 수 초 걸릴 수 있어（클라이언트 타임아웃 15초）, 행 락을 계속 쥐고 있으면 같은 DB의 다른 쓰기 작업을 무너뜨린다.
//
// 대가는 프로세스가 전송 도중 크래시하면 행이 sending에 멈춘다는 것이다. 이것은 **자가 치유 가능**하다:리스 만료 후
// next_attempt_at 이 과거로 떨어지면, 다음 라운드 수령이 같은 행을 다시 건져 올린다（수령 조건의
// state IN ('pending','sending')）. 재시도 카운트는 수령 시점에 이미 +1 되므로, 크래시가 부르지 않는 것:
// 무한 재시도——MaxNotifyAttempts번 기회를 다 쓰면 failed로 떨어져 수동 처리를 기다린다.

// MaxNotifyAttempts 한 건의 전송에 대한 최대 시도 횟수다（첫 시도 포함）.
// 여기에 정의하고 전송 엔진에 두지 않는다:이것은 상태 기계 자체의 정책이고, 엔진은 실행자일 뿐이다.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize 단일 요약 배치가 한 번에 최대 몇 건의 전송을 합칠지다.
//
// 존재 이유는 리소스다:한 요약 주기 안에 수만 개의 취약점을 스캔해 내면（충분히 가능하다——한 번의 전량 스캔
// 으로도 해낼 수 있다）, 상한을 두지 않으면 수령이 모든 행을 메모리로 읽어 들여 하나의 초장문 메시지로 렌더링하고,
// 그다음 채널의 길이 상한에 절반 이상이 잘려 나간다——메모리도 낭비하고, 잘려 나간 그 취약점들을 **조용히 잃는다**.
// 상한을 둔 뒤에는, 초과분이 DB에 남아 다음 배치가 되고, 다음 주기에 자연스럽게 발송돼, 잃지 않는다.
//
// 의 근거:메시지로 렌더링한 뒤 위챗워크 4096바이트 상한 안에서도 "읽을 내용이 있는" 수준이다;
// 더 커도 잘림이 더 뒤쪽 위치에서 발생하게 할 뿐이다.
const MaxDigestBatchSize = 500

// NotificationDelivery 한 건의 전송 작업으로, 렌더링에 필요한 채널 설정과 이벤트 스냅샷을 담는다.
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
	// 조인 로드한 렌더링 컨텍스트로, JSON에는 들어가지 않는다（server 계층이 DTO를 조립）.
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind 이벤트에서 끌어내, 히스토리 목록이 취약점 상세로 바로 점프하도록 제공한다.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind 목록 표시용 중복 필드로, 프런트엔드의 2차 조회를 없앤다.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery 전송 행의 통일된 읽기 형태다:전송 + 이벤트 스냅샷 + 채널 설정.
// 메시지 하나를 렌더링하려면 셋 중 하나도 빠지면 안 되고, 나눠 조회하면 세 번 왕복하게 된다.
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

// claimQuery 한 번의 수령을 설명한다:먼저 sel로 후보를 골라 락을 걸고, 다시 그것들을 sending으로 두고
// 리스를 연장한다. sel 안의 lease 위치는 호출 측이 $n으로 플레이스홀드하고 직접 인자를 전달한다.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries 어떤 채널의 만기된 실시간 전송 한 묶음을 수령한다, 최대 limit건.
//
// 의도적으로 **단일 채널** 단위로 수령하고「전역으로 한 묶음 받아 골라 발송」하지 않는다:레이트 리밋 게이트가 전송 엔진에서 채널별로
// 유지되므로, 이 채널이 이번 라운드에 몇 건 더 보낼 수 있는지 먼저 알고, 그만큼의 행을 수령해야, 레이트 리밋이 비로소 소모하지 않는
// 재시도 횟수. 반대로 먼저 받고 버리면, 레이트 리밋에 막힌 행은 이미 attempts가 한 번 계산됐고,
// 3 회 예산이 순수한 대기만으로 소진돼, 결국 failed로 떨어진다.
//
// 조건은「리스가 만료된 sending」을 포함한다——그것이 크래시 자가 치유의 착지점이다. lease는 1회
// 전송의 최악 소요 시간（채널 HTTP 클라이언트 타임아웃 15초）보다 뚜렷이 커야 하고, 아니면 같은 행이 두 dispatcher에 dispatcher
// 동시에 전송된다. 동시에 비활성화된 채널도 막는다:비활성화 작업이 이미 재고 전송을 skipped로 표시했지만,
// 여기서 한 번 더 막아, 비활성화와 수령이 동시에 일어날 때의 누락을 방지한다.
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

// DigestBatchDue 해당 채널이 만기 배치 하나를 충분히 모았는지 보고한다:대기 전송이 존재하고, 또한 **가장 오래된 그 건**이**
// 나이가 요약 주기에 도달했다.
//
// 판정 근거는 벽시계가 아니라 가장 오래된 전송의 나이다:이렇게 하면 방금 만든 채널이 정각에 맞춘다는 이유로
// 건 하나뿐인「요약」을 즉시 뱉지 않고, 오래 적체된 배치도 한 라운드를 더 헛되이 기다리지 않는다.
//
// ClaimDigestBatch와 분리한 것은 의미가 다르기 때문이다:이 함수는「보내야 할지 말지」에만 답하고,
// 수령은 해당 채널의 **전체** 대기 행을 가져가야 한다（아직 나이가 안 찬 것들 포함）——아니면 한 주기가
// 여러 메시지로 쪼개져, 요약이 의미를 잃는다.
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

// ClaimDigestBatch 해당 채널의 현재 만기된 대기 전송을 수령해, 하나의 요약 배치로 만든다,
// 배치당 최대 MaxDigestBatchSize건.
//
// 같은 배치의 모든 전송은 batch_id를 공유하고, 집합 내 최소 id를 배치 번호로 쓴다（안정적、읽기 쉬움、
// 별도 시퀀스 불필요）. 재시도 시 COALESCE로 원래 배치 번호를 보존해,「이 배치 N건은 함께 보낸 것」이
// 여러 번 재시도한 뒤에도 여전히 성립하게 한다.
//
// id 오름차순으로 앞 N건을 취하고 무작위로 취하지 않는다:가장 먼저 생긴 전송이 가장 먼저 발송되고, 적체 시 나타나지 않는 것은
// 「새 취약점 먼저 발송、오래된 취약점은 영원히 뒤에 밀림」이라는 기아 현상이다.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit **메모리 상한**이고, 호출 측이 MaxDigestBatchSize를 전달한다;여기서 한 번 더 조여,
	// 호출 측이 더 큰 값을 전달하는 것을 방지한다.
	//
	// 의도적으로「레이트 리밋 할당량」이 배치 크기를 대신하는 것을 받지 않는다:레이트 리밋의 단위는 메시지 건수다——한 배치는
	// 메시지 하나만 보내고、토큰 하나를 소모하며, server 계층의 takeTokens가 차감한다——「한 배치에 몇 건
	// 취약점을 담나」와는 서로 다른 두 차원이다. 예전에 rate_per_min을 digest에 적용되게 하려고 매 라운드
	// 요청 예산을 배치 크기로 넘겼는데, 그 결과 rate=20/min인 채널은 배치마다 취약점 1건만 담아,
	// digest digest가 요약 문구를 단 실시간 푸시로 퇴화했다. 레이트 리밋을 바꾸려면 takeTokens의 want를 바꾸고,
	// 여기는 건드리지 마라.
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

// claimDeliveries 「선택 + sending 설정 리스 연장 + 전체 행 읽기」를 수행하며, 전부 하나의 트랜잭션 안에서 한다.
// postClaim 선택적 추가 단계다（요약 배치가 이것으로 batch_id를 쓴다）.
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋 성공 후에는 no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending으로 두고 next_attempt_at을 미래로 민다:이 미래 시각이 곧 리스 만료 시각이고,
	// 「리스 미만료」와「재시도 시간 미도달」이 그래서 같은 조건식을 공유하고, 새 컬럼을 추가할 필요가 없다.
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

// MarkDeliveriesSent 한 묶음의 전송을 송달 완료로 표시한다.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries 한 묶음의 전송을 pending으로 되돌리고 재시도 시간을 뒤로 민다.
//
// pending으로 되돌리고 새로운 중간 상태를 도입하지 않는 것은,「몇 번의 기회가 남았나」를 오직 한 곳에서만
// 표현하게 하기 위함이다（MaxNotifyAttempts）, 상태 기계의 분기가 재시도 정책에 따라 부풀어 오르는 것을 방지한다.
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

// DeferDeliveries 한 묶음의 전송을 pending으로 되돌려、즉시 다시 수령 가능하게 하고, **수령 시 계산된 그 1회 시도를 철회한다**.
//
// 용도는 하나뿐이다:요약 메시지를 채널 길이 상한에 따라 분할 발송할 때, 이 건에 담기지 못한 항목은 다음 배치로 남겨야 한다.
// 그것은 실패가 아니므로 재시도 예산을 소모해선 안 된다——수령 시 attempts가 이미 낙관적으로 +1 됐으니,
// 여기서 반드시 다시 빼야 한다. 아니면 500건의 적체가 단락당 20건씩 25단락으로 쪼개져,
// 꼬리 쪽 항목이 3번째 단락에서 MaxNotifyAttempts에 의해 failed로 판정되는데, 그것들은 어떤 오류도 낸 적이 없다.
//
// GREATEST(...,0) GREATEST(...,0)이「누군가 수동 재발송으로 attempts를 0으로 만든 뒤 다시 여기에 도달한」상황을 받아내,
// 카운트가 음수가 되지 않게 한다.
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

// FailDeliveries 한 묶음의 전송을 최종 실패로 표시하고, 전송 히스토리에서 수동으로 재발송하기를 기다린다.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 플레이스홀더는 $3부터 시작한다:$1은 state、$2는 last_error다.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery 전송 한 건을 수동 재발송한다:pending으로 리셋、재시도 카운트 0으로 초기화、
// 즉시 만기시킨다. 카운트를 지우는 것은 의도적이다——수동으로「재발송」을 누른다는 것은 앞선 몇 번의 실패 원인이 이미 처리됐다는 뜻이라,
// 낡은 카운트로 그것을 제한하는 것은 말이 안 된다.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("전송 %d가 존재하지 않거나 현재 상태가 재발송을 허용하지 않음", id)
	}
	return nil
}

// NotificationDeliveryFilter 전송 히스토리의 조회 조건이다.
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

// ListNotificationDeliveries 전송 히스토리를 페이지네이션으로 반환하고, 새것이 앞에 온다.
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

// truncateNotifyError 에러 정보를 컬럼이 받아들일 수 있는 길이로 자른다. 채널이 반환하는 응답 본문은 매우 길 수 있어
// 범용 Webhook이 자체 구축 서비스를 칠 때 특히 심하다）, 자르지 않으면 히스토리 목록의 페이로드가 부풀어 오른다.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 문자 경계를 따라 되돌려, UTF-8 문자 절반이 남아 프런트엔드에 깨진 글자로 표시되는 것을 방지한다.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders start부터 시작하는 $n 플레이스홀더 문자열과 대응 인자를 생성해, IN (...)에 쓰도록 제공한다.
// 예를 들어 start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
