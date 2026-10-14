package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// The tests in this file really connect to PostgreSQL (they skip when there is no database). The SQL uses
// FOR UPDATE SKIP LOCKED, make_interval, JSONB, and multi-row IN (...) placeholder splicing,
// all of which can compile and still fail at runtime, so they only count as verified when they actually run.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel creates a channel and deletes it when the test ends.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "test-channel-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("failed to create channel: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent writes an event directly (not through a finding) so fan-out and delivery can be tested.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("failed to write event: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// Each of the three asset kinds has its own display form: domain, IP, URL.
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

	// The input order is deliberately scrambled, and includes an id that does not exist.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("failed to resolve asset names: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("asset name count mismatch, want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/value mismatch, want %v got %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure is the core case for the savepoint: inside a transaction it first
// makes the notification_events write fail for sure (a temporary constraint that is always false), then asserts
// (1) the function reports false and (2) the transaction is not aborted, so later statements still run.
//
// Without the savepoint, PostgreSQL aborts the whole transaction and every later statement fails with
// "current transaction is aborted" — that is the failure path where a problem in a notification table
// stops the finding from being stored.
//
// The test deliberately ends with ROLLBACK rather than COMMIT: ALTER TABLE is transactional in PostgreSQL,
// and committing would leave that temporary constraint in the schema forever and break every later test.
// Rollback undoes the DDL on its own, with no manual cleanup. The assertion only needs "the transaction is still alive";
// it does not need a real commit.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// Defensive cleanup: drop the constraint if an earlier run left it behind.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // undo the temporary constraint; see the function comment

	// NOT VALID: only constrain rows written after this, and do not check historical events already in the database
	// (otherwise existing rows that violate it would stop the constraint from being added).
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("failed to add temporary constraint: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("reported a successful write under a constraint that must fail")
	}
	// The important assertion: the transaction is still usable.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("transaction was poisoned (savepoint did not take effect): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	// Confirm the DDL was undone with the rollback, so later tests are not left with a landmine.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("temporary constraint was not undone by rollback and would pollute later tests")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL injection"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("fan-out failed: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"catch-all channel receives high", highSQL, all.ID, true},
		{"catch-all channel receives low", lowXSS, all.ID, true},
		{"critical-only channel skips high", highSQL, onlyCritical.ID, false},
		{"critical-only channel receives critical", criticalXSS, onlyCritical.ID, true},
		{"SQL-only channel receives SQL", highSQL, sqlOnly.ID, true},
		{"SQL-only channel skips XSS", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("delivery exists: want %v got %v", tc.want, exists)
			}
		})
	}

	// A second fan-out must not create duplicate deliveries (fanned_out is idempotent).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("already-fanned events must not be processed again, got events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel covers an event that matches no channel.
// That event must still be marked fanned out, or it stays in the pending set and is rescanned every tick.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["a type that never matches"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("should not create deliveries, got %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("an event that matches no channel must still be marked fanned out, or it is rescanned forever")
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

	// A realtime claim should only take the realtime channel's row, and must not touch the digest channel.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("should claim 1 row, got %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("after claim, state should be sending and attempts=1, got state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// The joined render context must be complete (channel config + event snapshot + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("claim result is missing channel config, so rendering would fail")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id was not carried from the event, got %d", got[0].FindingID)
	}

	// The lease has not expired, so a second claim should be empty — that is the guarantee that
	// two dispatchers do not deliver the same row at once.
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("should not claim again while the lease holds, got %d rows", len(again))
	}

	// A realtime claim must not touch the digest channel's deliveries.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("realtime claim should not take digest-channel deliveries, got %d rows", len(left))
	}
}

