// This file is Task Master task 7: the Discord streaming outbound worker —
// the subscriber that turns an agent's chat run into visible Discord
// messages, mirroring telegram/outbound.go's shape (Register(bus), a
// per-task streaming placeholder, throttled edits, and a EventChatDone
// finalize path) adapted to what Discord's REST API actually offers.
//
// # Streaming UX: placeholder-then-edit, throttled
//
// Discord has no native "streaming message" primitive any more than
// Telegram does. The UX is simulated the same way telegram/outbound.go
// documents: post one placeholder message on the first EventTaskMessage text
// frame for a task, then PATCH it (discordAPI.EditMessage, sender.go) as the
// transcript grows, and land the definitive content on EventChatDone.
//
// editThrottleInterval bounds how often that PATCH fires per task. Discord
// rate-limits message routes per-channel in short, bucketed windows (its own
// docs do not publish an exact "edits per second" number the way Telegram's
// community documents ~1/sec for editMessageText, so this package cannot cite
// a precise ceiling) — editing on every token would turn one streamed reply
// into dozens of requests a few seconds apart and risk a 429 well before the
// run finishes. 3 seconds keeps a long-running generation visibly "live"
// (an update roughly every 2-4 lines of typical assistant output) while
// bounding one task to at most one edit every 3s, comfortably inside any
// per-channel message-route bucket Discord is known to apply elsewhere
// (compare Telegram's 2.5s, chosen for a documented ~1/sec ceiling with
// margin — Discord's is undocumented, so this errs more conservative).
// EventChatDone's own delivery never depends on this cadence: the final
// PATCH/POST goes through sender.go's bounded 429-aware retry
// (sendWithRetry/editWithRetry), so a throttled stream never costs the user
// the definitive answer, only how "live" the in-progress typing looked.
//
// # CRITICAL: this worker MUST be able to run on ANY replica (bug #7215)
//
// Every code path below resolves its destination fresh, per event, from
// db.ChannelTaskDelivery — an immutable snapshot of the task's channel
// binding written once when the task is created (see
// CreateChannelTaskDeliveryFromSession) — and posts over the same stateless,
// bounded-timeout REST client sender.go already documents as immune to
// multica-ai/multica#7215 (see sender.go's package doc: "Outbound MUST be
// stateless REST, never the Gateway"). o.streams below is a same-process
// OPTIMIZATION ONLY: it lets the replica that is already streaming a task's
// partials reuse its own placeholder message id when EventChatDone lands on
// it. It is never required for correctness — TestFinalize_NoPriorStreamState
// (outbound_test.go) calls the EventChatDone path directly, with an empty
// o.streams and no OnIngested/Gateway/resume state anywhere in the process,
// and asserts delivery still succeeds by posting a fresh message. This is
// deliberately the opposite of wecom/outbound.go's SINGLE-REPLICA
// CONSTRAINT (wecom/outbound.go:15-23, bug #7215): WeCom's only outbound
// path is the in-process WebSocket held by whichever replica owns the bot's
// lease, so EventChatDone firing on a different replica silently drops the
// reply. This file must never grow a dependency on discordChannel's Gateway
// connection, ResumeCache or Reconnector (discord_channel.go) — doing so
// would reproduce #7215 for Discord.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// editThrottleInterval is the minimum spacing between streaming-placeholder
// PATCHes for one task. See this file's package doc for the exact reasoning.
const editThrottleInterval = 3 * time.Second

// streamPlaceholder is the first frame's content while the first tokens of
// an agent reply are still arriving.
const streamPlaceholder = "…"

// taskFailedText is posted when the agent run fails outright, mirroring
// telegram.taskFailedText.
const taskFailedText = "❌ The agent run failed. Please try again."

