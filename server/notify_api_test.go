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

// This file covers notification behavior end to end: a finding is stored, an event is created, it is dispatched, and a real HTTP request is sent.
//
// One safety note: these cases do not call the global Notifier.step(). They call stepRealtime or stepDigest only for channels they created. step() walks every enabled channel in the database. On a development database that already has a real DingTalk or WeCom robot, a global step would push findings created during the test into those chats. Calling one channel at a time keeps the effect on the fake receiver the test built.
//
// Cleanup: when a case ends it deletes the events it created (deliveries cascade) and the channels, so real channels are not left with a backlog.
//
// What is asserted: stepRealtime and stepDigest return nothing and log internally, so these tests assert observable behavior (what the fake receiver got, and what state the delivery rows landed in), not a function return value. That is closer to the real call path than stubbing the return.

// notifyFixture is the shared fixture for the cases in this file.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// A task and exploration this fixture owns. Cases record findings here, isolated from other cases.
	taskID int64
	expID  int64
	// Events created after cleanupMark are deleted together during cleanup.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// Every fake receiver in this file listens on 127.0.0.1, and delivery refuses loopback addresses by default (to block SSRF against local services and cloud metadata). The test turns that switch on explicitly. The default refusal is covered by ssrf_test.go in the notify package.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// Create a task here. The task from the shared trafficEvidenceServer fixture does not expose an exploration id, and recording a finding requires one.
	task, err := s.m.CreateTask("notification delivery test", "delivery behavior check", nil, 0, 0)
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
	// Close the fixture on itself: mark events that already existed before the fixture was built as dispatched.
	//
	// This is required because FanOutPendingEvents is global. It expands every undispatched event onto every matching channel. The shared trafficEvidenceServer fixture records a finding of its own (the initial finding it returns), and other cases may leave leftovers. Without isolation those stray events are dispatched onto this case's channel, and assertions such as "there should be N deliveries" pass or fail depending on test order, which is harder to diagnose than a direct failure.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("failed to clean up notification events: %v", err)
		}
	})
	// The master switch must be on (another case may have turned it off).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record writes one finding through the real evidence path and returns the finding id. That path records the notification event in the same transaction, which is the hook this feature uses.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " summary",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("failed to record finding: %v", err)
	}
	return out.FindingID
}

// channel reads the channel configuration back, for calling stepX on one channel.
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("failed to read channel: %v", err)
	}
	return ch
}

// deliver dispatches events and runs one delivery round for the given channel only.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel creates a channel through the HTTP API and also covers that API's validation path.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("failed to create channel %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("unexpected create-channel response: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook is a fake receiver that records request bodies.
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
		t.Fatalf("the fake receiver got only %d requests and has no item %d", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("the fake receiver got no requests")
	}
	return f.body(t, f.count()-1)
}

// markdownText pulls the body out of the request. Field names differ: DingTalk markdown uses `text`, ActionCard uses `text`, and WeCom markdown uses `content`.
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
	t.Fatalf("request body has no recognizable text: %v", body)
	return ""
}

