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

// Global setting keys (stored in the settings table, so no extra table is needed).
const (
	// settingNotifyEnabled is the master switch for notifications. It defaults on. It is an emergency stop during maintenance, not the feature's enable condition. The real enable condition is whether any channel is configured.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external address used to build finding-detail back-links (for example https://artex.example.com). When empty, messages have no back-link button. The project has no reusable external-address setting, so this one was added.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest interval in minutes.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine poll interval. Three seconds is the ceiling on how live this engine is, and the main delay between a finding being stored and a message reaching chat.
	notifyTick = 3 * time.Second
	// notifyLease is the lease taken when a delivery is claimed. It must be much longer than the worst case for one delivery (the notify package HTTP client times out at 15 seconds), or two dispatchers can deliver the same row.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick caps how many events one round dispatches, so the first time a channel is enabled it does not expand the whole historical backlog into delivery jobs at once.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest interval.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no rate limit. It keeps "no rate limit plus a scan that finds thousands of findings" from blocking the loop for a long time.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is how many deliveries one channel may send per round.
	//
	// The cap is derived from the lease. Claiming a row stamps a lease (notifyLease = 3 minutes). If one round sends so many in series that the worst case exceeds the lease, later rows expire before they are sent. Inside one process that does not matter (Run is a single goroutine and a tick does not re-enter). When two processes share one database, the other side reclaims expired rows and sends them again, increments attempts twice, and marks them failed while the original process is still delivering.
	//
	// The numbers: a 3-minute lease divided by a 30-second send timeout is 6, which fills the lease with no margin, so it is not used. 5 leaves a 30-second margin on a 150-second worst case. TestNotifyTickBudgetFitsWithinLease locks that relationship. Changing notifyLease, notifySendTimeout, or this value fails that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for one delivery. It also sets the constant above. Their product must not exceed notifyLease. See TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the retry backoff sequence, indexed by attempts already made. Three chances, including the first try, match db.MaxNotifyAttempts. Change them together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for finding notifications.
//
// It runs as its own goroutine beside Scheduler (see server.New). It does not share Scheduler's tick: notifications need a 3-second cadence, which is a different rhythm from triggers, and a stuck notification must not block agent triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. There are few channels and little contention, so one mutex is enough. A finer structure is not worth it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the token bucket for one channel.
//
// A token bucket is used instead of a counter that resets every minute, because that window has a bad edge: filling 20 at the end of a window and 20 more a moment later is 40 in one second to the platform and gets rate-limited. A token bucket refills at a steady rate and avoids that burst.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. server.New starts it once.
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

// step runs one round: dispatch new events, then deliver jobs that are due.
//
// A failed step is only logged. The loop keeps going. A notification failure must never become a process-level problem. Each tick is independent, so the next round retries on its own.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to dispatch events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket counts messages (one HTTP request each), not findings. In realtime mode they match (one finding, one message). In digest mode a whole batch of findings becomes one message and spends one token.
		//
		// Both modes ask the bucket first, then claim up to that allowance. The order cannot be reversed, or a delivery blocked by the rate limit would already have spent a retry.
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

// digestTickPlan returns the token cost and the batch-size ceiling for a digest channel this round.
//
// The two return values have different units, which is why they are a function of their own:
//
//   - tokens is a message count. A batch of findings becomes one message and one HTTP request, so it is always 1. rate_per_min still applies to digest (at most that many digest messages per minute).
//   - claimLimit is how many findings this batch may hold. It is bounded only by memory, not by the request budget.
//
// rate_per_min used to be applied to digest by passing the per-round request budget (notifyMaxSendsPerChannelPerTick, derived from the lease) straight through as the batch size. A channel with rate_per_min=20 then refilled only one token in a 3-second tick, so each digest message held one finding. Digest collapsed into realtime delivery with digest wording, and readers got a string of "1 new finding in the last 30 minutes", while db.MaxDigestBatchSize was never reachable.
//
// That is hard to see in an end-to-end test (existing cases pass a large enough limit into stepDigest and skip the allowance math in step), so the decision lives here and TestDigestTickPlanDecouplesBatchSizeFromSendBudget locks it.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers a channel's realtime jobs, one finding per message.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim realtime deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A render failure is a local data problem. Retrying will not fix it.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest, when a batch is due, folds a channel's pending deliveries into one message and sends it.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to check the digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim the digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshots are broken and did not make it into the message must be marked failed explicitly. Otherwise they stay outside included, in neither the message nor the failure list. A successful send's later bulk mark misses them, and they sit in sending until the lease expires and they are claimed again.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "the event snapshot could not be parsed, so this finding could not be rendered as a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad-snapshot deliveries channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries whose snapshots could not be parsed channel=%d", len(skipped), ch.ID)
	}
	// Hand send only the deliveries that made it into the message. included[i] lines up with msg.Items[i], and send uses that to apply "the channel fit the first K items" to the right delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and moves state from the result.