// maxInFlightFinalizes bounds how many EventChatDone finalizations this
// process runs concurrently. events.Bus dispatches EventChatDone
// synchronously (see events.Bus.Publish), so handleChatDone below only ever
// enqueues; doing the chunked-send/retry network work inline would stall
// every other SubscribeAll listener on the bus behind Discord rate-limit
// backoff. The bound exists so an unbounded burst of finished chats cannot
// spawn unbounded goroutines; a burst beyond capacity is logged and counted
// as a dropped delivery (see metrics) rather than delivered out of order or
// silently discarded — it still reaches the retry-capable sender.go path,
// just after a queued task instead of alongside it.
const maxInFlightFinalizes = 8

// outboundQueries is the slice of generated queries this subscriber needs.
// *db.Queries satisfies it. Mirrors telegram.outboundQueries exactly — same
// generic channel_* tables, filtered to channel_type='discord'.
//
// SetChatMessageChannelOutboundProvenanceByTask and
// RecordChannelOutboundMessage mirror slack.outboundQueries
// (slack/outbound.go) exactly: they let finalize (below) leave the same
// durable delivery ledger Slack already writes, on the same channel-agnostic
// schema (channel_outbound_message, chat_message.channel_outbound_*) — see
// docs/discord-outbound-persistence-parity-decision-2026-09-04.md.
type outboundQueries interface {
	GetChannelTaskDelivery(ctx context.Context, taskID pgtype.UUID) (db.ChannelTaskDelivery, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
	SetChatMessageChannelOutboundProvenanceByTask(ctx context.Context, arg db.SetChatMessageChannelOutboundProvenanceByTaskParams) (int64, error)
	RecordChannelOutboundMessage(ctx context.Context, arg db.RecordChannelOutboundMessageParams) error
}

// DeliveryMetrics records the PRD's generated-vs-delivered ratio (see
// internal/metrics/channel_delivery.go). Optional: NewOutbound installs a
// no-op implementation when nil so tests and callers that do not care about
// metrics never need to supply one.
type DeliveryMetrics interface {
	RecordReplyGenerated(channelType string)
	RecordReplyDelivered(channelType, outcome string)
}

type noopDeliveryMetrics struct{}

func (noopDeliveryMetrics) RecordReplyGenerated(string)         {}
func (noopDeliveryMetrics) RecordReplyDelivered(string, string) {}

// replyTarget is the resolved Discord destination for one task's reply.
type replyTarget struct {
	streamKey string // taskID, string form; also the o.streams map key
	channelID string
	replyTo   string // Discord message id to quote on the very first post, or ""
	botToken  string

	// installationID/bindingID/routeRevision are carried onto replyTarget
	// solely so finalize's post-delivery ledger write (persistDelivery) can
	// populate RecordChannelOutboundMessageParams/
	// SetChatMessageChannelOutboundProvenanceByTaskParams without a second
	// lookup — they are read off db.ChannelTaskDelivery in resolveTarget,
	// which already loads that row for every other field above.
	installationID pgtype.UUID
	bindingID      pgtype.UUID
	routeRevision  int64
}

// streamState tracks one in-flight streamed reply, same-process only — see
// this file's package doc for why nothing here is load-bearing for
// correctness.
type streamState struct {
	mu          sync.Mutex
	messageID   string // placeholder message id; "" until the first POST lands
	accumulated string
	lastEdit    time.Time
	editing     bool // true while a PATCH for this stream is in flight
}

// Outbound is the Discord streaming outbound subscriber: EventTaskMessage
// drives the throttled placeholder edit, EventChatDone drives the definitive
// finalize send. Register it on the shared events.Bus and call Start once
// the process is ready to deliver (main.go owns the lifecycle exactly as it
// does for telegram.Outbound).
type Outbound struct {
	q       outboundQueries
	decrypt Decrypter
	apiBase string
	client  *http.Client
	logger  *slog.Logger
	metrics DeliveryMetrics

	now func() time.Time

	mu      sync.Mutex
	streams map[string]*streamState

	baseCtx   context.Context
	baseDone  context.CancelFunc
	startOnce sync.Once
	sem       chan struct{}
	wg        sync.WaitGroup
}

// NewOutbound builds the Discord outbound subscriber. apiBase/client default
// exactly like newDiscordAPI (empty apiBase -> defaultAPIBase, nil client ->
// a bounded-timeout http.Client); tests supply an httptest server URL.
func NewOutbound(q outboundQueries, decrypt Decrypter, apiBase string, client *http.Client, metrics DeliveryMetrics, logger *slog.Logger) *Outbound {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = noopDeliveryMetrics{}
	}
	return &Outbound{
		q:       q,
		decrypt: decrypt,
		apiBase: apiBase,
		client:  client,
		logger:  logger,
		metrics: metrics,
		now:     time.Now,
		streams: make(map[string]*streamState),
		sem:     make(chan struct{}, maxInFlightFinalizes),
	}
}

