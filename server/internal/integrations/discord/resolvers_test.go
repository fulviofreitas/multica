package discord

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
)

// rawMsg builds the InboundMessage.Raw payload the way inbound.go does, so
// these tests exercise discordSessionRouting exactly as the real pipeline
// feeds it (see inbound.go's inboundFromMessageCreate).
func rawMsg(t *testing.T, guildID string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(discordRawEvent{
		BotID:     "bot-1",
		EventType: "MESSAGE_CREATE",
		GuildID:   guildID,
	})
	if err != nil {
		t.Fatalf("marshal discordRawEvent: %v", err)
	}
	return b
}

func TestDiscordSessionRouting(t *testing.T) {
	tests := []struct {
		name           string
		msg            channel.InboundMessage
		wantKey        string
		wantChatType   channel.ChatType
		wantGuildID    string // "" means the config must omit guild_id entirely
		wantChannelID  string
		guildIDOmitted bool
	}{
		{
			name: "DM channel routes p2p keyed by the DM channel id",
			msg: channel.InboundMessage{
				Source: channel.Source{
					ChannelType: TypeDiscord,
					ChatID:      "dm-channel-111",
					ChatType:    channel.ChatTypeP2P,
				},
				Raw: rawMsg(t, ""),
			},
			wantKey:        "dm-channel-111",
			wantChatType:   channel.ChatTypeP2P,
			wantChannelID:  "dm-channel-111",
			guildIDOmitted: true,
		},
		{
			name: "guild text channel routes group keyed by the channel id",
			msg: channel.InboundMessage{
				Source: channel.Source{
					ChannelType: TypeDiscord,
					ChatID:      "channel-222",
					ChatType:    channel.ChatTypeGroup,
				},
				Raw: rawMsg(t, "guild-999"),
			},
			wantKey:       "channel-222",
			wantChatType:  channel.ChatTypeGroup,
			wantGuildID:   "guild-999",
			wantChannelID: "channel-222",
		},
		{
			name: "thread routes group keyed by the thread's own channel id",
			msg: channel.InboundMessage{
				Source: channel.Source{
					ChannelType: TypeDiscord,
					// Discord addresses a thread message with channel_id set
					// to the THREAD's own snowflake id, never the parent
					// channel's id (see MessageCreateEvent / parseMessageCreate
					// in identify.go, and this file's package doc).
					ChatID:   "thread-333",
					ChatType: channel.ChatTypeGroup,
				},
				Raw: rawMsg(t, "guild-999"),
			},
			wantKey:       "thread-333",
			wantChatType:  channel.ChatTypeGroup,
			wantGuildID:   "guild-999",
			wantChannelID: "thread-333",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, chatType, cfg := discordSessionRouting(tt.msg)

			if key != tt.wantKey {
				t.Errorf("bindingKey = %q, want %q", key, tt.wantKey)
			}
			if chatType != tt.wantChatType {
				t.Errorf("chatType = %q, want %q", chatType, tt.wantChatType)
			}

			var decoded discordBindingConfig
			if err := json.Unmarshal(cfg, &decoded); err != nil {
				t.Fatalf("config did not round-trip as JSON: %v", err)
			}
			if decoded.ChannelID != tt.wantChannelID {
				t.Errorf("config.ChannelID = %q, want %q", decoded.ChannelID, tt.wantChannelID)
			}
			if decoded.GuildID != tt.wantGuildID {
				t.Errorf("config.GuildID = %q, want %q", decoded.GuildID, tt.wantGuildID)
			}

			if tt.guildIDOmitted {
				var raw map[string]any
				if err := json.Unmarshal(cfg, &raw); err != nil {
					t.Fatalf("config did not decode as a generic map: %v", err)
				}
				if _, present := raw["guild_id"]; present {
					t.Errorf("config JSON = %s, want guild_id key omitted for a DM", cfg)
				}
			}

			// Determinism: same input, called twice, must produce the exact
			// same key and config bytes (no map-iteration or clock
			// dependence anywhere in the function).
			key2, chatType2, cfg2 := discordSessionRouting(tt.msg)
			if key2 != key || chatType2 != chatType || string(cfg2) != string(cfg) {
				t.Errorf("discordSessionRouting is not deterministic: got (%q,%q,%s) then (%q,%q,%s)",
					key, chatType, cfg, key2, chatType2, cfg2)
			}
		})
	}
}

// TestDiscordSessionRouting_ThreadVsParentChannelIsolation is the whole point
// of session isolation: a thread and its parent guild channel must never
// collapse onto the same binding key, even though both are "group" chat_type
// and both carry the same guild_id. See this file's package doc for why the
// key is safe as the bare channel/thread id on Discord (globally unique
// snowflakes) without a Telegram/Slack-style composite key.
func TestDiscordSessionRouting_ThreadVsParentChannelIsolation(t *testing.T) {
	parentMsg := channel.InboundMessage{
		Source: channel.Source{
			ChannelType: TypeDiscord,
			ChatID:      "parent-channel-1",
			ChatType:    channel.ChatTypeGroup,
		},
		Raw: rawMsg(t, "guild-1"),
	}
	threadMsg := channel.InboundMessage{
		Source: channel.Source{
			ChannelType: TypeDiscord,
			ChatID:      "thread-under-parent-1",
			ChatType:    channel.ChatTypeGroup,
		},
		Raw: rawMsg(t, "guild-1"),
	}

	parentKey, _, _ := discordSessionRouting(parentMsg)
	threadKey, _, _ := discordSessionRouting(threadMsg)

	if parentKey == threadKey {
		t.Fatalf("thread and parent channel produced the same binding key %q; session isolation is broken", parentKey)
	}
}

