package discord

// inbound_test.go exercises inboundFromMessageCreate (Task Master subtasks
// 3.1 inbound normalization and 3.2 bot-mention gating/stripping). No
// network access; every case builds a MessageCreateEvent by hand.

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

const testBotID = "999000111"

// TestInboundFromMessageCreate_Gating checks inboundFromMessageCreate's own
// responsibility: loop-prevention drops (ok=false) and the AddressedToBot
// verdict it hands to the Router. Per the coordinator's correction, this
// adapter must NOT drop an unaddressed guild/thread message itself — the
// Router (engine/router.go) owns that policy and its audit trail
// (DropReasonNotAddressedInGroup), so those cases assert ok=true with
// AddressedToBot=false rather than a drop.
func TestInboundFromMessageCreate_Gating(t *testing.T) {
	tests := []struct {
		name          string
		evt           MessageCreateEvent
		wantOK        bool
		wantAddressed bool // only checked when wantOK is true
	}{
		{
			name: "DM accepted without a mention, AddressedToBot true",
			evt: MessageCreateEvent{
				ID:        "1",
				ChannelID: "dm-chan",
				GuildID:   "", // DM: no guild_id
				Content:   "hello there",
				Author:    MessageAuthor{ID: "42", Username: "someone", Bot: false},
			},
			wantOK:        true,
			wantAddressed: true,
		},
		{
			name: "guild message without a mention is accepted but not addressed (Router drops+audits it)",
			evt: MessageCreateEvent{
				ID:        "2",
				ChannelID: "guild-chan",
				GuildID:   "guild-1",
				Content:   "hello there",
				Author:    MessageAuthor{ID: "42", Username: "someone", Bot: false},
			},
			wantOK:        true,
			wantAddressed: false,
		},
		{
			name: "guild message with a mention is accepted and addressed",
			evt: MessageCreateEvent{
				ID:        "3",
				ChannelID: "guild-chan",
				GuildID:   "guild-1",
				Content:   "<@" + testBotID + "> hello there",
				Author:    MessageAuthor{ID: "42", Username: "someone", Bot: false},
			},
			wantOK:        true,
			wantAddressed: true,
		},
		{
			name: "thread message (guild_id present, channel is a thread) accepted and addressed when mentioned",
			evt: MessageCreateEvent{
				ID:        "4",
				ChannelID: "thread-chan-abc",
				GuildID:   "guild-1",
				Content:   "<@" + testBotID + "> please help in this thread",
				Author:    MessageAuthor{ID: "42", Username: "someone", Bot: false},
			},
			wantOK:        true,
			wantAddressed: true,
		},
		{
			name: "thread message without a mention is accepted but not addressed",
			evt: MessageCreateEvent{
				ID:        "5",
				ChannelID: "thread-chan-abc",
				GuildID:   "guild-1",
				Content:   "no mention here",
				Author:    MessageAuthor{ID: "42", Username: "someone", Bot: false},
			},
			wantOK:        true,
			wantAddressed: false,
		},
		{
			name: "bot-authored message is dropped",
			evt: MessageCreateEvent{
				ID:        "6",
				ChannelID: "dm-chan",
				GuildID:   "",
				Content:   "I am some other bot",
				Author:    MessageAuthor{ID: "777", Username: "other-bot", Bot: true},
			},
			wantOK: false,
		},
		{
			name: "our own bot's message is dropped (belt-and-braces id check, Bot flag false)",
			evt: MessageCreateEvent{
				ID:        "7",
				ChannelID: "dm-chan",
				GuildID:   "",
				Content:   "echo of our own reply",
				Author:    MessageAuthor{ID: testBotID, Username: "multica-bot", Bot: false},
			},
			wantOK: false,
		},
		{
			// Loop prevention must win even when the message would otherwise
			// be addressed via repliedToBot: a bot replying to our bot's
			// message (e.g. another bot, or a misbehaving relay) is still an
			// authorship-based drop, computed before "addressed" is even
			// evaluated in inboundFromMessageCreate.
			name: "a reply authored by a bot is still dropped (loop prevention wins over addressed-ness)",
			evt: MessageCreateEvent{
				ID:               "8",
				ChannelID:        "guild-chan",
				GuildID:          "guild-1",
				Content:          "thanks",
				Author:           MessageAuthor{ID: "777", Username: "other-bot", Bot: true},
				ReplyToMessageID: "parent-7",
				ReplyToAuthorID:  testBotID,
			},
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := inboundFromMessageCreate(tc.evt, testBotID)
			if ok != tc.wantOK {
				t.Fatalf("inboundFromMessageCreate() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && msg.AddressedToBot != tc.wantAddressed {
				t.Errorf("AddressedToBot = %v, want %v", msg.AddressedToBot, tc.wantAddressed)
			}
		})
	}
}

