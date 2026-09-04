package discord

// trace.go — an opt-in, purely STRUCTURAL record of every inbound
// MESSAGE_CREATE dispatch this adapter decodes.
//
// Why it exists: a live validation run once produced seven guild
// MESSAGE_CREATE events that decoded with mentions_count=0 content_len=0 and
// were dropped downstream (engine/router.go) as not_addressed_in_group,
// intermittently, while DM handling stayed healthy. The entire evidence base
// for that investigation was an ad-hoc probe compiled into a container image
// for the occasion and reverted afterwards — so diagnosing a recurrence today
// would again require rebuilding an image rather than reading a log. Worse,
// that probe never captured the Discord message "type" field (see
// MessageCreateEvent.Type in identify.go), so nothing in the evidence it
// produced could rule out the simplest explanation: some of those seven were
// Discord SYSTEM messages (a thread-created notice, a pin notice, a
// member-join notice, ...), which structurally carry no mentions and no
// content and arrive over the exact same MESSAGE_CREATE dispatch as a real
// user message. MULTICA_DISCORD_TRACE turns that guesswork into a permanent,
// zero-rebuild answer: type, the mention array's shape, reply linkage, guild
// vs. DM context, and the relevant ids, for every decoded message, without
// needing to reproduce the problem under a special build first.
//
// Divergence from the wecom precedent (wecom/trace.go), which this file
// otherwise mirrors field-for-field (atomic.Bool switch, SetTrace, an
// operator env var, wired the same way from cmd/server/router.go): wecom's
// trace records a bounded, redacted PREVIEW of message text and its file
// comment explains why that is default-off — a live credential or message
// body reaching the log by accident. This file's default-off gate is NOT
// protecting against that risk, because there IS no such risk here: message
// content, tokens and any other credential are NEVER logged by this file,
// not even a length-bounded preview — traceInboundMessage below logs a
// content LENGTH (an int) and nothing else content-shaped. The gate exists
// purely for volume/noise control: one line per inbound message is real log
// volume at production message rates, and that is a cost worth paying only
// while an investigation is actually active, not a privacy boundary. Turn it
// on deliberately for a debugging session with MULTICA_DISCORD_TRACE=1 and
// unset it when done, the same operational habit as MULTICA_WECOM_TRACE —
// just for a different reason.
//
// Do NOT add a message-content or attachment-name field to this file. If a
// future investigation genuinely needs message text, that is a new,
// separately-reviewed decision (as wecom's was), not a quiet extension of
// this switch.

import (
	"log/slog"
	"sync/atomic"
)

// tracing is set once at boot (or by a test) and read on every decoded
// MESSAGE_CREATE dispatch.
var tracing atomic.Bool

// SetTrace turns inbound structural tracing on or off. Called from the
// server wiring with MULTICA_DISCORD_TRACE; returns what it set so the
// caller can log it, mirroring wecom.SetTrace.
func SetTrace(on bool) bool {
	tracing.Store(on)
	return on
}

// tracingOn reports whether inbound MESSAGE_CREATE dispatches are being
// recorded.
func tracingOn() bool { return tracing.Load() }

// traceInboundMessage records the structural discriminators of one decoded
// MESSAGE_CREATE dispatch. handleMessageCreate (inbound.go) is the single
// call site: it is the one place downstream of parseMessageCreate that has
// both the raw decoded event AND this adapter's own loop-prevention/
// addressing verdict for it, so this is where a future "why did this drop"
// question is answerable in one log line instead of by correlating two.
//
// passedLoopGuard is inboundFromMessageCreate's ok return value: false means
// THIS adapter already dropped the message (author is a bot, including our
// own echo) before any addressing verdict was computed. addressed is
// meaningless in that case — logged as false only because Go has no
// "not applicable" bool — which is exactly why loop_dropped is its own field
// rather than something a reader has to infer from addressed being false:
// a system message with no mentions (addressed=false, loop_dropped=false)
// and a bot's own echo (addressed=false, loop_dropped=true) must not read
// the same in the log, since only one of them is the failure mode this
// switch exists to catch.
//
// Every field here is a length, a count, an id, or a boolean. No message
// content, token, or credential is read, computed from, or passed to this
// function — see this file's package comment for why that is not merely a
// style choice.
func traceInboundMessage(log *slog.Logger, evt MessageCreateEvent, botID string, passedLoopGuard, addressed bool) {
	if !tracingOn() || log == nil {
		return
	}
	log.Info("discord trace",
		"dir", "in.msg",
		"type", evt.Type,
		"msg_id", evt.ID,
		"channel_id", evt.ChannelID,
		"guild_id", evt.GuildID,
		"is_dm", evt.GuildID == "",
		"author_id", evt.Author.ID,
		"author_bot", evt.Author.Bot,
		"mentions_count", len(evt.Mentions),
		"bot_mentioned", botInMentions(evt.Mentions, botID),
		"has_reference", evt.HasMessageReference,
		"content_len", len(evt.Content),
		"loop_dropped", !passedLoopGuard,
		"addressed", addressed,
	)
}