//
// One batch (dozens of rows in digest mode) shares one send result: delivered, or the whole batch retries. There is no per-item retry. A digest message is one message, and resending part of it would break the batch.
//
// The exception is a split caused by the channel length limit. If the channel reports that only the first K items fit, item K+1 onward must wait for the next batch instead of being marked sent with the rest. Otherwise the truncated findings are in neither the message nor the failure list, and they disappear.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap one delivery so a stuck channel does not hold up every remaining channel in this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// A channel cannot report more items delivered than there are deliveries. If it does, the renderer miscounted. Treat them all as delivered and log it, which is safer than writing the records into a mess.
			log.Printf("[notify] channel reported %d delivered, more than %d deliveries, channel=%s; treating all as delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message hit the channel length limit. Put the rest back on the queue immediately and continue on the next tick. Use DeferDeliveries rather than RescheduleDeliveries. This is not a failure and must not spend retry budget (claiming already incremented attempts optimistically, and defer decrements them).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message hit the channel length limit; only the first %d items were delivered and the rest wait for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to queue the remainder for the next batch channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel reported neither an error nor how many items were delivered. Treat it as a failure (with backoff) so the delivery is not claimed forever without a terminal mark.
		err = fmt.Errorf("channel did not report how many items were delivered (delivered=%d)", delivered)
	}

	// Failure is decided per row, not from the highest attempt count in the batch.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts`, which killed the whole batch. Attempts differ inside a batch: an old delivery that already retried twice (attempts=2) would drag a brand-new one (attempts=1) into failed with it. The new finding would be dropped forever without using a retry, which is the opposite of keeping an old row from pulling a new one down.
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
			log.Printf("[notify] failed to mark the failure state channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d attempts: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] failed to mark the failure state channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Reschedule by delay group. There are only 3 backoff steps, so the number of groups stays small. One UPDATE per row would turn a 500-row batch into 500 round trips.
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d exhausted=%d retrying=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns the rows in all that are not in keep, compared by pointer identity. It finds deliveries that did not make it into the message. They must be handled explicitly and cannot be left in between.
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

// adapt loads the channel implementation and parses its configuration. ok=false means the type is not registered, and the delivery should fail immediately rather than retry forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On a parse failure, use an empty map. The channel's own Validate then reports which field is missing, which guides a fix better than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders one finding message.
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

// renderBatch renders a digest message. Snapshots are parsed one by one. A bad one is skipped on its own and does not drop the whole digest.
//
// included lines up with msg.Items one to one (delivery i is item i). That alignment is required: the caller marks the first K deliveries sent when the channel reports that the first K items fit. If a bad snapshot is skipped but its delivery stays in included, the indexes shift. A bad item that should fail would be marked sent, and a good item would be treated as not sent. The caller marks the bad ones failed explicitly. See stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A bad snapshot stays out of the message and out of included. The caller handles it (marks it failed explicitly, rather than letting it pass as sent).
			log.Printf("[notify] skipped an unparseable snapshot in the digest batch delivery=%d: %v", dl.ID, err)
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
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch could not be parsed", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor turns an event snapshot into an item to send, and resolves asset names and the detail back-link.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Failing to resolve asset names must not block the notification. A missing name is much lighter than a missing message. The message simply omits an asset line.
		log.Printf("[notify] failed to resolve asset names finding=%d: %v", snap.FindingID, err)
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
		// The detail route is web/src/app/(main)/function/findings/detail/page.tsx. It reads the finding id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens removes at most want tokens from the channel's bucket and returns how many were actually taken.
//
// One token is one message (one HTTP request). In realtime mode the caller passes how many it wants. In digest mode a whole batch is one message, so the caller passes 1.
//
// Capacity is the channel's per-minute limit, refilled at a steady rate. ratePerMin<=0 means no rate limit, and the function returns a finite but large enough value so an unbounded backlog cannot stall one round.
//
// The want cap is required. Without it the only option is to drain the bucket, while the caller already has a per-round cap. Extra tokens cannot be used and vanish before the next refill, so saved burst capacity is never reachable, and even a round with nothing pending would still spend some.
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
	// Refill from elapsed real time, at ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating. Tokens are a float sum, and filling in two steps can turn 0.5 + 0.5 into 0.9999999999, which int() truncates to 0. A mathematically full bucket would then yield no token. 1e-9 is far smaller than one token and does not hide a real shortfall.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for back-links, with a trailing slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest period, falling back to the default when the setting is invalid or unset.
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

// parseSnapshot parses the event snapshot for a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot for delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse the event snapshot for delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event row is the source of truth for the event type. The copy inside the snapshot may have been written by an older version.
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
