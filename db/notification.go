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

// This file is the channel-config and event layer for IM notifications. Claiming
// delivery jobs and their state transitions live in db/notification_delivery.go.
//
// Two invariants to keep when changing this file:
//
//  1. The transaction that writes a finding (RecordFindingTx) only calls
//     InsertNotificationEventTx for one blind insert. It does not read any
//     notification table and does not run filter matching. Any read introduced
//     here could poison or even abort the finding write because of a filter the
//     user misconfigured.
//  2. Filter matching never returns an error: a malformed config is treated as a
//     hit (see notify.Match). Prefer an extra push over a missed one.

// ErrNotificationChannelNotFound means the channel does not exist.
var ErrNotificationChannelNotFound = errors.New("notification channel not found")

// Delivery states.
const (
	NotifyStatePending = "pending" // waiting to send
	NotifyStateSending = "sending" // claimed by a dispatcher; lease not expired
	NotifyStateSent    = "sent"    // delivered
	NotifyStateFailed  = "failed"  // retries exhausted or a permanent failure; can be resent manually
	NotifyStateSkipped = "skipped" // channel disabled; will not be sent
)

// Push modes.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode whitelist-checks the push mode (same idea as findings.status:
// no DB CHECK, so it can be extended later).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel is one channel instance. Config and Filter stay raw JSON;
// the notify package parses them — the db layer does not know what the fields mean.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled is a pointer so "field not sent" can be told apart from "explicitly false" —
	// the frontend toggle only submits fields that changed.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled reports whether the channel is enabled; a nil Enabled (not loaded) is treated as enabled.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is one event fact.
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

// ListNotificationChannels returns every channel instance, enabled ones first, then by id.
// The order is in SQL so the UI and the dispatcher see the same stable order.
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

// NotificationChannelByID loads one channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel creates or updates a channel.
//
// An update overwrites only fields the caller set explicitly (non-nil / non-empty),
// so the frontend can submit a partially edited drawer without sending back config
// fields it did not show — sending those back would let a masked value overwrite the real secret.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// 0 is deliberately left alone: 0 is a valid setting and means "no rate limit".
	//
	// This used to be `if c.RatePerMin <= 0 { c.RatePerMin = default }`, meant as
	// "fill a safe default when unset", but that also swallowed an explicit 0 —
	// the docs, the UI hint, and takeTokens all treat 0 as unlimited, and only this
	// layer quietly rewrote it to 20 (DingTalk/WeCom/Telegram) or 100 (Feishu). The
	// operator thought the limit was lifted and was actually stuck at 20/minute with no hint.
	//
	// Only the caller can tell "unset" from "explicit 0" (field omitted vs sent as 0),
	// so the server layer fills the default when the field is omitted. See notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("rate limit cannot be negative")
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

// SetNotificationChannelEnabled toggles a channel on or off.
//
// Disabling a channel also marks its unsent deliveries skipped: otherwise turning it
// back on would suddenly deliver a pile of old findings from while it was off, which
// are stale and easy to mistake for new ones.
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
				id, NotifyStateSkipped, "channel disabled", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel deletes a channel. Its delivery history cascades with the foreign key
// (once the channel config is gone, the history has nothing to explain).
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

// RecordNotificationEventTx does its best to write one notification event inside the caller's transaction.
//
// This is the only notification change on the finding-write path: one INSERT, no table reads, no knowledge of channels,
// no filtering. Commit makes "the finding is stored" and "a notification job exists" atomic.
// There is no window where the commit succeeds, nothing is queued, and the message is lost forever.
//
// Two design choices, neither accidental:
//
//  1. Why a SAVEPOINT: in PostgreSQL, any statement error inside a transaction puts the whole transaction into the
//     aborted state, and every later statement (including COMMIT) fails. So "ignore this INSERT's error and let the
//     caller commit" is impossible in PG unless a savepoint isolates the error on that one statement. Without a
//     savepoint, the only option left is rolling the whole transaction back.
//
//  2. Why rolling the whole transaction back is wrong: notifications are a convenience; the finding record is the product.
//     A problem in a notification table (an unmigrated old database, a brief disk fault) must not keep a high-severity
//     finding out of the database. So the error is isolated, logged, and reported as false, and the finding write still
//     commits — the cost is losing this one notification. Returning a bool rather than an error is deliberate: the caller
//     should not treat it as an error that decides whether the write succeeds.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] failed to serialize notification event finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] failed to create savepoint finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] failed to write notification event finding=%d (the finding record is unaffected): %v", findingID, err)
		// Roll back to the savepoint so the transaction leaves the aborted state.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] failed to roll back to savepoint finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release the savepoint so a long transaction does not pile up unused ones.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is the standalone-transaction form of InsertNotificationEventTx, for call sites
