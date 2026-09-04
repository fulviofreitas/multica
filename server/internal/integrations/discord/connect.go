// connect.go — Task Master subtask 2.6, the capstone that wires the Gateway
// transport (gateway.go), IDENTIFY (identify.go), RESUME (resume.go) and
// reconnect policy (reconnect.go) into discordChannel.Connect so the adapter
// satisfies channel.Channel's contract (see channel/channel.go:34-76).
//
// Loop-vs-return decision — Connect does ONE dial+run attempt per
// invocation and returns; it does NOT loop over reconnects internally. The
// engine's supervisor (internal/integrations/channel/engine/supervisor.go,
// see supervise()) already owns the reconnect loop for every channel: it
// re-acquires the lease, calls Connect once, and on a non-nil return applies
// its own exponential backoff (MinBackoff..MaxBackoff, jittered, reset after
// ResetBackoffAfter uptime) before calling Connect again. Two problems would
// follow from looping inside Connect instead:
//
//   - Double/absent backoff: supervise()'s backoff only fires between two
//     Connect calls. An internal loop that redials without ever returning
//     would bypass it entirely — an ActionResume decision carries Wait==0
//     by design (see reconnect.go), so a broken network would hot-loop
//     dial attempts with no pacing at all until this function decided on
//     its own backoff schedule, duplicating (and almost certainly getting
//     wrong) logic supervise() already has.
//   - Delayed operator visibility and uptime accounting: supervise() logs
//     "connection exited with error"/"exited cleanly" and measures uptime
//     once per Connect call. An internal loop would hide every failed
//     attempt but the last inside a single opaque Connect call.
//
// So this file's Connect does exactly one dial → (IDENTIFY | RESUME) → Run
// cycle, consults Reconnector.Decide only for the "why did Run end" outcome,
// and returns. reconnect.go's ReconnectDecision.Wait is a FLOOR imposed by
// the IDENTIFY budget (see reconnect.go's package doc), not a substitute for
// supervise()'s backoff: Connect blocks for Wait (interruptibly) BEFORE
// returning the error, so the two compose additively — total elapsed time
// before the next attempt is Wait (paid here, inside this call) plus
// supervise()'s own jittered backoff (paid there, after this call returns) —
// rather than fighting each other or requiring supervise() to know anything
// about the IDENTIFY budget.
package discord

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// defaultGatewayURL is the real Discord Gateway WebSocket endpoint, dialed
// when no fresh ResumeCache entry exists for this installation. v10 is
// Discord's current documented Gateway API version; encoding=json matches
// this package's JSON (not ETF) frame handling throughout gateway.go/
// identify.go/resume.go.
const defaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

