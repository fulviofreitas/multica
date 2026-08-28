// This file is Task Master subtask 3.5: the Discord typing indicator.
//
// It mirrors telegram/resolvers.go's typingNotifier (NewTypingNotifier /
// OnIngested / OnSettled) in every way it can, but Discord's REST typing
// indicator has one property Telegram's sendChatAction does not: it
// self-expires after ~10 seconds
// (https://discord.com/developers/docs/resources/channel#trigger-typing-indicator),
// while a real agent run routinely takes far longer than that. A single
// fire-and-forget POST on ingest — Telegram's whole strategy, hence its
// OnSettled being a no-op — would make the indicator flicker off mid-run,
// which is the exact "is this actually working?" experience a typing
// indicator exists to prevent.
//
// So this notifier keeps posting the indicator on a timer, keyed per
// session, until OnSettled arrives for that session, its context is
// cancelled, or a safety timeout elapses — whichever comes first. See the
// two constants below for the exact numbers and why they were chosen.
package discord

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// discordTypingRefreshInterval is how often the loop re-POSTs the typing
// indicator while a run is in flight. Discord's indicator expires ~10s after
// the last trigger; 8s leaves ~2s of margin against scheduling jitter, a slow
// HTTP round trip, or a delayed goroutine wakeup, while still comfortably
// avoiding a double-post inside one expiry window.
const discordTypingRefreshInterval = 8 * time.Second

// discordTypingMaxLifetime bounds how long a single loop may run when
// OnSettled never arrives for its session — e.g. the process that would have
// called it crashed, or an upstream bug drops the event. An agent run
// routinely takes minutes, so the bound must clear any real run, but it must
// not be "forever": 10 minutes is generous headroom over ordinary run
// durations while still guaranteeing every loop this notifier ever starts
// eventually stops posting and exits, with no operator action required.
const discordTypingMaxLifetime = 10 * time.Minute

// discordTypingPostTimeout bounds a single typing POST so one slow or hung
// request cannot stall the refresh loop's ticker cadence or eat into the
// loop's max lifetime budget.
const discordTypingPostTimeout = 5 * time.Second

var _ engine.TypingNotifier = (*discordTypingNotifier)(nil)

// discordTypingLoop is the per-session refresh goroutine's handle. done is
// closed when the loop's for-select returns, after it has deregistered
// itself — tests use it to observe loop exit deterministically instead of
// sleeping and hoping.
type discordTypingLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// discordTypingNotifier shows Discord's native typing indicator when a
// message is ingested and keeps refreshing it until the run settles. Unlike
// Telegram's typingNotifier, OnSettled is NOT a no-op — see the package doc
// above.
type discordTypingNotifier struct {
	decrypt Decrypter
	apiBase string
	client  *http.Client
	logger  *slog.Logger

	refreshInterval time.Duration
	maxLifetime     time.Duration

	mu    sync.Mutex
	loops map[pgtype.UUID]*discordTypingLoop
}