func TestInboundFromMessageCreate_MentionStripping(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		guildID  string // non-empty so the mention gate is exercised too
		wantText string
	}{
		{
			name:     "mention at start",
			content:  "<@" + testBotID + "> hello",
			guildID:  "guild-1",
			wantText: "hello",
		},
		{
			name:     "mention in the middle",
			content:  "hello <@" + testBotID + "> there",
			guildID:  "guild-1",
			wantText: "hello  there", // internal spacing left by the removed token is not collapsed
		},
		{
			name:     "mention at the end",
			content:  "hello <@" + testBotID + ">",
			guildID:  "guild-1",
			wantText: "hello",
		},
		{
			name:     "repeated mentions",
			content:  "<@" + testBotID + "> hello <@" + testBotID + "> again",
			guildID:  "guild-1",
			wantText: "hello  again",
		},
		{
			name:     "legacy nickname form <@!id>",
			content:  "<@!" + testBotID + "> hello",
			guildID:  "guild-1",
			wantText: "hello",
		},
		{
			name:     "mention-only message becomes empty text",
			content:  "<@" + testBotID + ">",
			guildID:  "guild-1",
			wantText: "",
		},
		{
			name:     "mentions of OTHER users are left intact",
			content:  "<@" + testBotID + "> please ask <@555> to review",
			guildID:  "guild-1",
			wantText: "please ask <@555> to review",
		},
		{
			name:     "whitespace-only content after stripping becomes empty",
			content:  "<@" + testBotID + ">   \t  ",
			guildID:  "guild-1",
			wantText: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evt := MessageCreateEvent{
				ID:        "10",
				ChannelID: "guild-chan",
				GuildID:   tc.guildID,
				Content:   tc.content,
				Author:    MessageAuthor{ID: "42", Username: "someone", Bot: false},
			}
			msg, ok := inboundFromMessageCreate(evt, testBotID)
			if !ok {
				t.Fatalf("inboundFromMessageCreate() ok = false, want true")
			}
			if msg.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", msg.Text, tc.wantText)
			}
			if tc.wantText == "" && msg.Type != channel.MsgTypeUnknown {
				t.Errorf("Type = %q, want %q for empty text", msg.Type, channel.MsgTypeUnknown)
			}
			if tc.wantText != "" && msg.Type != channel.MsgTypeText {
				t.Errorf("Type = %q, want %q for non-empty text", msg.Type, channel.MsgTypeText)
			}
		})
	}
}

