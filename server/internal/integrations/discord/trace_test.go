package discord

// trace_test.go — guards for the MULTICA_DISCORD_TRACE operator switch. Four
// things have to hold or the switch is not shippable: it records nothing at
// all when off (the default), a plain user message and a reply come out
// distinguishable from each other, a SYSTEM message (non-default "type",
// no mentions, no content) is distinguishable from an ordinary unaddressed
// message instead of looking identical to it — the exact ambiguity a prior
// investigation could not resolve — and no message content or token-shaped
// string ever reaches the log regardless of what a message's Content field
// contains.
//
// These tests do NOT call t.Parallel: `tracing` is a package-level atomic
// and every test flips it; each one restores the switch on the way out via
// withTrace.

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// withTrace sets the switch for the duration of one test and restores it,
// mirroring wecom/trace_test.go's helper of the same name and purpose.
func withTrace(t *testing.T, on bool) {
	t.Helper()
	prev := tracingOn()
	SetTrace(on)
	t.Cleanup(func() { SetTrace(prev) })
}

// capturingLogger returns a logger writing into a buffer the test can read.
// handleMessageCreate is called synchronously and only once per test here,
// so a plain bytes.Buffer (no mutex) is enough — unlike wecom's syncBuf,
// nothing in this file drives a background goroutine against it.
func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// testChannelForTrace builds the minimal discordChannel handleMessageCreate
// needs: a logger to trace into and the bot's own id (appID), used both by
// the addressing computation and by traceInboundMessage's bot_mentioned
// lookup. No handler is wired — these tests assert on the trace line, not on
// delivery to an InboundHandler.
func testChannelForTrace(log *slog.Logger, botID string) *discordChannel {
	return &discordChannel{appID: botID, logger: log}
}

const traceTestBotID = "bot-id-1"

// ---- the switch itself ----

// TestTraceOffRecordsNothing is what makes the switch safe to ship: with
// MULTICA_DISCORD_TRACE unset, a decoded message must leave no trace line at
// all, even though the logger here is at LevelDebug (matching this
// package's production default when LOG_LEVEL is unset).
func TestTraceOffRecordsNothing(t *testing.T) {
	withTrace(t, false)
	log, buf := capturingLogger()
	c := testChannelForTrace(log, traceTestBotID)

	c.handleMessageCreate(context.Background(), MessageCreateEvent{
		ID: "999", ChannelID: "chan-1", GuildID: "guild-1",
		Content: "a message that must not be traced",
		Author:  MessageAuthor{ID: "42"},
	})

	if out := buf.String(); strings.Contains(out, "discord trace") {
		t.Errorf("tracing is off but a trace line was written:\n%s", out)
	}
}

// ---- representative payloads ----

// TestTraceUserMessage covers the plain case: an ordinary guild message with
// no mentions and no reply. It doubles as the reproduction of the
// investigation's own symptom (mentions_count=0 content_len=0 in a guild)
// with every discriminator this file adds now visible alongside it: type=0
// and loop_dropped=false say this was a real, undropped user message, not a
// SYSTEM message masquerading as one — see TestTraceSystemMessage for the
// contrasting case that used to look identical.
func TestTraceUserMessage(t *testing.T) {
	withTrace(t, true)
	log, buf := capturingLogger()
	c := testChannelForTrace(log, traceTestBotID)

	c.handleMessageCreate(context.Background(), MessageCreateEvent{
		ID: "999", ChannelID: "chan-1", GuildID: "guild-1",
		Content: "hello", // 5 bytes
		Author:  MessageAuthor{ID: "42", Bot: false},
	})

	out := buf.String()
	for _, want := range []string{
		"discord trace",
		"dir=in.msg",
		"type=0",
		"msg_id=999",
		"channel_id=chan-1",
		"guild_id=guild-1",
		"is_dm=false",
		"author_id=42",
		"author_bot=false",
		"mentions_count=0",
		"bot_mentioned=false",
		"has_reference=false",
		"content_len=5",
		"loop_dropped=false",
		"addressed=false", // no mention, no reply-to-bot, not a DM
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output is missing %q; got:\n%s", want, out)
		}
	}
}

// TestTraceReplyMessage covers a reply to the bot: has_reference=true
// alongside mentions_count=0 — Discord's default ping-the-replied-to-author
// behavior adds the bot to "mentions" only when that per-message setting is
// on, so a reply carrying zero mentions is a normal, addressed shape this
// trace must not confuse with an unaddressed message.
func TestTraceReplyMessage(t *testing.T) {
	withTrace(t, true)
	log, buf := capturingLogger()
	c := testChannelForTrace(log, traceTestBotID)

	c.handleMessageCreate(context.Background(), MessageCreateEvent{
		ID: "1000", ChannelID: "chan-1", GuildID: "guild-1",
		Content:             "thanks",
		Author:              MessageAuthor{ID: "42", Bot: false},
		HasMessageReference: true,
		ReplyToMessageID:    "parent-1",
		ReplyToAuthorID:     traceTestBotID, // the reply is to OUR bot
	})

	out := buf.String()
	for _, want := range []string{
		"has_reference=true",
		"mentions_count=0",
		"content_len=6",
		"loop_dropped=false",
		"addressed=true", // repliedToBot, and content is non-empty (not the degraded case)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output is missing %q; got:\n%s", want, out)
		}
	}
}

