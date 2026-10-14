package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file claims delivery jobs and moves them through their states.
//
// Claiming uses a lease rather than a long transaction: the row is set to sending and next_attempt_at
// is pushed into the future as the lease expiry, the transaction commits, and only then does the network send.
// That way the send does not hold a database lock — a network request can take seconds (the client timeout is 15s),
// and holding a row lock that long would stall other writes on the same database.
//
// The cost is that a crash mid-send leaves the row in sending. That heals itself: once the lease expires,
// next_attempt_at is in the past and the next claim picks the same row up again (see state IN ('pending','sending')
// in the claim condition). The attempt count is incremented at claim time, so a crash cannot retry forever —
// after MaxNotifyAttempts the row falls into failed and waits for a person.

// MaxNotifyAttempts is the maximum attempts for one delivery (including the first).
// It lives here rather than in the delivery engine: it is the state machine's own policy, and the engine only executes it.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize is how many deliveries one digest batch merges at most.
//
// The reason is resources: one digest period can scan tens of thousands of findings (a full scan can do that).
// With no cap, the claim would read every row into memory, render one enormous message, and then have the channel's
// length limit cut most of it off — wasting memory and silently dropping the findings that were cut.
// With a cap, the overflow stays in the database as the next batch and goes out on the next period, so nothing is lost.
//
// 500 is the size that, once rendered, still has readable content inside WeCom's 4096-byte limit;
// anything larger only moves the truncation further down.
const MaxDigestBatchSize = 500

// NotificationDelivery is one delivery job, including the channel config and event snapshot needed to render it.
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
	// Render context loaded with the row; not part of the JSON (the server layer builds the DTO).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind come from the event so the history list can jump straight to the finding.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind are redundant fields for the list, so the frontend does not query again.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery is the one read shape for a delivery row: delivery + event snapshot + channel config.
// Rendering a message needs all three; separate queries would be three round trips.
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

// claimQuery describes one claim: select candidates by sel and lock them, then set them to sending and
// extend the lease. The caller places the lease argument with a $n placeholder and passes it.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries claims up to limit due realtime deliveries for one channel.
//
// It claims per channel on purpose, rather than claiming a global batch and then picking what to send: the rate-limit
// gate in the delivery engine is per channel. Only if you know how many this channel can still send this round, and
// then claim that many rows, does rate limiting avoid burning retry attempts. Claiming first and dropping the rest
// would already have counted an attempt on rows the limiter held back, and the budget of 3 would be spent on waiting
// until they land in failed.
//
// The condition includes sending rows whose lease has expired — that is how a crash heals. The lease must be clearly
// longer than the worst single delivery (the channel HTTP client times out at 15 seconds), or two dispatchers would
// send the same row. Disabled channels are excluded too: disable already marks existing deliveries skipped, and this
// is a second check so a disable racing a claim does not slip through.
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

// DigestBatchDue reports whether this channel has a due batch: there are deliveries waiting, and the oldest
// one is at least as old as the digest period.
//
// The decision uses the oldest delivery's age rather than the wall clock, so a channel that was just created does not
// immediately emit a one-item "digest" because the clock hit an hour boundary, and a long-backlogged batch does not wait an extra round for nothing.
//
// It is separate from ClaimDigestBatch because the meaning is different: this function only answers "should we send",
// while the claim takes every waiting row for the channel (including ones that are not old enough yet) — otherwise one period
// would be split into several messages and the digest would stop meaning a digest.
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

// ClaimDigestBatch claims the due waiting deliveries for one channel as one digest batch,
// at most MaxDigestBatchSize rows.
//
// Every delivery in the batch shares batch_id, using the smallest id in the set as the batch number (stable, readable,
// no extra sequence). Retries keep the original batch id with COALESCE, so "these N rows were sent together"
// still holds after several retries.
//
// The first N rows are taken in id order rather than at random: the earliest delivery goes out first, so a backlog
// does not starve old findings behind new ones.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit is a memory cap. The caller passes MaxDigestBatchSize; this clamps it again
	// so a larger value cannot get through.
	//
	// The rate-limit budget is deliberately not used as the batch size: the limiter counts messages — one batch sends
	// one message and spends one token, deducted by takeTokens in the server — which is a different unit from
	// "how many findings fit in a batch". Passing each round's request budget in as the batch size, so rate_per_min
	// would apply to digest, made a 20/min channel put 1 finding in each batch, and digest collapsed into realtime
	// with digest wording. To change the limit, change takeTokens' want, not this.
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

// claimDeliveries runs "select + set sending and extend the lease + read the full rows" in one transaction.
// postClaim is an optional extra step (digest batches use it to write batch_id).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Set sending and push next_attempt_at into the future: that future time is the lease expiry,
	// so "lease not expired" and "retry not due yet" share one condition and no new column is needed.
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

// MarkDeliveriesSent marks a batch of deliveries as delivered.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries returns a batch of deliveries to pending and pushes the retry time later.
//
// Returning to pending instead of adding a new intermediate state keeps "how many attempts are left" in one place
// (MaxNotifyAttempts), so the state machine does not grow a branch for every retry policy.
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

// DeferDeliveries returns a batch of deliveries to pending, immediately claimable, and undoes the attempt counted at claim time.
//
// It has one use: when a digest is split to fit a channel's length limit, items that did not fit this message wait for the next batch.
// That is not a failure, so it must not spend retry budget — claim already incremented attempts optimistically,
// and this has to decrement them. Otherwise a backlog of 500 cut into segments of 20 becomes 25 segments,
// and the tail is marked failed by MaxNotifyAttempts at segment 3 even though those rows never errored.
//
// GREATEST(...,0) covers "someone manually resent and zeroed attempts, then we got here",
// so the count cannot go negative.
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

// FailDeliveries marks a batch of deliveries as finally failed, waiting for a person to resend them from the delivery history.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// Placeholders start at $3: $1 is state, $2 is last_error.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery manually resends one delivery: reset to pending, clear the attempt count,
// and make it due immediately. Clearing the count is deliberate — a person clicking "resend" means the earlier failures have been handled,
// and limiting the new attempt with the old count would not make sense.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery %d does not exist or its current state cannot be resent", id)
	}
	return nil
}

// NotificationDeliveryFilter is the query for delivery history.
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

// ListNotificationDeliveries returns delivery history a page at a time, newest first.
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

// truncateNotifyError cuts an error down to a length the column can hold. A channel response body can be long
// (especially a generic webhook hitting a service you run yourself), and leaving it uncut would bloat the history list.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// Step back to a character boundary so a split UTF-8 character is not left for the frontend to show as garbage.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders builds a $n placeholder list starting at start, plus the matching args, for IN (...).
// For example start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