// agePendingBatch ages the channel's pending deliveries so a digest batch is due.
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
		"name":   "realtime push",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL injection", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("expected 1 message, got %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL injection", "High", "summary"} {
		if !strings.Contains(text, want) {
			t.Fatalf("message body is missing %q:\n%s", want, text)
		}
	}
	// The delivery should move to sent.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d deliveries are still not marked sent", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "masking case",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("failed to list channels %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("the API echoed a credential: %s", r.Body)
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
		t.Fatal("the new channel is not in the list")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("credential fields should be masked: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("the API should tell the frontend which fields are credentials")
	}

	// PATCH changes only the name and sends the masked credentials back. The real credentials must be kept as stored.
	body, _ := json.Marshal(map[string]any{
		"name":   "renamed",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("update failed %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("sending the mask back overwrote the real credential: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("sending the mask back overwrote the secret: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "renamed" {
		t.Fatal("the name was not updated")
	}

	// Explicitly clearing the secret must take effect, unlike sending the mask back, which means keep it.
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("failed to clear secret %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("an empty string should clear the secret")
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
		{"invalid type", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "invalid channel type"},
		{"missing name", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "channel name is required"},
		{"missing webhook", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "webhook"},
		{"invalid webhook scheme", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "webhook address is invalid"},
		{"invalid mode", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "invalid delivery mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("expected 400, got %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("error should mention %q, got %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("deleting a missing channel should be 404, got %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "critical only",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "low severity issue", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a finding below the threshold should not create a delivery, got %d", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("a filtered finding should not send a message")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "digest push",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("digest finding %d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// Not due yet: do not send.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("the digest batch was sent before it was due")
	}

	// After aging the batch: three findings become one message.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("three findings should fold into one message, sent %d", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "in the last") || !strings.Contains(text, "3 new vulnerabilities") {
		t.Fatalf("digest message is missing the count or time-window wording:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("digest finding %d", i)) {
			t.Fatalf("digest message is missing item %d:\n%s", i, text)
		}
	}
	// One batch should share a batch_id.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("three deliveries should share one batch_id, got distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "disabled channel",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "finding while disabled", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a disabled channel should not create deliveries, got %d", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "status change subscription",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "status change case", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("failed to change status %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// There should be two: the fixed one is the status change, and the finding_created one may go out in the same round. The status change is created later, but the test does not depend on order and searches all of them.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "Status change") && strings.Contains(text, "Fixed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no message contained \"Status change\" to \"Fixed\" (%d messages)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "no status-change subscription",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "unsubscribed change", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("failed to change status %d: %s", r.Code, r.Body)
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
		t.Fatalf("a channel that did not subscribe to status changes should not get one, got %d", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "test send",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("test send failed %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("the fake receiver should get 1 test message, got %d", hook.count())
	}
	// A test message must be obviously a test, so it is not mistaken for a real finding.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "test") {
		t.Fatalf("the test message should say it is a test: %s", text)
	}
	// When the configuration is broken, the channel's raw error should be returned to the user.
	badID := f.createChannel(t, map[string]any{
		"name":   "bad address",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("a failed delivery should return 502, got %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// Point at an address that must fail, to create a failed delivery.
	chID := f.createChannel(t, map[string]any{
		"name":   "fail and retry",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "delivery that will fail", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// Keep sending until the retry budget is exhausted.
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
		t.Fatalf("after retries are exhausted the state should be failed, got %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("failed to read history %d: %s", r.Code, r.Body)
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
		t.Fatalf("expected 1 failed delivery, got total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("history should include the failure reason, or the user cannot diagnose it")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("attempts should be recorded, got %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "delivery that will fail" {
		t.Fatalf("history should include the finding title, got %q", hist.Deliveries[0].Title)
	}

	// A manual resend should return to pending and clear the counter.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("resend failed %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("after resend the state should be pending and attempts=0, got %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta failed: %s", r.Body)
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
		t.Fatalf("meta should list all %d channels, got %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("channel %s did not report credential fields", k.Kind)
		}
	}

	// Round-trip the three global settings. A trailing slash should be normalized away, or back-links would contain "//function/...".
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("failed to write settings %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("back-link address was not normalized: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("digest interval did not take effect: %v", payload["notify_digest_interval_min"])
	}

	// Invalid values should be rejected.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s should return 400, got %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL covers back-link assembly. When public_base_url is set, a single message must use an ActionCard with a button, and the link must point at the finding detail page.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "back link",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "finding with a back link", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("a back link should use an ActionCard, got msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("back link mismatch\nwant %s\ngot %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL covers the opposite. With no external address there should be no bad link (localhost or a relative path). It should fall back to plain markdown.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "no back link",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "finding without a back link", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("with no external address the message should be markdown, got %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "View details") {
		t.Fatalf("with no external address there should be no detail link:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder is the end-to-end evidence for the silent-loss fix.
//
// A digest is limited by the channel length (WeCom markdown is 4096 bytes). When a batch does not fit, it must be split on whole items: the ones that fit are marked sent, and the rest go back on the queue for the next message. The old code marked the whole batch successful. The truncated items were in neither the message nor the failure list, history still said success, and the findings were gone.
//
// Four checks: only the items that actually fit are marked sent; the rest stay pending; deferred items did not spend a retry; another round can send the remainder and does not get stuck.
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// Use WeCom: its markdown cap is 4096 bytes, the tightest of the six channels.
	chID := f.createChannel(t, map[string]any{
		"name":   "segmented digest",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// Keep the titles long so 60 items are far past 4096 bytes and must be split.
	longName := strings.Repeat("long finding name ", 6)
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
		t.Fatalf("expected exactly one message, got %d", hook.count())
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
		t.Fatal("some items should be marked sent")
	}
	if pending == 0 {
		t.Fatalf("a batch of %d cannot fit in 4096 bytes, so some should stay pending; sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("counts do not add up: sent=%d pending=%d total=%d (neither sent nor pending means lost)", sent, pending, total)
	}
	// The message body must say how many items were left out of this message.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "continue in the next message") {
		t.Fatalf("the message should say some items were left out:\n%.400s", text)
	}

	// Deferred items must not spend retry budget. Claiming increments attempts optimistically, and deferring must decrement them.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("deferred items must not spend retries (or they would be marked failed after a few), got attempts=%d", maxAttempts)
	}

	// Keep going until it converges. The assertion is that everything is eventually sent and that it really took more than one round. That is stronger than "the second round finishes it": it shows splitting does not get stuck and does not drop the remainder.
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
			t.Fatalf("segmented delivery did not converge: %d rounds left %d unresolved", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("round %d made no progress, so the remaining %d would stay stuck", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("one 4096-byte message cannot hold %d long-titled findings, so it should take multiple rounds, but it took %d", total, rounds)
	}
	// Every round after the first should be a pure continuation. No item should be rejected by the channel.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("the fake receiver always succeeds, so there should be no failed items, got %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget guards against the two tables drifting apart.
//
// The retry budget (db.MaxNotifyAttempts) and the backoff table (notifyBackoff) live in different packages. The first is state-machine policy and the second is the engine's timing. Changing only one, for example raising the budget to 5 and forgetting a backoff step, does not fail to compile. Attempts 4 and 5 just reuse the last interval, so retries quietly slow down, and that is hard to trace back here. Asserting equal lengths makes that drift show up in CI.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("backoff steps (%d) do not match the max attempts (%d); change them together",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// Backoff intervals must be non-decreasing, or retries get more aggressive and make rate limiting worse.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("backoff intervals must be non-decreasing: step %d %v < step %d %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget locks the order "take a token, then claim". If it were reversed (claim, then drop), a delivery blocked by the rate limit would already have counted one attempt. Waiting alone would burn the budget and the row would end in failed.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// Only the token bucket is under test. It does not need a Server, and one should not be built for it.
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// One per minute: a full bucket yields at most one.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("a full bucket at 1 per minute should yield 1 token, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("an empty bucket should return 0 immediately, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("halfway through the period should not refill a whole token, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("a full period should refill 1 token, got %d", got)
	}
	// An unlimited channel uses a finite cap so one round is not stalled by an unbounded backlog.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("no rate limit should return the per-round cap %d, got %d", notifyUnlimitedBurstPerTick, got)
	}
	// Token buckets are independent per channel.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("channel 1's bucket should still be empty, got %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens locks the "take only want" behavior.
//
// The old code drained the bucket and let the caller truncate. A channel at rate=100/min filled the bucket, used 5 in one round, and threw away the other 95. A round with nothing pending was charged the same way. The comment's claim that a backlog can burst up to rate_per_min at once was then unreachable.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// The bucket starts full (100). This round wants 5.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5 should take exactly 5 tokens, got %d", got)
	}
	// The remaining 95 must still be in the bucket, not drained and discarded. Time does not advance, so what is taken can only come from the balance, not a refill.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("the remaining tokens should still be available (want 95), got %d; the bucket was drained for the whole round", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("an empty bucket should return 0, got %d", got)
	}
	// want<=0 must not spend any token. An empty round is free.
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 should return 0, got %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("the want=0 call must not spend tokens; a full 20 should still be available, got %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget locks the two units of digest mode.
//
// If the digest batch size were tied to the per-round request budget, a channel with rate_per_min=20 would refill only one token in each 3-second tick, so each digest message would hold one finding. That is not a digest, and the header would still say "1 new vulnerability in the last 30 minutes". The regression does not error, and the existing end-to-end cases miss it because they pass a large limit into stepDigest and skip the allowance math in step. This test asserts the decision itself.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// One batch is one message, one request, and one token. A token counts messages, not findings.
	if tokens != 1 {
		t.Fatalf("one digest batch sends one message and should spend exactly 1 token, got %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("digest batch size should be the memory ceiling db.MaxDigestBatchSize=%d, got %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// The batch size must be much larger than the per-round request budget. If they are the same order of magnitude, "how many messages to send" and "how many findings one batch holds" have been collapsed into one number again.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("digest batch size %d should not be bounded by the per-round request budget %d; "+
			"the request budget is how many requests the lease allows, and how many findings one batch holds is a different unit",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease is another drift guard.
//
// The per-channel per-round send cap (notifyMaxSendsPerChannelPerTick) is derived from the lease. The worst case for sending that many in series must stay under the lease, or later rows expire before they are sent and another instance reclaims and resends them. The three constants live in different places. Changing any one can break the relationship with no compile error, so the test locks it.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("the worst case for one channel in one round %v must stay under the lease %v "+
			"(notifyMaxSendsPerChannelPerTick=%d x notifySendTimeout=%v); "+
			"changing any of these three constants means checking the other two",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