// Register subscribes to the transcript / completion / failure events.
// Mirrors telegram.Outbound.Register.
func (o *Outbound) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventTaskMessage, o.handleTaskMessage)
	bus.Subscribe(protocol.EventChatDone, o.handleChatDone)
	bus.Subscribe(protocol.EventTaskFailed, o.handleTaskFailed)
	bus.Subscribe(protocol.EventTaskCancelled, o.handleTaskCancelled)
}

// Start owns the base context every asynchronous finalize goroutine derives
// its own bounded-timeout context from, so shutdown (via WaitWithTimeout,
// then cancelling ctx) stops in-flight delivery attempts promptly instead of
// leaking past process shutdown. Call once; wiring: in cmd/server/router.go
// next to telegramOutbound.Register(bus), add
//
//	discordOutbound := discord.NewOutbound(queries, box.Open, "", nil, registry.ChannelDelivery, slog.Default())
//	discordOutbound.Register(bus)
//	h.DiscordOutbound = discordOutbound
//
// and in cmd/server/main.go next to `h.TelegramOutbound.Start(sweepCtx)` add
// `if h.DiscordOutbound != nil { h.DiscordOutbound.Start(sweepCtx) }`, plus a
// WaitWithTimeout(5*time.Second) call alongside TelegramOutbound's on
// shutdown.
func (o *Outbound) Start(ctx context.Context) {
	o.startOnce.Do(func() {
		o.baseCtx, o.baseDone = context.WithCancel(ctx)
	})
}

// WaitWithTimeout waits for every in-flight finalize to finish, bounded by
// timeout, then cancels the base context so anything still running is asked
// to stop. Mirrors telegram.Outbound.WaitWithTimeout. Safe to call even if
// Start was never called.
func (o *Outbound) WaitWithTimeout(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		o.wg.Wait()
		close(done)
	}()
	ok := false
	select {
	case <-done:
		ok = true
	case <-time.After(timeout):
	}
	if o.baseDone != nil {
		o.baseDone()
	}
	return ok
}

// finalizeCtx derives a bounded-timeout context from the base context Start
// installed, or from context.Background() if Start was never called (tests,
// and callers that intentionally run the subscriber without the full
// process lifecycle).
func (o *Outbound) finalizeCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	base := o.baseCtx
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, timeout)
}

// ---- streaming partials (EventTaskMessage) ----

// handleTaskMessage streams a partial: on each agent text frame, throttled,
// update the placeholder message. Bus delivery is synchronous, so this runs
// under a tight timeout and never propagates an error — a failed or skipped
// stream edit never costs the user anything, because EventChatDone always
// lands the definitive content regardless (see finalize).
func (o *Outbound) handleTaskMessage(e events.Event) {
	payload, ok := e.Payload.(protocol.TaskMessagePayload)
	if !ok || payload.Type != "text" || payload.Content == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	target, err := o.resolveTarget(ctx, e)
	if err != nil || target == nil {
		return
	}

	st := o.getOrCreateStream(target.streamKey)
	st.mu.Lock()
	st.accumulated += payload.Content
	snapshot := st.accumulated
	st.mu.Unlock()

	o.pushPartial(ctx, target, st, snapshot)
}

