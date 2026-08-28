package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func noopHandler(_ context.Context, _ channel.InboundMessage) error { return nil }

func TestNewDiscordFactory_BuildsChannel(t *testing.T) {
	cfg, _ := json.Marshal(installConfig{
		AppID:             "123456789012345678",
		BotUsername:       "MulticaBot",
		BotTokenEncrypted: "cGxhaW4tYm90LXRva2Vu",
	})
	factory := newDiscordFactory(ChannelDeps{})
	built, err := factory(channel.Config{Type: TypeDiscord, Raw: cfg, Handler: noopHandler})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	dc, ok := built.(*discordChannel)
	if !ok {
		t.Fatalf("built channel is not a *discordChannel: %T", built)
	}
	if dc.appID != "123456789012345678" {
		t.Errorf("appID = %q, want app id from config", dc.appID)
	}
	if dc.botUsername != "MulticaBot" {
		t.Errorf("botUsername = %q, want MulticaBot", dc.botUsername)
	}
	if dc.botToken != "plain-bot-token" {
		t.Errorf("botToken = %q, want decrypted plaintext", dc.botToken)
	}
	if dc.Type() != TypeDiscord {
		t.Errorf("Type() = %q, want %q", dc.Type(), TypeDiscord)
	}
}

func TestNewDiscordFactory_RejectsEmptyRawConfig(t *testing.T) {
	factory := newDiscordFactory(ChannelDeps{})
	if _, err := factory(channel.Config{Type: TypeDiscord, Raw: nil, Handler: noopHandler}); err == nil {
		t.Error("expected an error for empty Raw config")
	}
}

func TestNewDiscordFactory_RejectsMalformedRawConfig(t *testing.T) {
	factory := newDiscordFactory(ChannelDeps{})
	if _, err := factory(channel.Config{Type: TypeDiscord, Raw: json.RawMessage(`{not json`), Handler: noopHandler}); err == nil {
		t.Error("expected an error for malformed Raw config")
	}
}

func TestNewDiscordFactory_RejectsMissingBotToken(t *testing.T) {
	cfg, _ := json.Marshal(installConfig{AppID: "123456789012345678"})
	factory := newDiscordFactory(ChannelDeps{})
	if _, err := factory(channel.Config{Type: TypeDiscord, Raw: cfg, Handler: noopHandler}); err == nil {
		t.Error("an installation with no bot token must fail to build")
	}
}

func TestNewDiscordFactory_RejectsMissingAppID(t *testing.T) {
	cfg, _ := json.Marshal(installConfig{BotTokenEncrypted: "cGxhaW4tYm90LXRva2Vu"})
	factory := newDiscordFactory(ChannelDeps{})
	if _, err := factory(channel.Config{Type: TypeDiscord, Raw: cfg, Handler: noopHandler}); err == nil {
		t.Error("an installation with no app_id must fail to build")
	}
}

func TestDiscordChannel_Capabilities(t *testing.T) {
	c := &discordChannel{}
	got := c.Capabilities()

	want := channel.CapText | channel.CapThreadReply | channel.CapQuoteReply |
		channel.CapAttachment | channel.CapTypingIndicator | channel.CapMessageEdit
	if got != want {
		t.Errorf("Capabilities() = %s, want %s", got, want)
	}
	if got.Has(channel.CapRichCard) {
		t.Error("Capabilities() must not declare CapRichCard in v1 (embeds degrade to markdown)")
	}
	if got.Has(channel.CapVoice) {
		t.Error("Capabilities() must not declare CapVoice")
	}
}

func TestDiscordChannel_Disconnect_NoOp(t *testing.T) {
	c := &discordChannel{}
	if err := c.Disconnect(context.Background()); err != nil {
		t.Errorf("Disconnect() = %v, want nil", err)
	}
}

// TestDiscordChannel_Connect_DialFailureReturnsError is a minimal smoke test
// that Connect is wired up end to end (dial → error path) for a channel
// built the way newDiscordFactory builds one. The full Gateway
// connect/identify/resume/reconnect contract (READY, resume, reconnect
// decisions, ctx-cancellation-is-nil, repeat-Connect-after-return, ...) is
// covered in connect_test.go against an in-process fake Gateway.
func TestDiscordChannel_Connect_DialFailureReturnsError(t *testing.T) {
	c := &discordChannel{
		botToken:    "test-token",
		logger:      slog.Default(),
		resumeCache: NewResumeCache(ResumeCacheConfig{}),
		reconnector: NewReconnector(nil),
		gatewayURL:  "ws://127.0.0.1:0", // nothing listens here
	}
	if err := c.Connect(context.Background()); err == nil {
		t.Error("Connect() = nil, want an error when the Gateway dial fails")
	}
}

func TestDiscordChannel_Send_NotImplementedYet(t *testing.T) {
	c := &discordChannel{}
	if _, err := c.Send(context.Background(), channel.OutboundMessage{}); err == nil {
		t.Error("Send() must return an error until the outbound-sending subtask lands")
	}
}

func TestRegisterDiscord_RegistersFactory(t *testing.T) {
	reg := channel.NewRegistry()
	RegisterDiscord(reg, ChannelDeps{})
	if _, ok := reg.Lookup(TypeDiscord); !ok {
		t.Fatal("RegisterDiscord did not register a factory for TypeDiscord")
	}
}
