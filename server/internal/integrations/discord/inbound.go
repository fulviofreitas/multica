// inbound.go — Task Master subtasks 3.1 (inbound normalization: mapping a
// decoded MESSAGE_CREATE onto channel.InboundMessage) and 3.2 (bot-mention
// detection and stripping), which are tightly coupled: the same
// mention-detection logic both sets AddressedToBot and strips the mention
// token out of the agent-visible text.
//
// Mention GATING (deciding whether an unaddressed group message is dropped)
// is NOT done here — that is the Router's job (engine/router.go), which
// drops and audits (DropReasonNotAddressedInGroup) using the AddressedToBot
// verdict this file computes. This adapter only ever drops for loop
// prevention (bot-authored / own-bot messages), never for routing policy.
//
// Deliberately OUT of scope for this file (left for later subtasks):
//   - session routing / binding keys / chat_type persistence (task 3.3) —
//     this file only guarantees InboundMessage.Source carries the raw
//     platform ids (channel id, guild id via Raw, sender id) that 3.3 needs
//     to derive its binding key; it does not look anything up.
//   - the ResolverSet / media resolution (task 3.4) — MessageCreateEvent
//     carries no attachment data yet, so InboundMessage.MediaRefs is always
//     left empty here, exactly as InboundMessage's own doc comment requires
//     of every adapter.
//   - typing indicators (task 3.5).
package discord

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util"
)

// discordMentionPattern matches a Discord user-mention token in message
// content: "<@USER_ID>" or the legacy per-guild-nickname form "<@!USER_ID>".
// Discord itself renders both forms depending on client/age of the message;
// both must be recognized as a mention of the same user id. Capture group 1
// is the mentioned user's snowflake id.
var discordMentionPattern = regexp.MustCompile(`<@!?(\d+)>`)

// discordRawEvent carries the Discord-specific fields the cross-platform
// envelope does not, read back only inside Discord-specific code (task 3.3's
// routing, primarily). Mirrors telegram/inbound.go's telegramRawEvent.
type discordRawEvent struct {
	// BotID is this installation's bot/application user id, carried so 3.3
	// can route the message to its installation without re-deriving it.
	BotID string `json:"bot_id"`
	// EventType is a coarse label for drop audits, mirroring Telegram's
	// raw event envelope.
	EventType string `json:"event_type"`
	// GuildID is the Discord guild (server) the message was sent in, empty
	// for a DM. It has no field on channel.Source (which is deliberately
	// platform-neutral), so it is carried here for any Discord-specific
	// logic (e.g. 3.3's session binding, if it needs guild-level context)
	// that reads Raw.
	GuildID string `json:"guild_id,omitempty"`
}

// botInMentions reports whether botID appears in a decoded MESSAGE_CREATE's
// "mentions" array. This is the AUTHORITATIVE addressing signal — see
// MessageCreateEvent.Mentions in identify.go for why: it is exactly the
// condition Discord itself uses to decide whether it exempts this message
// from the MESSAGE_CONTENT privileged intent, so testing it here makes "we
// consider ourselves addressed" and "Discord actually gave us content" the
// same condition rather than two that can disagree.
func botInMentions(mentions []MentionedUser, botID string) bool {
	if botID == "" {
		return false
	}
	for _, m := range mentions {
		if m.ID == botID {
			return true
		}
	}
	return false
}

// mentionsBotID reports whether content contains a mention token addressed
// to botID.
//
// This intentionally matches on the bot's numeric user id, NOT its
// username, unlike telegram/inbound.go's mentionsBot (which matches
// "@botusername" text). Two reasons this must not be copied from Telegram:
//
//   - Spoofable: Discord mention tokens are literal text
//     ("<@123456789012345678>") that any client can render from typed text
//     like "@SomeName" without actually mentioning anyone; only the id form
//     is Discord's own authoritative signal that a mention actually
//     happened.
//   - Wrong on rename: a bot's display name/username can change at any
//     time (Discord even allows per-guild nicknames); the id never does.
//     A username-text match would silently stop working — or stop working
//     only in guilds where the nickname differs — the moment the bot is
//     renamed.
func mentionsBotID(content, botID string) bool {
	if botID == "" {
		return false
	}
	for _, m := range discordMentionPattern.FindAllStringSubmatch(content, -1) {
		if m[1] == botID {
			return true
		}
	}
	return false
}

// stripBotMentions removes every mention token addressed to botID from
// content, in any position (start/middle/end, repeated) and in either the
// "<@id>" or legacy "<@!id>" form. Mentions of OTHER users are left
// completely untouched — the agent may need them for context (e.g. "ask
// @alice to review this"). Internal spacing left behind by a removed token
// is NOT collapsed (mirrors telegram/inbound.go's removeBotMentions, which
// only trims the outer result, not internal double-spaces) — callers that
// want a fully normalized body call strings.TrimSpace on the result, as
// inboundFromMessageCreate does.
func stripBotMentions(content, botID string) string {
	if botID == "" {
		return content
	}
	return discordMentionPattern.ReplaceAllStringFunc(content, func(tok string) string {
		m := discordMentionPattern.FindStringSubmatch(tok)
		if len(m) == 2 && m[1] == botID {
			return ""
		}
		return tok
	})
}