// TestTraceSystemMessage is the case this whole file exists to make
// diagnosable: a Discord SYSTEM message (here, GUILD_MEMBER_JOIN, type 7)
// arrives over the same MESSAGE_CREATE dispatch as a user message, with no
// mentions and no content — structurally identical, in every field the prior
// ad-hoc probe captured, to an unaddressed user message. Only "type" tells
// them apart, which is exactly the field that investigation never decoded.
func TestTraceSystemMessage(t *testing.T) {
	withTrace(t, true)
	log, buf := capturingLogger()
	c := testChannelForTrace(log, traceTestBotID)

	c.handleMessageCreate(context.Background(), MessageCreateEvent{
		ID: "1001", ChannelID: "chan-1", GuildID: "guild-1",
		Type:    7, // GUILD_MEMBER_JOIN
		Content: "",
		Author:  MessageAuthor{ID: "42", Bot: false},
	})

	out := buf.String()
	for _, want := range []string{
		"type=7",
		"mentions_count=0",
		"content_len=0",
		"has_reference=false",
		"loop_dropped=false",
		"addressed=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output is missing %q; got:\n%s", want, out)
		}
	}
}

// TestTraceLoopDroppedMessage covers the adapter's own loop-prevention drop
// (author is a bot, including our own echo): passedLoopGuard is false, and
// addressed must not be read as a real verdict in that case — it is exactly
// this field, not addressed, that tells a reader the message never reached
// the addressing computation at all.
func TestTraceLoopDroppedMessage(t *testing.T) {
	withTrace(t, true)
	log, buf := capturingLogger()
	c := testChannelForTrace(log, traceTestBotID)

	c.handleMessageCreate(context.Background(), MessageCreateEvent{
		ID: "1002", ChannelID: "chan-1", GuildID: "guild-1",
		Content: "an echo of our own reply",
		Author:  MessageAuthor{ID: traceTestBotID, Bot: true},
	})

	out := buf.String()
	if !strings.Contains(out, "loop_dropped=true") {
		t.Errorf("trace output is missing %q; got:\n%s", "loop_dropped=true", out)
	}
	if !strings.Contains(out, "author_bot=true") {
		t.Errorf("trace output is missing %q; got:\n%s", "author_bot=true", out)
	}
}

// ---- the hard privacy requirement ----

// TestTraceNeverLogsContentOrToken is the guard that makes this switch safe
// to ship at all: turned on, with a message whose Content carries both a
// distinctive marker and a token-shaped string, neither may reach the log —
// only content_len (an int) is ever derived from Content.
func TestTraceNeverLogsContentOrToken(t *testing.T) {
	withTrace(t, true)
	log, buf := capturingLogger()
	c := testChannelForTrace(log, traceTestBotID)

	const distinctiveContent = "THE-QUICK-BROWN-FOX-MUST-NEVER-APPEAR-IN-THE-LOG"
	// Shaped like a real Discord bot token (three dot-separated base64-ish
	// segments) so this guards against the credential shape actually at
	// risk, not an arbitrary string. Assembled from fragments, and with a
	// first segment that decodes to "NOTA_REAL_BOT" rather than a
	// snowflake, so no contiguous token-shaped literal exists in source
	// for secret scanners to flag.
	const tokenShaped = "Tk9UQV9SRUFMX0JPVA" + "." + "G7vptL" + "." +
		"not-a-real-signature-value"
	content := distinctiveContent + " " + tokenShaped

	c.handleMessageCreate(context.Background(), MessageCreateEvent{
		ID: "1003", ChannelID: "chan-1", GuildID: "guild-1",
		Content: content,
		Author:  MessageAuthor{ID: "42", Bot: false},
	})

	out := buf.String()
	if !strings.Contains(out, "discord trace") {
		t.Fatalf("the message was not traced, so this test proves nothing:\n%s", out)
	}
	if strings.Contains(out, distinctiveContent) {
		t.Errorf("message content reached the log:\n%s", out)
	}
	if strings.Contains(out, tokenShaped) {
		t.Errorf("a token-shaped string reached the log:\n%s", out)
	}
	wantLen := "content_len=" + strconv.Itoa(len(content))
	if !strings.Contains(out, wantLen) {
		t.Errorf("trace output is missing %q (content_len must still be recorded as a length); got:\n%s", wantLen, out)
	}
}
