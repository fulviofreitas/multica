// reconnect.go — the reconnect POLICY layer (Task Master subtask 2.5): given
// the error GatewayConn.Run or DialGateway returned, decide what the next
// attempt should do. This file deliberately does NOT own the connect loop
// (subtask 2.6 wires DialGateway/Identify/Resume/Run in a loop against this
// policy) and does NOT edit anything in gateway.go, identify.go, or
// resume.go.
//
// Two things this file exists to prevent:
//
//  1. Retrying against a FATAL close code forever. Some close codes mean the
//     installation is broken in a way no amount of reconnecting fixes
//     (revoked token, invalid/disallowed intents, bad sharding). Retrying
//     those burns IDENTIFY budget (see 2) for zero chance of success and
//     hides an operator-actionable problem inside silent background retries.
//
//  2. Exceeding Discord's 1000-IDENTIFY-per-24h-per-bot budget. Discord
//     RESETS THE BOT TOKEN when a bot exceeds it, which is a catastrophic,
//     human-in-the-loop failure (the installation is dead until someone
//     pastes a new token). The engine's supervisor
//     (internal/integrations/channel/engine/supervisor.go) retries with
//     exponential backoff capped at 60s; a permanently failing connection
//     that always needs a fresh IDENTIFY could otherwise attempt one every
//     ~60s, i.e. up to ~1440/day — over budget. RESUME does not consume this
//     budget; only a fresh IDENTIFY does. IdentifyLimiter enforces a minimum
//     90s spacing between fresh IDENTIFYs regardless of how eagerly the
//     supervisor's backoff would otherwise retry, which bounds a
//     permanently-failing installation to a steady-state rate of
//     24h/90s = 960 fresh IDENTIFYs/day (961 including the very first,
//     unspaced identify of a given window — see
//     TestIdentifyBudget_PermanentFailureStaysUnder1000PerDay)
//     — comfortably under 1000.
package discord

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// identifySpacing is the minimum time between two fresh IDENTIFYs for the
// same installation. 90s bounds a permanently failing connection to
// 24h/90s = 960 fresh IDENTIFYs/day, comfortably under Discord's
// 1000/24h budget (see this file's doc comment). RESUME attempts are not
// subject to this spacing — only a fresh IDENTIFY consumes the budget.
const identifySpacing = 90 * time.Second

// ReconnectAction is what the next reconnect attempt should do once a
// decision to retry has already been made.
type ReconnectAction int

const (
	// ActionResume means: reconnect and attempt RESUME (opcode 6) if a
	// fresh-enough ResumeCache entry exists, falling back to a plain
	// IDENTIFY only if it does not. A RESUME attempt does not consume
	// IDENTIFY budget, so decisions in this category are not subject to
	// IdentifyLimiter spacing.
	ActionResume ReconnectAction = iota
	// ActionFreshIdentify means: the previous session is no longer usable
	// (Discord said so, or the local state can no longer be trusted), so
	// the next attempt must discard any cached ResumeEntry and send a
	// fresh IDENTIFY. Subject to IdentifyLimiter spacing.
	ActionFreshIdentify
	// ActionFatal means: do not retry. The installation is broken in a way
	// reconnecting cannot fix; ReconnectDecision.OperatorMessage names the
	// cause for an operator-actionable log line.
	ActionFatal
)

func (a ReconnectAction) String() string {
	switch a {
	case ActionResume:
		return "resume"
	case ActionFreshIdentify:
		return "fresh_identify"
	case ActionFatal:
		return "fatal"
	default:
		return "unknown"
	}
}

// closeCodeInfo is one entry in the close-code classification table.
type closeCodeInfo struct {
	// name is a short human-readable label for logs, matching Discord's
	// documented close-code name.
	name string
	// action classifies what the next attempt should do.
	action ReconnectAction
	// operatorHint is set only for action == ActionFatal: an
	// operator-actionable explanation naming the cause and (where
	// applicable) the fix.
	operatorHint string
}