// inboundFromMessageCreate normalizes one decoded MESSAGE_CREATE event. ok
// is false only for loop prevention: the message was authored by any bot,
// including this bot's own echo. An unaddressed guild message is NOT
// dropped here — it is returned with ok=true and AddressedToBot=false so the
// Router can drop and audit it centrally (see this file's package doc).
func inboundFromMessageCreate(evt MessageCreateEvent, botID string) (channel.InboundMessage, bool) {
	// Loop prevention (critical): a bot replying to its own message is an
	// infinite loop that burns agent quota and Discord rate limit budget.
	// Author.Bot is Discord's own signal for "this author is any bot,
	// including ours"; comparing the author id against our own bot id is a
	// belt-and-braces check in case Bot is ever absent/wrong on a payload —
	// both must independently be able to drop the message.
	if evt.Author.Bot || (botID != "" && evt.Author.ID == botID) {
		return channel.InboundMessage{}, false
	}

	// A message with no guild_id is a DM; Discord never populates guild_id
	// for direct messages. This is the only signal needed to distinguish a
	// DM from a guild channel/thread message (a thread is itself a channel
	// with its own channel_id, but it still carries the parent guild's
	// guild_id — so this check gates threads exactly like top-level guild
	// channels, with no separate thread detection needed at this layer).
	isDM := evt.GuildID == ""
	// botInMentionsArray is the primary signal (see botInMentions' doc
	// comment): Discord's own "mentions" array, which is exactly what
	// Discord keys the MESSAGE_CONTENT exemption on and — unlike a content
	// regex — also catches a reply with the default ping-the-replied-to-
	// author behavior enabled, where Discord adds the bot to "mentions"
	// without ever inserting a "<@id>" token into Content. The content
	// regex remains a secondary signal: it costs nothing to also check, and
	// covers any payload where Discord omitted the mentions array (or a
	// future/legacy shape this package hasn't seen). The two are combined
	// with OR, not compared for disagreement — either one being true is
	// sufficient to consider the message addressed, and mentionsBotID
	// never needs to run once botInMentionsArray is already true (Go's ||
	// short-circuits it).
	botInMentionsArray := botInMentions(evt.Mentions, botID)
	mentioned := botInMentionsArray || mentionsBotID(evt.Content, botID)
	// repliedToBot is true only when Discord told us, authoritatively, that
	// the message being replied to was authored by our bot
	// (referenced_message.author.id). It is deliberately false — not
	// "unknown treated as true" and not resolved via a REST call — whenever
	// referenced_message is absent or null, which Discord does whenever the
	// referenced message was deleted. See MessageCreateEvent.ReplyToAuthorID
	// in identify.go for the full trade-off.
	repliedToBot := botID != "" && evt.ReplyToAuthorID != "" && evt.ReplyToAuthorID == botID

	// Mention gating is NOT enforced here. The Router already owns
	// group-mention filtering (engine/router.go: `if
	// msg.Source.ChatType == channel.ChatTypeGroup && !msg.AddressedToBot`)
	// and records DropReasonNotAddressedInGroup to channel_inbound_audit
	// when it drops — the operator-visible evidence that "the bot saw this
	// and deliberately ignored it". AddressedToBot exists on
	// channel.InboundMessage precisely so every adapter reports its verdict
	// and lets the core apply one shared drop-and-audit policy; dropping an
	// unaddressed guild message here instead would (a) silently diverge
	// from Telegram/Lark/Slack/WeCom/DingTalk, all of which report the
	// verdict and let the Router decide, and (b) make that drop invisible
	// to the audit trail. This adapter's only job is to compute the
	// verdict correctly.
	chatType := channel.ChatTypeGroup
	if isDM {
		chatType = channel.ChatTypeP2P
	}
	// degradedBotReply is the one case where repliedToBot is true but we
	// have nothing useful to hand the agent: a reply to our bot with
	// Discord's ping-the-replied-to-author behavior turned OFF. In that
	// case the bot is NOT added to Mentions (Content's other exemption
	// path), so none of the four documented MESSAGE_CONTENT exemptions
	// apply and Discord blanks Content to "". We deliberately do NOT count
	// repliedToBot as "addressed" here — Content=="" is the tell that we
	// are in the ping-off case rather than the normal repliedToBot case
	// (see TestInboundFromMessageCreate_ReplyToBot, where content is
	// always present) — so that AddressedToBot ends up false and the
	// Router drops+audits it (DropReasonNotAddressedInGroup) instead of
	// this adapter silently handing the agent a text-less "addressed"
	// message. See inbound_test.go's
	// TestInboundFromMessageCreate_ReplyToBotDegraded for the full
	// reasoning and the alternative considered.
	degradedBotReply := repliedToBot && !botInMentionsArray && !mentioned && evt.Content == ""
	// AddressedToBot: true for a DM (mirrors telegram/inbound.go's
	// `addressed := chatType == channel.ChatTypeP2P || mentioned ||
	// repliedToBot`, whose first disjunct makes every p2p message
	// unconditionally addressed — the field is documented as "meaningless
	// for direct (p2p) chats" and the core ignores it there, so any p2p
	// value would do, but matching Telegram's exact choice keeps adapters
	// consistent). For a guild message, true when it's mentioned (array or
	// content token) OR directly replies to a message the bot authored
	// (repliedToBot, computed above) — matching InboundMessage.AddressedToBot's
	// own doc ("@-mention or reply to a bot message") — EXCEPT the
	// degraded-reply case above, which is excluded so it is not silently
	// mishandled.
	addressed := isDM || mentioned || (repliedToBot && !degradedBotReply)

	text := strings.TrimSpace(stripBotMentions(evt.Content, botID))

	// A mention-only message ("@bot") or otherwise-empty content is never
	// dropped here on account of being empty — this mirrors
	// telegram/inbound.go's inboundFromUpdate, which never drops on empty
	// Text either (its analogue is a caption-less photo message): whether
	// an empty body needs a reply is the router's decision, not this
	// normalizer's.
	msgType := channel.MsgTypeText
	if text == "" {
		msgType = channel.MsgTypeUnknown
	}

	raw, _ := json.Marshal(discordRawEvent{
		BotID:     botID,
		EventType: "MESSAGE_CREATE",
		GuildID:   evt.GuildID,
	})

	// ReplyTo is populated whenever this message carries a
	// message_reference, mirroring telegram/inbound.go and
	// slack/inbound.go/lark/feishu_channel.go, which all populate ReplyTo
	// for any reply, not only a reply to the bot — the engine may use it
	// for quoting/threading regardless of who authored the parent message.
	// Discord has no separate "thread root" distinct from the immediate
	// parent at this layer (a Discord thread is itself addressed by
	// Source.ChatID), so RootID is left empty rather than duplicating
	// MessageID.
	var reply *channel.ReplyCtx
	if evt.ReplyToMessageID != "" {
		reply = &channel.ReplyCtx{MessageID: evt.ReplyToMessageID}
	}

	return channel.InboundMessage{
		// Discord snowflake message ids are globally unique (unlike
		// Telegram's message ids, which are only unique per chat and need
		// telegram/inbound.go's "chat:message" composite key) — do not
		// "fix" this into a composite id, it would be redundant.
		EventID:   evt.ID,
		MessageID: evt.ID,
		Type:      msgType,
		Text:      text,
		Source: channel.Source{
			ChannelType: TypeDiscord,
			// The Discord channel id (a regular text channel or a thread —
			// threads are channels in Discord's model) is the conversation
			// identity; no separate ThreadID is needed for that reason.
			ChatID:   evt.ChannelID,
			ChatType: chatType,
			SenderID: evt.Author.ID,
			// Discord user ids are global (not per-installation, unlike
			// e.g. a Lark open_id), so the per-installation id doubles as
			// the cross-installation stable id — mirrors Telegram's
			// SenderStableID = SenderID.
			SenderStableID: evt.Author.ID,
		},
		// The Router (engine/router.go) is the sole enforcer of group
		// mention gating; it drops+audits an unaddressed group message
		// using exactly this verdict. See the "addressed" comment above.
		AddressedToBot: addressed,
		ReplyTo:        reply,
		Raw:            raw,
	}, true
}

// handleMessageCreate is this discordChannel's wiring into connect.go's
// onMessageCreate seam (see discord_channel.go's onMessageCreate field doc
// and connect.go's EventMessageCreate case): normalize the raw event and, if
// it passes inboundFromMessageCreate's loop-prevention check, deliver it to
// the shared InboundHandler — including unaddressed group messages, which
// the Router (not this adapter) drops and audits. Nil-safe on c.handler for
// the same reason connect.go's seam is nil-safe on onMessageCreate itself —
// some tests build a discordChannel with no handler wired.
func (c *discordChannel) handleMessageCreate(ctx context.Context, evt MessageCreateEvent) {
	msg, ok := inboundFromMessageCreate(evt, c.appID)
	if !ok || c.handler == nil {
		return
	}
	if err := c.handler(ctx, msg); err != nil {
		c.logger.Error("discord: inbound handler failed",
			"installation_id", util.UUIDToString(c.installationID),
			"err", err.Error(),
		)
	}
}