// pushPartial posts the placeholder on the first flush and PATCHes it after,
// throttled to editThrottleInterval. Best-effort: a failed or throttled edit
// is dropped, never retried here — retrying belongs to the definitive
// EventChatDone send (sender.go), not to an interim partial the user will
// see replaced within seconds anyway.
func (o *Outbound) pushPartial(ctx context.Context, target *replyTarget, st *streamState, snapshot string) {
	st.mu.Lock()
	if st.editing {
		st.mu.Unlock()
		return
	}
	now := o.now()
	if !st.lastEdit.IsZero() && now.Sub(st.lastEdit) < editThrottleInterval {
		st.mu.Unlock()
		return
	}
	msgID := st.messageID
	st.editing = true
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		st.editing = false
		st.mu.Unlock()
	}()

	text := snapshot
	if chunks := chunkMessage(formatDiscordMarkdown(text), maxMessageChars); len(chunks) > 0 {
		text = chunks[0]
	} else {
		text = formatDiscordMarkdown(text)
	}

	api := newDiscordAPI(o.apiBase, target.botToken, o.client)
	if msgID == "" {
		payload := discordMessagePayload{Content: firstNonEmptyDiscord(text, streamPlaceholder)}
		if target.replyTo != "" {
			payload.MessageReference = &discordMessageReference{MessageID: target.replyTo}
		}
		m, err := api.CreateMessage(ctx, target.channelID, payload)
		if err != nil {
			o.logger.WarnContext(ctx, "discord outbound: stream placeholder post failed", "error", err)
			return
		}
		st.mu.Lock()
		st.messageID = m.ID
		st.lastEdit = o.now()
		st.mu.Unlock()
		return
	}

	if _, err := api.EditMessage(ctx, target.channelID, msgID, discordMessagePayload{Content: text}); err != nil {
		o.logger.WarnContext(ctx, "discord outbound: stream edit failed", "error", err)
		return
	}
	st.mu.Lock()
	st.lastEdit = o.now()
	st.mu.Unlock()
}

// ---- finalize (EventChatDone) ----

// handleChatDone only enqueues work: events.Bus dispatches synchronously, so
// doing network I/O here would stall every other SubscribeAll listener
// behind Discord rate-limit backoff (see maxInFlightFinalizes' doc).
func (o *Outbound) handleChatDone(e events.Event) {
	taskID, ok := eventTaskID(e)
	if !ok {
		o.logger.Error("discord outbound: chat:done missing task id", "chat_session_id", e.ChatSessionID)
		return
	}
	key := util.UUIDToString(taskID)
	content := chatDoneContent(e.Payload)
	if content == "" {
		// No agent text this turn (e.g. a no_response turn) — nothing to
		// deliver; just drop any streaming state.
		o.clearStream(key)
		return
	}

	o.metrics.RecordReplyGenerated(string(TypeDiscord))

	select {
	case o.sem <- struct{}{}:
	default:
		o.logger.Error("discord outbound: finalize queue saturated; dropping reply",
			"task_id", key, "in_flight_limit", maxInFlightFinalizes)
		o.metrics.RecordReplyDelivered(string(TypeDiscord), "dropped")
		o.clearStream(key)
		return
	}

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() { <-o.sem }()
		o.finalize(e, taskID, key, content)
	}()
}