// discordCloseCodes maps Discord's documented Gateway close codes
// (https://discord.com/developers/docs/topics/gateway#gateway-close-event-codes)
// to a classification. Every code Discord documents in the 4000-4014 range
// is listed; 4006 is intentionally absent — Discord has never assigned it
// (skipped in their own table). Codes not in this map (including 4006 and
// anything above 4014, e.g. a future Discord addition) fall back to
// classifyCloseCode's UNKNOWN default of ActionResume: a resume attempt that
// turns out not to work costs at most one round trip (Discord answers with
// Invalid Session, which this package already handles), whereas defaulting
// an unknown code to ActionFreshIdentify would burn IDENTIFY budget on
// every occurrence, and defaulting to ActionFatal would permanently kill a
// possibly-recoverable installation on an unrecognized code.
//
//   - 4000 Unknown error                — RESUMABLE. Discord's generic
//     "something went wrong" code; nothing about it invalidates the
//     session.
//   - 4001 Unknown opcode               — RESUMABLE. We sent an opcode
//     Discord did not understand; the session itself is unaffected.
//   - 4002 Decode error                 — RESUMABLE. We sent a payload
//     Discord could not decode; the session itself is unaffected.
//   - 4003 Not authenticated            — RECONNECT_FRESH. Documented as
//     "you sent a payload prior to identifying". UNCERTAIN: Discord's docs
//     mark this reconnect-eligible but do not say whether a session exists
//     to resume. Since this can only happen before our own IDENTIFY is
//     ever sent, there is by construction no session yet to resume, so
//     fresh IDENTIFY is the only action that can succeed; classified
//     RECONNECT_FRESH rather than the safer RESUMABLE default because
//     RESUME here would just fail one avoidable round trip against a
//     session that was never created.
//   - 4004 Authentication failed        — FATAL. Bad token; retrying
//     cannot fix this and hammering IDENTIFY with a bad token is exactly
//     the kind of behavior that can lead Discord to take further action
//     against the bot.
//   - 4005 Already authenticated        — RESUMABLE. UNCERTAIN: documented
//     as "you sent more than one identify payload"; this indicates a bug
//     in our own client rather than a real session problem, and the
//     existing session (the first IDENTIFY) should still be usable, so
//     classified RESUMABLE rather than FRESH. Flagging because Discord's
//     docs do not state whether the session survives this close.
//   - 4007 Invalid seq                  — RECONNECT_FRESH. The sequence
//     number we resumed/heartbeated with was invalid; the session state
//     this client held is no longer trustworthy.
//   - 4008 Rate limited                 — RESUMABLE. We are being rate
//     limited for sending too many gateway commands; the session itself
//     is unaffected.
//   - 4009 Session timed out            — RESUMABLE. Documented as "your
//     session timed out, reconnect and start a new one" — but Discord
//     itself is the final arbiter of whether RESUME still works (it
//     answers a stale RESUME with Invalid Session, which this package
//     already handles as ActionFreshIdentify via ReasonInvalidSession); a
//     RESUME attempt costs one round trip at worst, which is cheaper than
//     unconditionally spending IDENTIFY budget on every 4009.
//   - 4010 Invalid shard                — FATAL. Sharding configuration
//     mismatch; requires a code/config change, not a reconnect.
//   - 4011 Sharding required            — FATAL. This bot has grown large
//     enough that Discord requires sharding; requires a code/config
//     change, not a reconnect.
//   - 4012 Invalid API version           — FATAL. UNCERTAIN: not called out
//     explicitly in this subtask's brief, but Discord's docs mark it
//     reconnect:false, same as 4010/4011/4013/4014 — a hardcoded gateway
//     API version mismatch needs a code change, not a retry.
//   - 4013 Invalid intent(s)             — FATAL. The requested intent bits
//     are structurally invalid.
//   - 4014 Disallowed intent(s)          — FATAL. The requested intents
//     are not enabled for this application in the Discord developer
//     portal.
var discordCloseCodes = map[int]closeCodeInfo{
	4000: {name: "unknown_error", action: ActionResume},
	4001: {name: "unknown_opcode", action: ActionResume},
	4002: {name: "decode_error", action: ActionResume},
	4003: {name: "not_authenticated", action: ActionFreshIdentify},
	4004: {name: "authentication_failed", action: ActionFatal,
		operatorHint: "authentication failed (close code 4004) — the bot token is invalid or was revoked; a human must paste a new token"},
	4005: {name: "already_authenticated", action: ActionResume},
	4007: {name: "invalid_seq", action: ActionFreshIdentify},
	4008: {name: "rate_limited", action: ActionResume},
	4009: {name: "session_timed_out", action: ActionResume},
	4010: {name: "invalid_shard", action: ActionFatal,
		operatorHint: "invalid shard (close code 4010) — sharding configuration does not match Discord's expectations; check the shard config"},
	4011: {name: "sharding_required", action: ActionFatal,
		operatorHint: "sharding required (close code 4011) — this bot has grown too large for a single shard; sharding support is required"},
	4012: {name: "invalid_api_version", action: ActionFatal,
		operatorHint: "invalid API version (close code 4012) — the gateway version this client requests is no longer supported; upgrade the client"},
	4013: {name: "invalid_intents", action: ActionFatal,
		operatorHint: "invalid intents (close code 4013) — the requested intent bits are malformed; check the intent calculation"},
	4014: {name: "disallowed_intents", action: ActionFatal,
		operatorHint: "disallowed intents (close code 4014) — a requested privileged intent is not enabled for this application in the Discord developer portal"},
}