// NewDiscordTypingNotifier builds the Discord typing indicator notifier.
// base and client are both optional, mirroring newDiscordAPI: base defaults
// to the production Discord API host, client defaults to a bounded-timeout
// http.Client. Tests supply an httptest server URL as base.
func NewDiscordTypingNotifier(decrypt Decrypter, apiBase string, client *http.Client, logger *slog.Logger) engine.TypingNotifier {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	if client == nil {
		client = &http.Client{Timeout: discordTypingPostTimeout}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &discordTypingNotifier{
		decrypt:         decrypt,
		apiBase:         apiBase,
		client:          client,
		logger:          logger,
		refreshInterval: discordTypingRefreshInterval,
		maxLifetime:     discordTypingMaxLifetime,
		loops:           make(map[pgtype.UUID]*discordTypingLoop),
	}
}

// OnIngested starts (or restarts) the refresh loop for sessionID and fires
// the first typing POST synchronously so the indicator appears immediately.
// Every failure here (bad platform value, undecryptable credentials, no
// channel id) is logged and swallowed: a typing indicator must never block or
// fail message ingestion, matching telegram.typingNotifier.OnIngested.
func (n *discordTypingNotifier) OnIngested(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, sessionID pgtype.UUID) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return
	}
	// discordSessionRouting (resolvers.go) uses msg.Source.ChatID as the
	// Discord channel_id for every surface (DM, guild channel, thread) — read
	// it the same way here rather than re-deriving it from Raw.
	channelID := msg.Source.ChatID
	if channelID == "" {
		return
	}
	creds, err := decodeCredentials(row.Config, n.decrypt)
	if err != nil {
		n.logger.WarnContext(ctx, "discord typing: decode credentials failed", "error", err)
		return
	}

	// The loop must outlive this call's ctx (a request/event-handling ctx
	// that may be cancelled the moment OnIngested returns), so it runs on a
	// detached context bounded only by the safety timeout.
	loopCtx, cancel := context.WithTimeout(context.Background(), n.maxLifetime)
	loop := &discordTypingLoop{cancel: cancel, done: make(chan struct{})}

	n.mu.Lock()
	if old, exists := n.loops[sessionID]; exists {
		// A prior loop for this session is still registered (e.g. OnSettled
		// never arrived for it, or a new run started before the old one's
		// safety timeout). Stop it before replacing it so at most one loop
		// per session ever posts.
		old.cancel()
	}
	n.loops[sessionID] = loop
	n.mu.Unlock()

	n.postTyping(loopCtx, creds.BotToken, channelID)
	go n.runLoop(loopCtx, sessionID, loop, creds.BotToken, channelID)
}

// OnSettled stops the refresh loop for sessionID, if one is running.
// Idempotent: settling a session with no loop (already stopped, safety
// timeout already fired, or never started) is a no-op, matching
// engine.TypingNotifier's documented contract.
func (n *discordTypingNotifier) OnSettled(ctx context.Context, sessionID pgtype.UUID) {
	n.mu.Lock()
	loop, ok := n.loops[sessionID]
	if ok {
		delete(n.loops, sessionID)
	}
	n.mu.Unlock()
	if ok {
		loop.cancel()
	}
}

// runLoop refreshes the typing indicator on n.refreshInterval until ctx ends
// (OnSettled cancelled it, or the safety timeout elapsed), then deregisters
// itself and closes loop.done. Guaranteed to terminate: the for-select only
// blocks on the ticker and ctx.Done(), and ctx always has a deadline
// (n.maxLifetime) even if OnSettled never arrives.
func (n *discordTypingNotifier) runLoop(ctx context.Context, sessionID pgtype.UUID, loop *discordTypingLoop, token, channelID string) {
	defer close(loop.done)
	defer loop.cancel()
	defer n.clearLoop(sessionID, loop)

	ticker := time.NewTicker(n.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.postTyping(ctx, token, channelID)
		}
	}
}

// clearLoop removes sessionID's registry entry, but only if it still points
// at loop. A newer OnIngested call may already have replaced it (see the
// "old.cancel()" branch above); that entry belongs to the newer loop and
// must not be deleted out from under it.
func (n *discordTypingNotifier) clearLoop(sessionID pgtype.UUID, loop *discordTypingLoop) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if current, ok := n.loops[sessionID]; ok && current == loop {
		delete(n.loops, sessionID)
	}
}

// postTyping POSTs {apiBase}/channels/{channelID}/typing with
// "Authorization: Bot <token>", mirroring discordAPI's request shape
// (install.go). Every failure (build, transport, non-2xx) is logged and
// swallowed — never returned, never panics — since a typing indicator must
// never block or fail message ingestion.
func (n *discordTypingNotifier) postTyping(ctx context.Context, token, channelID string) {
	reqCtx, cancel := context.WithTimeout(ctx, discordTypingPostTimeout)
	defer cancel()

	url := n.apiBase + "/channels/" + channelID + "/typing"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		n.logger.WarnContext(ctx, "discord typing: build request failed", "error", err)
		return
	}
	req.Header.Set("Authorization", "Bot "+token)

	resp, err := n.client.Do(req)
	if err != nil {
		n.logger.WarnContext(ctx, "discord typing: request failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		n.logger.WarnContext(ctx, "discord typing: non-2xx response",
			"status", resp.StatusCode, "body", strings.TrimSpace(string(body)))
	}
}