// finalize resolves the destination fresh (see this file's #7215 package
// doc), then sends the definitive content: if a same-process placeholder
// exists it is PATCHed with the first chunk, otherwise the first chunk is
// posted fresh (optionally quoting the original inbound message); any
// remaining chunks are posted as additional messages, mirroring sender.Send.
func (o *Outbound) finalize(e events.Event, taskID pgtype.UUID, key, content string) {
	ctx, cancel := o.finalizeCtx(30 * time.Second)
	defer cancel()

	target, err := o.resolveTarget(ctx, e)
	if err != nil {
		o.logger.WarnContext(ctx, "discord outbound: resolve finalize target failed", "task_id", key, "error", err)
		o.metrics.RecordReplyDelivered(string(TypeDiscord), "failed")
		o.clearStream(key)
		return
	}
	if target == nil {
		// Not a Discord task (or the installation was revoked between
		// trigger and reply) — nothing to deliver, and not a failure.
		o.clearStream(key)
		return
	}

	st := o.takeStream(key)
	chunks := chunkMessage(formatDiscordMarkdown(content), maxMessageChars)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	snd := newSender(newDiscordAPI(o.apiBase, target.botToken, o.client), o.logger)

	// deliveredIDs collects every message id Discord actually returned for
	// this reply, in post order, so a successful delivery can be recorded in
	// the outbound ledger below — mirroring slack/outbound.go:151-157's
	// messageIDs collection.
	deliveredIDs := make([]string, 0, len(chunks))

	var deliverErr error
	next := 0
	if st != nil {
		st.mu.Lock()
		placeholderID := st.messageID
		st.mu.Unlock()
		if placeholderID != "" {
			if m, err := snd.editWithRetry(ctx, target.channelID, placeholderID, discordMessagePayload{Content: chunks[0]}); err != nil {
				// The placeholder edit itself is retried by editWithRetry; if
				// it still failed after exhausting retries, fall back to a
				// fresh post so the reply is not lost outright.
				o.logger.WarnContext(ctx, "discord outbound: finalize edit failed, falling back to a new message",
					"task_id", key, "error", err)
			} else {
				next = 1
				deliveredIDs = append(deliveredIDs, m.ID)
			}
		}
	}

	for i := next; i < len(chunks); i++ {
		payload := discordMessagePayload{Content: chunks[i]}
		if i == 0 && target.replyTo != "" {
			payload.MessageReference = &discordMessageReference{MessageID: target.replyTo}
		}
		m, err := snd.sendWithRetry(ctx, target.channelID, payload)
		if err != nil {
			deliverErr = fmt.Errorf("post chunk %d/%d: %w", i+1, len(chunks), err)
			break
		}
		deliveredIDs = append(deliveredIDs, m.ID)
	}

	if deliverErr != nil {
		o.logger.WarnContext(ctx, "discord outbound: finalize delivery failed", "task_id", key, "error", deliverErr)
		o.metrics.RecordReplyDelivered(string(TypeDiscord), "failed")
		return
	}
	o.metrics.RecordReplyDelivered(string(TypeDiscord), "delivered")

	// Persist strictly AFTER delivery has already succeeded — see
	// persistDelivery's own doc comment for why a ledger-write failure here
	// must never retroactively change the outcome recorded above.
	o.persistDelivery(ctx, target, taskID, key, deliveredIDs)
}