// connect is discordChannel.Connect's implementation, split into its own
// file/method per this subtask's scope note in discord_channel.go. See this
// file's package doc comment for the loop-vs-return design decision.
func (c *discordChannel) connect(ctx context.Context) error {
	if ctx.Err() != nil {
		// A caller that hands Connect an already-cancelled ctx (e.g. a
		// lease lost between acquisition and the Connect call) gets the
		// same "clean shutdown" outcome as one cancelled mid-flight —
		// there is nothing to dial for.
		return nil
	}

	idStr := util.UUIDToString(c.installationID)
	log := c.logger

	gc, initialEntry, err := c.dial(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// DialGateway's HELLO read (readHello in gateway.go) returns
			// ctx.Err() — a plain context.Canceled, NOT wrapped in
			// *GatewayError — when ctx is cancelled while waiting for
			// HELLO. Without this check that would be reported to the
			// supervisor as a failed attempt (wrong: it is a graceful
			// shutdown), triggering spurious backoff and a misleading log
			// line. Same reasoning applies to a ctx cancellation racing
			// the initial TCP/TLS dial itself.
			return nil
		}
		log.Warn("discord gateway: connect failed", "installation_id", idStr, "err", err.Error())
		return err
	}
	defer gc.Close()

	// sessionEntry tracks the ResumeEntry this connection is currently
	// responsible for: the entry it dialed RESUME against (if any), then
	// whatever the most recent READY on THIS connection stored. Only this
	// connection's own Clear call may evict it (ResumeCache.Clear's pointer
	// identity guard protects a successor that has already Stored a fresher
	// entry — see resume.go).
	sessionEntry := initialEntry

	dispatch := NewDispatchFunc(
		func(evt DispatchEvent) {
			switch evt.Kind {
			case EventReady:
				seq, _ := gc.Sequence()
				sessionEntry = c.resumeCache.Store(c.installationID, evt.Ready.SessionID, evt.Ready.ResumeGatewayURL, seq)
				log.Info("discord gateway: session ready",
					"installation_id", idStr,
					"bot_id", evt.Ready.User.ID,
					"bot_username", evt.Ready.User.Username,
				)
			case EventMessageCreate:
				// Seam for Task Master task 3.1 (inbound normalization:
				// mention stripping, thread/DM classification, attachment
				// handling, mapping onto channel.InboundMessage). This
				// subtask deliberately does not implement that mapping;
				// onMessageCreate is nil-safe so a caller that hasn't wired
				// it yet simply drops MESSAGE_CREATE events rather than
				// delivering them to c.handler un-normalized.
				if c.onMessageCreate != nil {
					c.onMessageCreate(ctx, *evt.MessageCreate)
				}
			case EventUnhandled:
				if evt.EventName == "RESUMED" {
					// Discord's RESUMED dispatch is the direct confirmation
					// that THIS connection's opcode 6 RESUME (resume.go)
					// succeeded: session continuity was preserved instead of
					// falling back to a fresh IDENTIFY, which instead
					// produces the EventReady branch's "session ready" line
					// above. Before this line, a successful RESUME was
					// completely unobservable — the only prior signal was
					// the absence of a "disconnected, will attempt resume"
					// follow-up, which proves nothing (see this package's
					// remediation notes: two live disconnects were each
					// followed by "session ready", which only ever fires on
					// a fresh IDENTIFY, so it was impossible to tell from
					// logs alone whether RESUME was even attempted).
					// "RESUMED" is not decoded into its own DispatchEventKind
					// by identify.go (out of this task's file scope), so it
					// is recognized here by its raw event name instead.
					log.Info("discord gateway: session resumed",
						"installation_id", idStr,
					)
				}
				// Otherwise expected and frequent (Discord sends many event
				// types this integration does not act on yet); never an
				// error.
			}
		},
		func(eventName string, decodeErr error) {
			log.Warn("discord gateway: dispatch decode failed",
				"installation_id", idStr,
				"event", eventName,
				"err", decodeErr.Error(),
			)
		},
	)

	runErr := gc.Run(ctx, dispatch)
	if runErr == nil {
		// Run already translates ctx cancellation (including the watchdog
		// closing the socket to unblock a parked read) into nil — see
		// gateway.go's Run doc comment. Nothing to decide: this is a clean
		// shutdown, not a failed attempt, so it must never reach
		// Reconnector.Decide (Decide's own doc comment requires a non-nil
		// err for exactly this reason).
		return nil
	}
	if ctx.Err() != nil {
		// Belt-and-suspenders: Run is documented to return nil on ctx
		// cancellation, but a caller must never surface a non-nil error as
		// a failed attempt once shutdown/lease-loss has already been
		// requested, regardless of which error narrowly "won" the race.
		return nil
	}

	decision := c.reconnector.Decide(c.installationID, runErr)
	switch {
	case !decision.Retry:
		// ActionFatal: log at Error so an operator sees it, and return the
		// error unretried — no internal loop, no wait. The supervisor still
		// re-invokes Connect after its own backoff (this package has no way
		// to tell the supervisor "give up on this installation forever"),
		// but this attempt does not compound the failure by hammering
		// IDENTIFY or RESUME against a structurally broken installation.
		log.Error("discord gateway: fatal, not retrying",
			"installation_id", idStr,
			"close_code", decision.CloseCodeName,
			"operator_message", decision.OperatorMessage,
			"err", runErr.Error(),
		)
	case decision.Action == ActionFreshIdentify:
		// The previous session is no longer usable: discard it so the next
		// attempt (this call's caller, or the supervisor's next Connect)
		// identifies fresh instead of retrying a doomed RESUME.
		if sessionEntry != nil {
			c.resumeCache.Clear(c.installationID, sessionEntry)
		}
		if initialEntry != nil {
			// dial() only returns a non-nil initialEntry when THIS
			// connection attempted RESUME (see dial's doc comment), so
			// landing here means that attempt was rejected — the answer to
			// the other half of the previously-unobservable "does resume
			// ever actually work?" question (see the RESUMED branch above).
			// A generic "disconnected, fresh identify required" line would
			// not distinguish this from a fresh-IDENTIFY session going bad
			// on its own (e.g. an invalid_seq close with no resume attempt
			// in this cycle at all).
			log.Warn("discord gateway: resume rejected, fresh identify required",
				"installation_id", idStr,
				"close_code", decision.CloseCodeName,
				"err", runErr.Error(),
			)
		} else {
			log.Warn("discord gateway: disconnected, fresh identify required",
				"installation_id", idStr,
				"close_code", decision.CloseCodeName,
				"err", runErr.Error(),
			)
		}
	default: // ActionResume
		// Leave the cache intact: the next attempt should retry RESUME
		// against the same session.
		log.Warn("discord gateway: disconnected, will attempt resume",
			"installation_id", idStr,
			"close_code", decision.CloseCodeName,
			"err", runErr.Error(),
		)
	}

	if decision.Wait > 0 && waitForCtxOrTimer(ctx, decision.Wait) {
		// ctx was cancelled while paying the IDENTIFY-spacing floor: report
		// the clean shutdown, not the connection failure that preceded it.
		return nil
	}

	return runErr
}

// dial opens the Gateway transport and sends whichever of RESUME/IDENTIFY
// is appropriate: RESUME (against entry.ResumeGatewayURL, per READY's
// documented contract that it may differ from the original Gateway URL)
// when ResumeCache holds a fresh entry for this installation, otherwise a
// fresh IDENTIFY against the default Gateway URL. Returns the entry it
// attempted RESUME with (nil for a fresh IDENTIFY) so the caller can Clear
// it if the attempt turns out to need a fresh IDENTIFY next time.
func (c *discordChannel) dial(ctx context.Context) (*GatewayConn, *ResumeEntry, error) {
	cfg := c.gatewayCfg
	if cfg.Logger == nil {
		cfg.Logger = c.logger
	}

	if entry, ok := c.resumeCache.Load(c.installationID); ok {
		cfg.URL = entry.ResumeGatewayURL
		gc, err := DialGateway(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		if err := gc.Resume(c.botToken, entry); err != nil {
			_ = gc.Close()
			return nil, nil, err
		}
		return gc, entry, nil
	}

	cfg.URL = c.gatewayURL
	if cfg.URL == "" {
		cfg.URL = defaultGatewayURL
	}
	gc, err := DialGateway(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := gc.Identify(c.botToken); err != nil {
		_ = gc.Close()
		return nil, nil, err
	}
	return gc, nil, nil
}

// waitForCtxOrTimer blocks for d or until ctx is cancelled, whichever comes
// first, returning true iff ctx won the race. Mirrors engine/supervisor.go's
// sleep helper: never a bare time.Sleep, which would ignore lease loss /
// shutdown and hold this goroutine past the point the supervisor has already
// decided to tear the connection down.
func waitForCtxOrTimer(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