// TestInboundFromMessageCreate_FieldMapping checks the produced
// InboundMessage carries the expected ids/fields through from the raw
// MESSAGE_CREATE event.
func TestInboundFromMessageCreate_FieldMapping(t *testing.T) {
	evt := MessageCreateEvent{
		ID:        "msg-123",
		ChannelID: "chan-456",
		GuildID:   "guild-789",
		Content:   "<@" + testBotID + "> do the thing",
		Author:    MessageAuthor{ID: "user-42", Username: "someone", Bot: false},
	}

	msg, ok := inboundFromMessageCreate(evt, testBotID)
	if !ok {
		t.Fatalf("inboundFromMessageCreate() ok = false, want true")
	}

	if msg.EventID != "msg-123" {
		t.Errorf("EventID = %q, want %q", msg.EventID, "msg-123")
	}
	if msg.MessageID != "msg-123" {
		t.Errorf("MessageID = %q, want %q", msg.MessageID, "msg-123")
	}
	if msg.Source.ChannelType != TypeDiscord {
		t.Errorf("Source.ChannelType = %q, want %q", msg.Source.ChannelType, TypeDiscord)
	}
	if msg.Source.ChatID != "chan-456" {
		t.Errorf("Source.ChatID = %q, want %q", msg.Source.ChatID, "chan-456")
	}
	if msg.Source.ChatType != channel.ChatTypeGroup {
		t.Errorf("Source.ChatType = %q, want %q", msg.Source.ChatType, channel.ChatTypeGroup)
	}
	if msg.Source.SenderID != "user-42" {
		t.Errorf("Source.SenderID = %q, want %q", msg.Source.SenderID, "user-42")
	}
	if msg.Source.SenderStableID != "user-42" {
		t.Errorf("Source.SenderStableID = %q, want %q", msg.Source.SenderStableID, "user-42")
	}
	if !msg.AddressedToBot {
		t.Error("AddressedToBot = false, want true")
	}
	if msg.Text != "do the thing" {
		t.Errorf("Text = %q, want %q", msg.Text, "do the thing")
	}

	var raw discordRawEvent
	if err := json.Unmarshal(msg.Raw, &raw); err != nil {
		t.Fatalf("decode Raw: %v", err)
	}
	if raw.GuildID != "guild-789" {
		t.Errorf("Raw.GuildID = %q, want %q", raw.GuildID, "guild-789")
	}
	if raw.BotID != testBotID {
		t.Errorf("Raw.BotID = %q, want %q", raw.BotID, testBotID)
	}
}

// TestInboundFromMessageCreate_DMFieldMapping checks the DM (p2p) path maps
// ChatType correctly and does not require a mention.
func TestInboundFromMessageCreate_DMFieldMapping(t *testing.T) {
	evt := MessageCreateEvent{
		ID:        "msg-1",
		ChannelID: "dm-chan-1",
		GuildID:   "",
		Content:   "no mention needed in a DM",
		Author:    MessageAuthor{ID: "user-1", Username: "someone", Bot: false},
	}

	msg, ok := inboundFromMessageCreate(evt, testBotID)
	if !ok {
		t.Fatalf("inboundFromMessageCreate() ok = false, want true")
	}
	if msg.Source.ChatType != channel.ChatTypeP2P {
		t.Errorf("Source.ChatType = %q, want %q", msg.Source.ChatType, channel.ChatTypeP2P)
	}
	if !msg.AddressedToBot {
		t.Error("AddressedToBot = false, want true for a DM")
	}
	if msg.Text != "no mention needed in a DM" {
		t.Errorf("Text = %q, want unchanged content", msg.Text)
	}
}