// persistDelivery writes the same two-part delivery ledger Slack already
// writes for every successful reply (slack/outbound.go:158-184):
// SetChatMessageChannelOutboundProvenanceByTask once for the whole reply
// (the assistant chat_message row's provenance columns), then
// RecordChannelOutboundMessage once per delivered chunk id (the
// channel_outbound_message ledger). Both are already-generic, already-proven
// idempotent writes (RecordChannelOutboundMessage is an
// `ON CONFLICT ... DO NOTHING` insert) — see
// docs/discord-outbound-persistence-parity-decision-2026-09-04.md.
//
// # Deliberate divergence from Slack: log-and-continue, not return an error
//
// slack/outbound.go's processEvent returns fmt.Errorf on either write
// failing, and its only caller (handleEvent) just logs that return value —
// the error never reaches, gates, or unwinds anything user-visible. Discord's
// finalize runs the same way (its caller, the goroutine spawned from
// handleChatDone, does not inspect finalize's return value either, and it has
// none), so propagating an error here would add a return path with no
// different effect, while a naive future refactor could accidentally wire it
// into the delivery outcome above. By the time this function runs,
// deliverErr == nil is already guaranteed by the caller: the reply is on
// Discord's servers. A ledger-write failure must never turn that into a
// reported failure, and must never delay or gate the delivery that already
// happened — so this logs and returns, never touching
// RecordReplyDelivered's "delivered" outcome already recorded above.
func (o *Outbound) persistDelivery(ctx context.Context, target *replyTarget, taskID pgtype.UUID, key string, ids []string) {
	if len(ids) == 0 {
		return
	}
	// This function is the first place finalize hands control to an external
	// dependency (a DB call via o.q) from inside a goroutine handleChatDone
	// spawns directly (outbound.go, not through events.Bus.Publish). Bus.Publish
	// (internal/events/bus.go) only recovers panics from the SYNCHRONOUS
	// handler call it makes; it has no visibility into — and cannot protect —
	// work a handler goes on to do in its own background goroutine, which is
	// exactly finalize's shape. Before this file's persistence write points
	// existed, nothing reachable from that goroutine could panic on a
	// data-dependent code path (network/marshal errors were already values,
	// not panics), so the gap was real but dormant. o.q is a caller-supplied
	// interface — a queries implementation panicking (nil pointer, driver
	// bug, a future test/fake) is exactly the kind of failure this delivery
	// path must survive: the reply has already been sent to Discord by the
	// time persistDelivery runs (see this function's doc above), so a panic
	// here must never take the whole backend process down with it. Recover
	// and log, mirroring bus.go's own established idiom for the same
	// problem.
	defer func() {
		if r := recover(); r != nil {
			o.logger.ErrorContext(ctx, "discord outbound: panic in outbound ledger write recovered",
				"task_id", key, "recovered", r)
		}
	}()
	rows, err := o.q.SetChatMessageChannelOutboundProvenanceByTask(ctx, db.SetChatMessageChannelOutboundProvenanceByTaskParams{
		ChannelType:    pgtype.Text{String: string(TypeDiscord), Valid: true},
		InstallationID: target.installationID,
		ChannelChatID:  pgtype.Text{String: target.channelID, Valid: true},
		MessageIds:     ids,
		TaskID:         taskID,
	})
	if err != nil {
		o.logger.WarnContext(ctx, "discord outbound: record reply provenance failed",
			"task_id", key, "error", err)
	} else if rows != 1 {
		o.logger.WarnContext(ctx, "discord outbound: record reply provenance updated unexpected row count",
			"task_id", key, "rows", rows)
	}

	for _, id := range ids {
		if err := o.q.RecordChannelOutboundMessage(ctx, db.RecordChannelOutboundMessageParams{
			OutboundInstallationID: target.installationID,
			OutboundChannelType:    string(TypeDiscord),
			OutboundMessageID:      id,
			OutboundBindingID:      target.bindingID,
			OutboundRouteRevision:  target.routeRevision,
			OutboundTaskID:         taskID,
			OutboundKind:           "task_reply",
		}); err != nil {
			o.logger.WarnContext(ctx, "discord outbound: record outbound message failed",
				"task_id", key, "message_id", id, "error", err)
		}
	}
}

// ---- task failure / cancellation ----

// handleTaskFailed clears any stream state and best-effort posts a failure
// notice, mirroring telegram.Outbound.handleTaskFailed. Never retried: a
// failure notice that itself fails to send is logged and dropped rather than
// competing for the same bounded finalize budget as a real reply.
func (o *Outbound) handleTaskFailed(e events.Event) {
	if taskFailureRetryPending(e.Payload) {
		o.clearStreamFromEvent(e)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target, err := o.resolveTarget(ctx, e)
	if err != nil || target == nil {
		o.clearStreamFromEvent(e)
		return
	}
	st := o.takeStream(target.streamKey)

	api := newDiscordAPI(o.apiBase, target.botToken, o.client)
	if st != nil {
		st.mu.Lock()
		placeholderID := st.messageID
		st.mu.Unlock()
		if placeholderID != "" {
			if _, err := api.EditMessage(ctx, target.channelID, placeholderID, discordMessagePayload{Content: taskFailedText}); err == nil {
				return
			}
		}
	}
	payload := discordMessagePayload{Content: taskFailedText}
	if target.replyTo != "" {
		payload.MessageReference = &discordMessageReference{MessageID: target.replyTo}
	}
	if _, err := api.CreateMessage(ctx, target.channelID, payload); err != nil {
		o.logger.WarnContext(ctx, "discord outbound: failure notice failed", "error", err)
	}
}

// handleTaskCancelled drops local stream state without altering the partial
// Discord message already visible, mirroring
// telegram.Outbound.handleTaskCancelled's reasoning: cancellation has no
// final agent answer, so overwriting user-visible partial content with a
// synthetic notice would be worse than leaving it as-is.
func (o *Outbound) handleTaskCancelled(e events.Event) {
	o.clearStreamFromEvent(e)
}

// ---- shared stream-map bookkeeping ----

func (o *Outbound) getOrCreateStream(key string) *streamState {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, ok := o.streams[key]
	if !ok {
		st = &streamState{}
		o.streams[key] = st
	}
	return st
}

// takeStream removes and returns key's stream state, or nil if none exists —
// used by finalize/handleTaskFailed so a placeholder is claimed by exactly
// one terminal path.
func (o *Outbound) takeStream(key string) *streamState {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.streams[key]
	delete(o.streams, key)
	return st
}

func (o *Outbound) clearStream(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.streams, key)
}