// TestClaimExpiredLeaseRecovers covers crash recovery: a process that dies mid-delivery leaves a sending
// row, and once the lease expires that row must be claimable again, or the delivery is stuck forever.
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
		t.Fatalf("first claim failed: %v (%d rows)", err, len(first))
	}
	// Push the lease into the past by hand, simulating "the lease has expired".
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("a sending row with an expired lease should be claimable again, got %d rows", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("reclaiming should increment the attempt count, got %d", second[0].Attempts)
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
	// Disabling also marks existing waiting deliveries as skipped.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("waiting deliveries of a disabled channel should be marked skipped, got %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a disabled channel should not be claimable, got %d rows", len(got))
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

	// The batch was just created, age 0, so a 30-minute period should not be due.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("failed to check whether the batch is due: %v", err)
	}
	if due {
		t.Fatal("a batch that was just created should not be due immediately")
	}

	// Push the created time of all three deliveries into the past, simulating a batch old enough for the period.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("a batch older than the period should be due")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("failed to claim digest batch: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("digest should claim all 3 rows at once, got %d", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("a digest batch must write batch_id, or history cannot show that these rows were sent together")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("one batch should share batch_id, got %v vs %d", dl.BatchID, firstBatchID)
		}
	}

	// Fail and reschedule the whole batch, then claim it again; batch_id must keep its original value (that is what COALESCE does):
	// otherwise one retry erases the fact that these rows were sent together.
	//
	// The whole batch must be rescheduled, not just one row — that is how the delivery engine sends a digest
	// (one message stands for the whole batch, and they succeed or fail together). Rescheduling only one row
	// would leave the rest inside their lease, and the reclaim would naturally get only that one row.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "simulated failure"); err != nil {
		t.Fatal(err)
	}
	// Push the lease into the past, simulating that the backoff has elapsed.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("reclaim should get all 3 rows, got %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("batch_id should stay %d after retry, got %v", firstBatchID, reclaimed[0].BatchID)
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
		t.Fatalf("claim failed: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "network blip"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "network blip" {
		t.Fatalf("after reschedule, state should be pending and the reason recorded, got state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "retries exhausted"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("should be failed, got %s", state)
	}

	// A manual resend must clear the attempt count and become due immediately, or it inherits the old failure budget.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("resend failed: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("after resend, state should be pending and attempts=0, got state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("a resend should be claimable immediately")
	}

	// A delivery that was already sent must not be resent.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("a delivered row must not allow resend")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "paging test"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 5 {
		t.Fatalf("total should be 5, got %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("2 per page, got %d", len(page1))
	}
	// Newest first: the first id on page 1 should be greater than the first id on page 2.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("paging should be newest first, got page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// Render context must come back with the history, or the list cannot show what was pushed.
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("history item is missing display fields: %+v", page1[0])
	}

	// Filter by state: there are no pending rows.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("should have no pending deliveries, got %d rows (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("notification status-change test", "target", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL injection", Name: "status-change case",
		Severity: "high", Summary: "summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// A finding_created event was recorded when the row was stored; count it first as the baseline.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("storing a finding should record a notification event in the same transaction")
	}

	// Setting the same status must not create an event (so a repeated submit does not spam notifications).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("status update failed: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("an unchanged status must not record a notification event")
	}
	if from != "pending" {
		t.Fatalf("should return the previous status pending, got %q", from)
	}

	// A real change should record an event with from/to.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("status update failed: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("a real status change should record a notification event")
	}
	if from != "pending" {
		t.Fatalf("from should be pending, got %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("status-change event not found: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("status transition in the snapshot is wrong: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// The snapshot must carry the fields rendering needs, or the status-change message is an empty shell.
	if snap.VulnClass != "SQL injection" || snap.Severity != "high" || snap.Name != "status-change case" {
		t.Fatalf("snapshot is missing render fields: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("status should now be fixed, got %s", status)
	}

	// A finding that does not exist: found=false, and no error.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("a missing finding should return found=false and no error, got found=%v err=%v", found, err)
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
		t.Fatalf("stats failed: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("channel counts are wrong: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("should count waiting deliveries: %+v", stats)
	}
	// A delivery that was just created should have a backlog age near 0, not negative or huge.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("backlog age is not valid: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD round trip",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD round trip" {
		t.Fatalf("round-trip fields do not match: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("the default should be enabled")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("config was not stored correctly: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("filter was not stored correctly: %+v", filter)
	}

	// Update, then read again.
	got.Name = "renamed"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "renamed" || after.IsEnabled() {
		t.Fatalf("update did not take effect: %+v", after)
	}

	// After delete it should report not found, not succeed silently.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("want ErrNotificationChannelNotFound, got %v", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("deleting again should report not found, got %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate pins a place that used to be wrong:
// **0 is a valid setting and means "no rate limit"; the db layer must not treat it as "unspecified" and overwrite it with a default**.
//
// The old bug: SaveNotificationChannel had `if RatePerMin <= 0 { use the default }`,
// so the docs, the UI hint, and takeTokens all treated 0 as unlimited, while the write path quietly changed it to
// 20 (DingTalk / WeCom / Telegram) or 100 (Feishu) — the operator thought the limit was off and was actually capped,
// with no warning. Only the request body can tell "unspecified" from "explicit 0",
// so the default is filled in the server layer (see notifyCreateChannel) and the db layer only stores what it is given.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// Explicit 0 (no rate limit) must be stored as given.
	unlimited := &NotificationChannel{
		Name: "unlimited", Kind: notify.KindDingTalk, RatePerMin: 0,
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
		t.Fatalf("explicit 0 means no rate limit and must be stored as given, got %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("default mode should be realtime, got %s", got.Mode)
	}

	// A negative value is invalid input and should be rejected rather than quietly rewritten.
	bad := &NotificationChannel{
		Name: "negative limit", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("a negative rate limit should be rejected")
	}
}

// TestDeleteChannelCascadesDeliveries pins the foreign-key behavior: deleting a channel removes its delivery history
// (the config is gone, so the history cannot be interpreted), but the event itself stays — another channel may still reference it.
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
		t.Fatal("precondition failed: no delivery was created")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("deleting a channel should cascade-delete its deliveries, %d remain", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("deleting a channel must not also delete the event itself")
	}
}

// TestClaimDigestBatchHonorsCallerLimit covers a gap the audit pointed out:
// digest channels used to skip the token bucket entirely — allow was deducted by takeTokens and then unused,
// so rate_per_min did nothing in digest mode. limit now constrains the claim too.
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
	// limit=3: only 3 rows are claimed, and the rest stay in the database.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("should claim only 3 rows for the caller's rate-limit budget, got %d", len(got))
	}
	// limit=0 means this round's budget is spent: claim nothing, and do not return an error.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("a budget of 0 should claim 0 rows and not error, got %d rows err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange covers a completeness gap the audit pointed out:
// when a retest concludes "fixed", the status really does change, but that UPDATE writes the database directly
// and bypasses the version that notifies — so a channel configured with on_status_change never hears about this transition,
// the status changes quietly in the UI, and an operator has to open the platform to notice.
//
// This test pins "every path that changes status must record a status-change event".
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("retest notification test", "target", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL injection", Name: "retest target",
		Severity: "high", Summary: "summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// Create a retest record and drive it to the completed state.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("a retest should be linked to a conversation")
	}
	// A retest must enter running before a conclusion can be stored (same as the real flow).
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("failed to start retest: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "fixed", "evidence"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("failed to finish retest: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("after a retest verdict of fixed, status should be fixed, got %s", status)
	}

	// The important assertion: there must be a status-change event, with the right from/to.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("a retest verdict of fixed should record a status-change notification (or a channel with on_status_change never hears about it): %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("status transition in the snapshot is wrong: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// The snapshot must carry the fields rendering needs, or the notification is an empty shell.
	if snap.Name != "retest target" || snap.Severity != "high" {
		t.Fatalf("snapshot is missing render fields: %+v", snap)
	}
}