// that are not already in a transaction (such as a channel's "send test message", which has no real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("failed to serialize notification event snapshot: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands findings events that have not been dispatched yet into delivery jobs
// for the currently enabled channels, and returns how many events this round handled and how many deliveries it created.
//
// The whole round is one transaction: events are claimed with FOR UPDATE SKIP LOCKED, so several processes
// running at once each get different rows (the archive queue claims work the same way; see
// completeNextArchiveJob in db/task_archives.go).
//
// Filter matching is deliberately in Go rather than SQL: a channel filter is JSONB of optional fields,
// and expressing the combinations in SQL would be hard to maintain. There are only a handful of channels
// a person configures, so loading them all and comparing in memory is faster and easier to test.
//
// Events that hit no channel are still marked fanned_out — otherwise they would stay in the pending set
// and be rescanned on every tick.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

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
		// We wrote the snapshot ourselves, so it should always parse; a parse failure does not stop delivery,
		// but this event will be skipped by every channel that has a filter because its fields are empty — better to drop one push
		// than let one bad row stall the whole queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind comes from the row: the copy in the snapshot is for rendering and may have been written by an older version.
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

	// Mark this round's events as dispatched. Events that hit no channel are marked too (see the function comment).
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

// listEnabledNotificationChannelsTx loads enabled channels inside the transaction. There are few of them,
// so there is no paging and no cache — a cache would add the extra question of when a config change takes effect.
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

// NotificationAssetNames resolves asset ids to short display names for notification messages.
//
// The result keeps the input order and may be shorter (missing ids are skipped). Stable order
// keeps the assets in one finding's message in the same sequence across deliveries — otherwise a retry
// would reorder them and look like the assets changed.
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

// assetDisplayName picks the most recognizable identifier for the asset type.
// The fallback is an empty string, and the caller decides how to present an asset with no name — this function
// does not invent a placeholder, or noise like "asset #42" would land in the message and be read as a real domain.
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

// SetFindingStatusWithNotify updates a finding's disposition and records a status-change
// notification event in the same transaction.
//
// from is the status before the change; found is whether the finding exists; notified is whether the event was recorded.
//
// Three deliberate behaviors:
//   - No event is recorded when the status did not actually change. A drawer resubmitting the same value, or an
//     automation script replaying idempotently, should not create notification noise.
//   - A missing finding returns found=false and writes nothing; the caller turns that into a 404.
//   - A failed event record does not undo the status update (see the savepoint note on RecordNotificationEventTx),
//     so when notified=false the status has already changed and the caller should not report an error for that.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx updates a finding's status and records a status-change notification inside the caller's transaction.
//
// It is a transaction-level function so every status-change path shares the same semantics — previously only
// patchFinding used the notifying version, and a retest verdict of "fixed" (the `UPDATE findings SET status=...`
// in finding_retests) wrote the database directly, so channels with on_status_change never heard about that
// transition: the status changed quietly in the UI and operators only noticed by opening the platform.
//
// from is the previous status, found is whether the finding exists, changed is whether the status really changed,
// and notified is whether the event was recorded (a failed record does not undo the status update; see RecordNotificationEventTx).
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
		// Do not record an event when the status did not really change: resubmitting the same value or an idempotent replay should not create notification noise.
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

// NotificationStats is the overview counts at the top of the notifications page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // age in milliseconds of the oldest delivery still waiting to send
}

// NotificationStatsSnapshot summarizes how healthy the notification system is.
// BacklogAgeMS is the most direct signal that "pushes are stuck" — much more useful than a pending count,
// because a backlog of 3 can be 3 seconds or 3 hours.
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