func (o *Outbound) clearStreamFromEvent(e events.Event) {
	if taskID, ok := eventTaskID(e); ok {
		o.clearStream(util.UUIDToString(taskID))
	}
}

// ---- target resolution ----

// resolveTarget maps an event's immutable task-delivery snapshot to Discord
// credentials + destination channel. A missing snapshot, or one belonging to
// a non-Discord channel, means this event is not this adapter's concern.
// This is the single seam the #7215 guarantee (this file's package doc)
// rests on: it reads only db.ChannelTaskDelivery + db.ChannelInstallation,
// nothing process-local.
func (o *Outbound) resolveTarget(ctx context.Context, e events.Event) (*replyTarget, error) {
	taskID, ok := eventTaskID(e)
	if !ok {
		return nil, nil
	}
	delivery, err := o.q.GetChannelTaskDelivery(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup discord task delivery: %w", err)
	}
	if delivery.ChannelType != string(TypeDiscord) {
		return nil, nil
	}
	inst, err := o.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID:          delivery.InstallationID,
		ChannelType: string(TypeDiscord),
	})
	if err != nil {
		return nil, fmt.Errorf("load discord installation: %w", err)
	}
	if inst.Status != "active" {
		return nil, nil // revoked between trigger and reply
	}
	creds, err := decodeCredentials(inst.Config, o.decrypt)
	if err != nil {
		return nil, fmt.Errorf("decode discord credentials: %w", err)
	}
	var replyTo string
	if delivery.ChannelMessageID.Valid {
		replyTo = delivery.ChannelMessageID.String
	}
	return &replyTarget{
		streamKey:      util.UUIDToString(taskID),
		channelID:      delivery.ChannelChatID,
		replyTo:        replyTo,
		botToken:       creds.BotToken,
		installationID: delivery.InstallationID,
		bindingID:      delivery.BindingID,
		routeRevision:  delivery.RouteRevision,
	}, nil
}

// eventTaskID extracts the task id from the event envelope or payload,
// mirroring telegram.eventTaskID.
func eventTaskID(e events.Event) (pgtype.UUID, bool) {
	raw := e.TaskID
	if raw == "" {
		switch p := e.Payload.(type) {
		case protocol.ChatDonePayload:
			raw = p.TaskID
		case map[string]any:
			raw, _ = p["task_id"].(string)
		}
	}
	id, err := util.ParseUUID(raw)
	return id, err == nil && id.Valid
}

// chatDoneContent extracts the reply text from an EventChatDone payload,
// mirroring telegram.chatDoneContent.
func chatDoneContent(payload any) string {
	switch p := payload.(type) {
	case protocol.ChatDonePayload:
		return p.Content
	case map[string]any:
		if s, ok := p["content"].(string); ok {
			return s
		}
	}
	return ""
}

// taskFailureRetryPending mirrors telegram.taskFailureRetryPending: a
// retry-pending failure is not terminal, so the stream is simply dropped
// rather than replaced with a failure notice a retry will shortly overwrite.
func taskFailureRetryPending(payload any) bool {
	fields, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	retryPending, _ := fields["retry_pending"].(bool)
	return retryPending
}

func firstNonEmptyDiscord(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