// TestInboundFromMessageCreate_ReplyToBot exercises the repliedToBot
// disjunct of the "addressed" computation (mirrors telegram/inbound.go's
// `addressed := chatType == channel.ChatTypeP2P || mentioned ||
// repliedToBot`), and the ReplyTo field this adapter now populates from
// identify.go's decoded message_reference/referenced_message.
func TestInboundFromMessageCreate_ReplyToBot(t *testing.T) {
	tests := []struct {
		name             string
		evt              MessageCreateEvent
		wantAddressed    bool
		wantReplyMessage string // "" means msg.ReplyTo must be nil
		wantText         string
	}{
		{
			name: "guild reply to our bot, no @-mention, is addressed",
			evt: MessageCreateEvent{
				ID:               "20",
				ChannelID:        "guild-chan",
				GuildID:          "guild-1",
				Content:          "thanks for the update",
				Author:           MessageAuthor{ID: "42", Username: "someone", Bot: false},
				ReplyToMessageID: "parent-1",
				ReplyToAuthorID:  testBotID,
			},
			wantAddressed:    true,
			wantReplyMessage: "parent-1",
			wantText:         "thanks for the update",
		},
		{
			name: "guild reply to a DIFFERENT user is not addressed, but still flows through",
			evt: MessageCreateEvent{
				ID:               "21",
				ChannelID:        "guild-chan",
				GuildID:          "guild-1",
				Content:          "I agree with you",
				Author:           MessageAuthor{ID: "42", Username: "someone", Bot: false},
				ReplyToMessageID: "parent-2",
				ReplyToAuthorID:  "some-other-user",
			},
			wantAddressed:    false,
			wantReplyMessage: "parent-2",
			wantText:         "I agree with you",
		},
		{
			name: "reply whose referenced_message is absent/null is not treated as addressed",
			evt: MessageCreateEvent{
				ID:               "22",
				ChannelID:        "guild-chan",
				GuildID:          "guild-1",
				Content:          "following up",
				Author:           MessageAuthor{ID: "42", Username: "someone", Bot: false},
				ReplyToMessageID: "parent-3",
				ReplyToAuthorID:  "", // referenced_message absent/null -> unknown author
			},
			wantAddressed:    false,
			wantReplyMessage: "parent-3",
			wantText:         "following up",
		},
		{
			name: "reply to our bot that ALSO @-mentions it is addressed exactly once, mention stripped",
			evt: MessageCreateEvent{
				ID:               "23",
				ChannelID:        "guild-chan",
				GuildID:          "guild-1",
				Content:          "<@" + testBotID + "> thanks!",
				Author:           MessageAuthor{ID: "42", Username: "someone", Bot: false},
				ReplyToMessageID: "parent-4",
				ReplyToAuthorID:  testBotID,
			},
			wantAddressed:    true,
			wantReplyMessage: "parent-4",
			wantText:         "thanks!",
		},
		{
			name: "DM reply is addressed regardless of reply target (p2p is unconditionally addressed)",
			evt: MessageCreateEvent{
				ID:               "24",
				ChannelID:        "dm-chan",
				GuildID:          "",
				Content:          "got it",
				Author:           MessageAuthor{ID: "42", Username: "someone", Bot: false},
				ReplyToMessageID: "parent-5",
				ReplyToAuthorID:  "some-other-user",
			},
			wantAddressed:    true,
			wantReplyMessage: "parent-5",
			wantText:         "got it",
		},
		{
			name: "message_reference present but referenced_message missing behaves like absent (no panic, not addressed)",
			evt: MessageCreateEvent{
				ID:               "25",
				ChannelID:        "guild-chan",
				GuildID:          "guild-1",
				Content:          "hm",
				Author:           MessageAuthor{ID: "42", Username: "someone", Bot: false},
				ReplyToMessageID: "parent-6",
				ReplyToAuthorID:  "",
			},
			wantAddressed:    false,
			wantReplyMessage: "parent-6",
			wantText:         "hm",
		},
		{
			name: "no reply fields at all behaves exactly as before (backward-compat regression guard)",
			evt: MessageCreateEvent{
				ID:        "26",
				ChannelID: "guild-chan",
				GuildID:   "guild-1",
				Content:   "<@" + testBotID + "> hi",
				Author:    MessageAuthor{ID: "42", Username: "someone", Bot: false},
			},
			wantAddressed:    true,
			wantReplyMessage: "",
			wantText:         "hi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := inboundFromMessageCreate(tc.evt, testBotID)
			if !ok {
				t.Fatalf("inboundFromMessageCreate() ok = false, want true")
			}
			if msg.AddressedToBot != tc.wantAddressed {
				t.Errorf("AddressedToBot = %v, want %v", msg.AddressedToBot, tc.wantAddressed)
			}
			if tc.wantText != "" && msg.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", msg.Text, tc.wantText)
			}
			if tc.wantReplyMessage == "" {
				if msg.ReplyTo != nil {
					t.Errorf("ReplyTo = %+v, want nil", msg.ReplyTo)
				}
				return
			}
			if msg.ReplyTo == nil {
				t.Fatalf("ReplyTo = nil, want MessageID %q", tc.wantReplyMessage)
			}
			if msg.ReplyTo.MessageID != tc.wantReplyMessage {
				t.Errorf("ReplyTo.MessageID = %q, want %q", msg.ReplyTo.MessageID, tc.wantReplyMessage)
			}
		})
	}
}