// classifyCloseCode looks up code in discordCloseCodes, defaulting any
// unmapped code (including the unassigned 4006, and any code Discord adds
// after this table was written) to ActionResume. See discordCloseCodes'
// doc comment for why ActionResume is the safe default.
func classifyCloseCode(code int) closeCodeInfo {
	if info, ok := discordCloseCodes[code]; ok {
		return info
	}
	return closeCodeInfo{name: fmt.Sprintf("unmapped_close_code_%d", code), action: ActionResume}
}

// discordCloseCode extracts a Discord Gateway close code from err, if err
// is (or wraps) a *websocket.CloseError carrying one. gateway.go's Run
// returns the raw *websocket.CloseError unwrapped as GatewayError.Err for
// any close frame it does not already recognize as a normal/going-away
// closure (see Run's ReadMessage error handling), so this is where
// Discord's app-specific 4000-4014 codes surface.
func discordCloseCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		return 0, false
	}
	if closeErr.Code < 4000 || closeErr.Code > 4999 {
		return 0, false
	}
	return closeErr.Code, true
}

// classifyGatewayError classifies a *GatewayError returned by
// GatewayConn.Run or DialGateway. A Discord app-specific close code found
// on gerr.Err always takes priority over the coarser DisconnectReason,
// since the close code is the more specific signal; DisconnectReason is
// the fallback for outcomes that never carry a Discord close code at all
// (a zombie connection, an opcode 7 Reconnect, opcode 9 Invalid Session, or
// a generic transport error).
func classifyGatewayError(gerr *GatewayError) closeCodeInfo {
	if code, ok := discordCloseCode(gerr.Err); ok {
		return classifyCloseCode(code)
	}
	switch gerr.Reason {
	case ReasonInvalidSession:
		// GatewayError.Resumable carries Discord's own opcode-9 payload
		// bool: true means Discord itself says RESUME may work, false
		// means it must be a fresh IDENTIFY.
		if gerr.Resumable {
			return closeCodeInfo{name: "invalid_session_resumable", action: ActionResume}
		}
		return closeCodeInfo{name: "invalid_session_not_resumable", action: ActionFreshIdentify}
	case ReasonZombieConnection:
		// A missed HEARTBEAT_ACK says nothing about session validity; the
		// session is very likely still resumable.
		return closeCodeInfo{name: "zombie_connection", action: ActionResume}
	case ReasonReconnectRequested:
		// Opcode 7: Discord explicitly says the client "MAY" resume.
		return closeCodeInfo{name: "reconnect_requested", action: ActionResume}
	case ReasonServerClosed:
		// A normal/going-away WebSocket close, not a Discord app-specific
		// close code (those are handled by discordCloseCode above).
		return closeCodeInfo{name: "server_closed", action: ActionResume}
	case ReasonReadError:
		// A transport-level failure (network error, corrupt frame) that
		// is not a Discord app-specific close code. Nothing here says the
		// session is invalid.
		return closeCodeInfo{name: "read_error", action: ActionResume}
	default:
		return closeCodeInfo{name: gerr.Reason.String(), action: ActionResume}
	}
}

// classify turns any error DialGateway or GatewayConn.Run can return into a
// closeCodeInfo. Errors that are not a *GatewayError (e.g. DialGateway's own
// dial/handshake/HELLO failures, which happen before any session exists)
// default to ActionResume: there is no session to lose and no close code to
// interpret, so the safe move is to retry the same way a resumable
// disconnect would (RESUME if a cache entry is fresh enough, otherwise a
// plain IDENTIFY that does NOT count against the "must invalidate session"
// path — ResumeCache.Load already reports absent/stale correctly on its
// own).
func classify(err error) closeCodeInfo {
	var gerr *GatewayError
	if errors.As(err, &gerr) {
		return classifyGatewayError(gerr)
	}
	return closeCodeInfo{name: "transport_error", action: ActionResume}
}