// TestDiscordSessionRouting_MalformedRawDoesNotPanic covers a decode miss on
// Raw (e.g. a test-constructed InboundMessage with no Raw set at all, which
// json.Unmarshal on an empty/nil slice rejects). Routing must still resolve
// using Source alone.
func TestDiscordSessionRouting_MalformedRawDoesNotPanic(t *testing.T) {
	msg := channel.InboundMessage{
		Source: channel.Source{
			ChannelType: TypeDiscord,
			ChatID:      "channel-no-raw",
			ChatType:    channel.ChatTypeGroup,
		},
		// Raw intentionally left nil.
	}

	key, chatType, cfg := discordSessionRouting(msg)
	if key != "channel-no-raw" {
		t.Errorf("bindingKey = %q, want %q", key, "channel-no-raw")
	}
	if chatType != channel.ChatTypeGroup {
		t.Errorf("chatType = %q, want group", chatType)
	}
	var decoded discordBindingConfig
	if err := json.Unmarshal(cfg, &decoded); err != nil {
		t.Fatalf("config did not round-trip as JSON: %v", err)
	}
	if decoded.ChannelID != "channel-no-raw" {
		t.Errorf("config.ChannelID = %q, want %q", decoded.ChannelID, "channel-no-raw")
	}
	if decoded.GuildID != "" {
		t.Errorf("config.GuildID = %q, want empty", decoded.GuildID)
	}
}

// ---- Task Master subtask 3.4: OriginType / ResolverSet-level pure checks --

// TestOriginDiscordChatMatchesMigration900 guards the exact failure mode
// this subtask's brief calls out: a mismatch between originDiscordChat (what
// Go writes to issue.origin_type for a Discord /issue command) and migration
// 900's widened CHECK constraint would make every Discord /issue fail at
// runtime with a constraint violation, discovered only in production. This
// reads the actual migration file and asserts the literal is present in its
// SQL, rather than duplicating the string in a second Go constant that could
// drift from the migration just as easily as the original could.
func TestOriginDiscordChatMatchesMigration900(t *testing.T) {
	if originDiscordChat != "discord_chat" {
		t.Fatalf("originDiscordChat = %q, want %q", originDiscordChat, "discord_chat")
	}

	migrationPath := filepath.Join("..", "..", "..", "migrations", "900_issue_origin_discord_chat.up.sql")
	sql, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration 900: %v", err)
	}
	// Must appear quoted exactly as a CHECK ... IN (...) member, e.g.
	// 'telegram_chat', 'discord_chat')) — not merely as a substring of a
	// longer identifier.
	quoted := "'" + originDiscordChat + "'"
	if !strings.Contains(string(sql), quoted) {
		t.Fatalf("migration 900 (%s) does not contain %s; originDiscordChat and the issue_origin_type_check CHECK constraint have drifted apart:\n%s",
			migrationPath, quoted, sql)
	}
}

// TestNewDiscordResolverSetWiresOriginTypeAndPassthroughs proves the
// constructor assembles every required seam (non-nil) and passes Replier/
// Typing straight through without wrapping — mirroring the shape telegram's
// resolvers_test.go would cover if it asserted on NewTelegramResolverSet
// (it does not; this is Discord-specific because BindMedia's no-op decision
// makes "every seam is wired" worth asserting explicitly here).
func TestNewDiscordResolverSetWiresOriginTypeAndPassthroughs(t *testing.T) {
	set := NewDiscordResolverSet(nil, nil, nil, nil)

	if set.OriginType != "discord_chat" {
		t.Errorf("OriginType = %q, want %q", set.OriginType, "discord_chat")
	}
	if set.Installation == nil || set.Identity == nil || set.Dedup == nil || set.Session == nil || set.Audit == nil {
		t.Fatalf("ResolverSet has a nil required seam: %+v", set)
	}
	if set.Replier != nil {
		t.Errorf("Replier = %v, want nil pass-through for a nil constructor arg", set.Replier)
	}
	if set.Typing != nil {
		t.Errorf("Typing = %v, want nil pass-through for a nil constructor arg", set.Typing)
	}
	// Media is intentionally never wired by NewDiscordResolverSet (see this
	// package's resolvers.go doc comment on why BindMedia is a documented
	// no-op instead of a MediaResolver implementation): confirm it stays nil
	// rather than silently picking up a future implementation this test
	// would then need updating to know about.
	if set.Media != nil {
		t.Errorf("Media = %v, want nil (BindMedia no-op, see resolvers.go doc)", set.Media)
	}
}

// TestDiscordSessionBinderBindMediaIsNoOp locks in BindMedia's documented
// no-op behavior: it must return a zero result and no error regardless of
// input, so a future caller cannot mistake "not implemented" for "nothing to
// bind this time".
func TestDiscordSessionBinderBindMediaIsNoOp(t *testing.T) {
	binder := &discordSessionBinder{}
	result, err := binder.BindMedia(context.Background(), engine.BindMediaParams{})
	if err != nil {
		t.Fatalf("BindMedia error = %v, want nil", err)
	}
	if result.InitialTitle != "" || result.TitleSource != "" {
		t.Fatalf("BindMedia result = %+v, want zero value", result)
	}
}
