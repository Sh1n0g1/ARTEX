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

// Global settings keys (stored in the settings key-value table; no dedicated table needed).
const (
	// settingNotifyEnabled is the master switch for push notifications. On by
	// default: it's meant as a one-flip kill switch during maintenance, not as
	// the feature's enablement condition — the real enablement condition is
	// "are any channels configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the externally reachable address used to build
	// finding-detail link-back URLs (e.g. https://artex.example.com). Left empty,
	// messages carry no link-back button. There was no reusable external-address
	// setting in the project, hence this new one.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest-mode period (minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. 3 seconds is the cap
	// on this engine's real-time responsiveness, and also the main source of
	// latency between "finding persisted" and "message arrives in IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration held while a delivery is claimed. It must
	// be significantly longer than the worst-case time for a single delivery (the
	// notify package's HTTP client timeout is 15 seconds), otherwise the same row
	// could be picked up and delivered by two dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick caps how many events are fanned out per round, so
	// enabling a channel for the first time doesn't unfold an entire historical
	// backlog into delivery tasks in one shot.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest period.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick caps deliveries per round for a channel with no
	// configured rate limit. It exists to stop "a channel set to unlimited plus a
	// scan that surfaces a thousand findings at once" from stalling a single
	// round for a long time.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick caps how many deliveries one channel may
	// send per round.
	//
	// This cap is derived from the **lease duration**: claiming a row stamps it
	// with a lease (notifyLease = 3 minutes); if a round delivers so many rows
	// serially that the worst-case time exceeds the lease, the later ones expire
	// before they're even sent. That's harmless within a single process (Run is
	// one goroutine running serially; a tick can't re-enter), but **two processes
	// sharing the same database** would have the other process re-claim the
	// lease-expired rows and resend them, double-incrementing attempts and
	// marking them failed while the original process is still delivering them.
	//
	// Value rationale: 3-minute lease / 30-second per-send timeout = 6 would
	// **exactly exhaust the lease** with zero margin, so it can't be used; 5
	// leaves a 30-second margin on a 150-second worst case. This relationship is
	// pinned down by TestNotifyTickBudgetFitsWithinLease — changing notifyLease,
	// notifySendTimeout, or this value alone will fail that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also constrains
	// the previous constant's value — their product must not exceed notifyLease;
	// see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the retry backoff sequence for failures, indexed by attempts
// so far. 3 chances (including the first try) correspond to db.MaxNotifyAttempts;
// the two must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for finding push notifications.
//
// It runs alongside Scheduler, as its own goroutine (see server.New). It
// deliberately does not reuse Scheduler's tick: push notifications have a
// real-time requirement (3 seconds) different from the trigger's business
// cadence, and failures in either must not affect the other — a stuck push
// should not affect agent triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu guards buckets. There are few channels and little contention, so one
	// mutex is enough; a finer-grained structure isn't worth it here.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is a single channel's token bucket.
//
// A token bucket is used instead of a "count per minute then reset" sliding
// window because the latter has nasty boundary effects: send 20 at the end of
// one window and another 20 right after, and the receiving platform sees 40 in
// one second, which gets rate-limited; a token bucket refills at a constant
// rate and naturally avoids this kind of burst.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
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

// step runs one round: fan out new events first, then deliver due tasks.
//
// Any step's failure is only logged, never stops the loop — a failure in the
// notification system must never escalate into a process-level problem. Each
// tick is independent, and the next round retries naturally.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to fan out events: %v", err)
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
		// The token bucket's unit is **messages** (equivalent to HTTP requests),
		// not findings. In realtime mode these are the same (one finding, one
		// message); in digest mode a whole batch of findings merges into one
		// message, so it consumes only one token.
		//
		// Both modes check the token bucket first and only then claim against the
		// quota — the order matters, otherwise a delivery blocked by rate
		// limiting would already have consumed a retry attempt.
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

// digestTickPlan returns a digest channel's token cost and batch-size ceiling
// for this round.
//
// The two return values are **two different dimensions**, which is exactly why
// this is its own function:
//
//   - tokens counts messages. A batch of findings merges into one message, one
//     HTTP request, so this is always 1. rate_per_min therefore still applies to
//     digest (at most this many digest messages per minute).
//   - claimLimit is how many findings this batch may hold at most. It is bounded
//     only by an in-memory ceiling, unrelated to the request budget.
//
// This function used to pass the per-round request budget
// (notifyMaxSendsPerChannelPerTick, derived from the lease) straight through as
// the batch size, in order to make rate_per_min apply to digest. The result was
// that a channel with rate_per_min=20 only refilled 1 token in a 3-second tick,
// so every digest message held only 1 finding — digest degenerated into
// "realtime push with digest wording", and readers got a stream of "1 new
// finding in the last 30 minutes" messages, while db.MaxDigestBatchSize was
// never reachable.
//
// This symptom is hard to catch in end-to-end tests (existing cases manually
// pass a large-enough limit to stepDigest, bypassing step's quota
// calculation), so the decision is pinned down here by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers one channel's realtime tasks, one finding per message.
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
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A render failure is a local data problem; retrying won't help.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message when its batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to determine digest batch due channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries with a broken snapshot that didn't make it into the message must
	// be explicitly marked failed. Otherwise they'd be left out of included —
	// neither in the message nor in the failure list — and a successful send's
	// bulk status update would miss them, leaving them stuck on "sending" forever,
	// repeatedly reclaimed once their lease expires.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot could not be parsed; this finding could not be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad-snapshot deliveries failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparsable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Only hand send the ones that made it into the message: included[i] maps
	// strictly to msg.Items[i], and send relies on that correspondence to map
	// "the channel reported the first K delivered" onto the right delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions status based on the result.
//
// One batch of deliveries (potentially dozens in digest mode) shares a single
// send result: either all delivered, or the whole batch retries. There is no
// per-item retry — a digest message is one message, and resending part of it
// would scramble the batch's semantics.
//
// The one exception is **splitting caused by a channel length cap**: when the
// channel reports that it actually only accepted the first K, item K+1 onward
// must be held for the next batch rather than marked successful along with the
// rest. Otherwise the truncated findings would end up neither in the message
// nor in the failure list, vanishing entirely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap a single delivery so one stuck channel can't drag down the rest of this round's channels.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The channel can't legitimately report more delivered than were sent;
			// if it happens, the render layer miscounted — treat it as fully
			// delivered and log the issue, which beats corrupting the records.
			log.Printf("[notify] channel reported %d delivered, exceeding %d sent, channel=%s; treating as fully delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message hit the channel's length cap: the rest go straight back
			// into the queue for the next tick to resend. Use DeferDeliveries, not
			// RescheduleDeliveries — this is not a failure and must not consume
			// retry budget (claiming already optimistically incremented it, and
			// that gets decremented there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message hit the channel's length cap; only the first %d were delivered, the rest are held for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to requeue split remainder channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither errored nor reported how many it delivered. Treat it
		// as a failure (goes through backoff) so this delivery doesn't get
		// reclaimed forever without ever resolving.
		err = fmt.Errorf("channel did not report a delivered count (delivered=%d)", delivered)
	}

	// Failure handling is decided **per item**, not by the batch's maximum attempt count.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts`, condemning
	// the whole batch at once, but attempt counts differ within a batch: an older
	// delivery already retried twice (attempts=2) would drag a brand-new delivery
	// in the same batch (attempts=1) into failed along with it — a new finding
	// would be permanently lost without using up a single retry, the opposite of
	// the intent that an old row shouldn't drag a new one down with it.
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
			log.Printf("[notify] error marking failed status channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] error marking failed status channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Reschedule grouped by delay: there are only 3 backoff tiers, so the number
	// of groups is naturally small — no need to issue a separate UPDATE per item
	// (which would turn a 500-item batch into 500 round-trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d exhausted=%d pending_retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns the items of all that are not in keep (compared by
// pointer identity). Used to find deliveries that "didn't make it into the
// message" — they must be explicitly handled and not left in limbo.
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

// adapt takes a channel's implementation and resolves its config.
// ok=false means the kind is not registered, so the delivery should be
// immediately marked failed instead of retried forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// Fall back to an empty map on a config parse failure: the channel's own
		// Validate will report which field is missing, and that error guides the
		// user to a fix better than a raw JSON-parsing error would.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single finding into a message.
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

// renderBatch renders the digest message. It parses each snapshot
// individually — one broken item is skipped rather than dragging the whole
// batch down.
//
// The return value included corresponds **strictly one-to-one** with
// msg.Items (the i-th delivery ↔ the i-th item). This correspondence is a hard
// requirement: the caller decides which of the first K deliveries to mark
// delivered based on "the channel reported accepting the first K". If a broken
// snapshot is skipped here without also removing the skipped delivery from
// included, the indices go out of sync — an item that should have failed gets
// marked delivered, while a good item gets wrongly marked undelivered. Broken
// ones are explicitly marked failed by the caller; see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A broken snapshot goes into neither the message nor included — its
			// handling is the caller's responsibility (explicitly marked failed,
			// rather than slipping through mixed in with "delivered").
			log.Printf("[notify] skipped unparsable snapshot in digest batch delivery=%d: %v", dl.ID, err)
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
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch failed to parse", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a pushable item, also resolving asset
// names and the detail link-back URL.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// A failure to resolve asset names should not block the push: missing a
		// name is far less costly than missing the notification entirely — the
		// message just has one fewer asset line.
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
		// The detail page route is web/src/app/(main)/function/findings/detail/page.tsx,
		// which reads the finding id from the "id" query parameter.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens withdraws **at most want** tokens from a channel's token bucket,
// returning the number actually taken.
//
// One token = one message (one HTTP request). In realtime mode the caller
// passes however many it wants; in digest mode a whole batch of findings sends
// only one message, so it passes 1.
//
// The bucket's capacity is the channel's per-minute cap, refilled at a
// constant rate. ratePerMin<=0 means unlimited, in which case this returns a
// finite but large-enough value, so a single round isn't stalled by an
// unbounded backlog.
//
// The want ceiling is necessary: without it the only option would be to drain
// the bucket entirely, while the caller has its own per-round cap — the
// excess tokens taken would go unused and vanish before the next refill, so
// accumulated burst capacity would never actually be reachable, and even "no
// pending deliveries this round" would still be charged for it.
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
	// Refill based on actual elapsed time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: the token count accumulates as a
	// float, and two partial refills like 0.5 + 0.5 can land on 0.9999999999,
	// which int() truncates to 0 — a bucket that's mathematically full would then
	// yield no tokens. 1e-9 is far smaller than one token, so it never masks a
	// genuine shortfall.
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

// publicBaseURL returns the external address used for link-backs, with the trailing slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest period, falling back to the default when invalid or unconfigured.
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

// parseSnapshot parses the event snapshot corresponding to a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("delivery %d has an empty event snapshot", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse delivery %d's event snapshot: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event kind is authoritative on the event row; the one in the
		// snapshot may have been written by an older version.
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