// IdentifyLimiter enforces identifySpacing between fresh IDENTIFYs for each
// installation, independently of the engine supervisor's own reconnect
// backoff (server/internal/integrations/channel/engine/supervisor.go),
// which is not aware of the IDENTIFY budget at all. Safe for concurrent use
// by multiple installation goroutines; one installation's reservations
// never affect another's.
type IdentifyLimiter struct {
	mu   sync.Mutex
	next map[string]time.Time
	now  func() time.Time
}

// NewIdentifyLimiter constructs an IdentifyLimiter. now is overridable for
// deterministic tests (mirrors GatewayConfig.Now / ResumeCacheConfig.Now);
// nil defaults to time.Now.
func NewIdentifyLimiter(now func() time.Time) *IdentifyLimiter {
	if now == nil {
		now = time.Now
	}
	return &IdentifyLimiter{
		next: make(map[string]time.Time),
		now:  now,
	}
}

// Reserve reports how long the caller must wait before id's next fresh
// IDENTIFY, and atomically reserves that slot (and the next one, spaced
// identifySpacing after it) so a burst of concurrent/rapid calls for the
// same id cannot all observe wait==0. It does NOT sleep — callers own where
// and how they wait (so they can select on ctx.Done() instead of blocking
// uninterruptibly), and it does NOT call time.Now() directly, using the
// injected clock instead so tests can simulate arbitrary elapsed time.
//
// Every call reserves a slot regardless of whether the caller ends up
// actually sending an IDENTIFY afterward; callers (namely Reconnector.Decide)
// must only call Reserve when the decision already is ActionFreshIdentify.
func (l *IdentifyLimiter) Reserve(id pgtype.UUID) time.Duration {
	key := util.UUIDToString(id)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	slot := now
	if earliest, ok := l.next[key]; ok && earliest.After(now) {
		slot = earliest
	}
	l.next[key] = slot.Add(identifySpacing)
	return slot.Sub(now)
}

// ReconnectDecision is what Reconnector.Decide returns: whether the next
// attempt should happen at all, what it should do if so, how long to wait
// first, and (for logging, always) a short name for the cause.
type ReconnectDecision struct {
	// Retry is false only for ActionFatal outcomes.
	Retry bool
	// Action is meaningful only when Retry is true.
	Action ReconnectAction
	// Wait is the minimum delay before the next attempt, as required by
	// IDENTIFY rate spacing (only non-zero when Action == ActionFreshIdentify).
	// It is a FLOOR, not a substitute for the engine supervisor's own
	// exponential backoff: callers should wait max(supervisorBackoff, Wait)
	// before the next attempt, since this value only accounts for the
	// IDENTIFY budget, not general reconnect pacing.
	Wait time.Duration
	// CloseCodeName is a short label for the cause, suitable for a log
	// line's structured field, regardless of Retry.
	CloseCodeName string
	// OperatorMessage is set only when Retry is false: an
	// operator-actionable explanation of why this installation was not
	// retried.
	OperatorMessage string
}

// Reconnector is the single decision entry point subtask 2.6 calls after
// every failed GatewayConn.Run / DialGateway attempt. Construct one per
// process (or per test) with NewReconnector; it is safe for concurrent use
// by multiple installation goroutines.
type Reconnector struct {
	limiter *IdentifyLimiter
}

// NewReconnector constructs a Reconnector with its own IdentifyLimiter. now
// is overridable for deterministic tests; nil defaults to time.Now.
func NewReconnector(now func() time.Time) *Reconnector {
	return &Reconnector{limiter: NewIdentifyLimiter(now)}
}

// Decide classifies err (as returned by DialGateway or GatewayConn.Run for
// installation id) and returns what the next attempt should do. err must be
// non-nil: Run returning nil means ctx was cancelled, a clean shutdown that
// callers must not route through Decide (there is nothing to retry or log
// as a failure).
func (r *Reconnector) Decide(id pgtype.UUID, err error) ReconnectDecision {
	info := classify(err)

	switch info.action {
	case ActionFatal:
		return ReconnectDecision{
			Retry:           false,
			Action:          ActionFatal,
			CloseCodeName:   info.name,
			OperatorMessage: info.operatorHint,
		}
	case ActionFreshIdentify:
		return ReconnectDecision{
			Retry:         true,
			Action:        ActionFreshIdentify,
			Wait:          r.limiter.Reserve(id),
			CloseCodeName: info.name,
		}
	default: // ActionResume
		return ReconnectDecision{
			Retry:         true,
			Action:        ActionResume,
			CloseCodeName: info.name,
		}
	}
}
