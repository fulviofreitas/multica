package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// discordChannel is ONE installation's Discord Gateway connection. Discord's
// Gateway is a per-application WebSocket (like Feishu's WS long-conn and
// Slack's Socket Mode), supervised per-installation by engine.Supervisor.
//
// Connect (see connect.go, Task Master subtask 2.6) wires the transport
// (gateway.go), IDENTIFY/RESUME (identify.go/resume.go) and reconnect policy
// (reconnect.go) together. Send remains a stub — the REST outbound sender is
// a later subtask (Task Master task 2, "outbound sending").
type discordChannel struct {
	appID       string
	botUsername string
	botToken    string
	handler     channel.InboundHandler
	logger      *slog.Logger

	// installationID is this discordChannel's channel_installation row id
	// (channel.Config.ID, threaded through by newDiscordFactory). It keys
	// the shared ResumeCache and Reconnector so concurrent installations
	// never share resume state or IDENTIFY budget.
	installationID pgtype.UUID

	// resumeCache and reconnector are shared across every discordChannel a
	// process runs (constructed once in newDiscordFactory's closure, see
	// ChannelDeps below) — Store/Load/Clear and the IDENTIFY budget are
	// process-wide concerns, not per-connection ones.
	resumeCache *ResumeCache
	reconnector *Reconnector

	// gatewayCfg is the DialGateway configuration template connect.go's
	// dial copies per-attempt, overriding only URL. Production leaves
	// Dialer/Now/JitterFunc zero (GatewayConfig.withDefaults fills them in
	// DialGateway); tests inject a fake Dialer / deterministic clock here.
	gatewayCfg GatewayConfig

	// gatewayURL is the default Gateway WebSocket endpoint to dial when no
	// resumable session exists. Empty defaults to defaultGatewayURL (see
	// connect.go).
	gatewayURL string

	// onMessageCreate is Task Master task 3.1's seam: connect.go invokes it
	// (if non-nil) for every decoded MESSAGE_CREATE dispatch. Nil-safe —
	// this subtask does not implement inbound normalization.
	onMessageCreate func(context.Context, MessageCreateEvent)
}

func (c *discordChannel) Type() channel.Type { return TypeDiscord }

// Capabilities declares what the Discord adapter supports. Discord embeds
// could in principle map onto CapRichCard, but v1 deliberately does NOT
// declare it: embeds degrade to plain markdown for the initial release, and
// CapRichCard is added only once embed rendering is implemented. CapVoice is
// also intentionally absent — voice channels are out of scope for the text
// adapter.
func (c *discordChannel) Capabilities() channel.Capability {
	return channel.CapText | channel.CapThreadReply | channel.CapQuoteReply |
		channel.CapAttachment | channel.CapTypingIndicator | channel.CapMessageEdit
}

// Connect dials the Gateway, IDENTIFYs (or RESUMEs, if a fresh session is
// cached) and blocks running the receive loop until the link ends. See
// connect.go for the implementation and its loop-vs-return design note.
func (c *discordChannel) Connect(ctx context.Context) error {
	return c.connect(ctx)
}

// Disconnect is a no-op: the Gateway receive loop owns its own teardown and
// returns when its run context is cancelled, mirroring
// dingtalkChannel/slackChannel/telegramChannel Disconnect.
func (c *discordChannel) Disconnect(ctx context.Context) error { return nil }

// Send is not implemented yet. The REST outbound sender is built in a later
// subtask (Task Master task 2, "outbound sending").
func (c *discordChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	return channel.SendResult{}, errors.New("discord: Send not implemented yet (Task Master task 2: outbound sending)")
}

// ChannelDeps are the shared dependencies the Discord Factory closes over.
// The engine inbound handler is supplied per-build via channel.Config.Handler.
type ChannelDeps struct {
	Decrypt Decrypter
	Logger  *slog.Logger

	// ResumeCache is the process-wide Gateway resume cache shared by every
	// installation's discordChannel (see resume.go). Nil constructs one
	// backed by time.Now — production callers normally leave this nil and
	// let newDiscordFactory build the single process-wide instance; tests
	// inject their own for a deterministic clock.
	ResumeCache *ResumeCache

	// Reconnector is the process-wide reconnect policy + IDENTIFY budget
	// limiter shared by every installation (see reconnect.go). Nil
	// constructs one backed by time.Now.
	Reconnector *Reconnector

	// GatewayURL overrides the default Gateway WebSocket endpoint to dial
	// when no resumable session exists. Empty defaults to defaultGatewayURL
	// (the real Discord Gateway) — see connect.go.
	GatewayURL string
}

// RegisterDiscord registers the per-installation Discord Factory so the
// engine.Supervisor builds + supervises one Gateway connection per active
// Discord installation. Same contract as telegram.RegisterTelegram /
// lark.RegisterFeishu / slack.RegisterSlack — no engine edit. Not wired into
// server/cmd/server/router.go by this subtask; that is a later subtask's
// responsibility.
func RegisterDiscord(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeDiscord, newDiscordFactory(deps))
}

func newDiscordFactory(deps ChannelDeps) channel.Factory {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	resumeCache := deps.ResumeCache
	if resumeCache == nil {
		resumeCache = NewResumeCache(ResumeCacheConfig{})
	}
	reconnector := deps.Reconnector
	if reconnector == nil {
		reconnector = NewReconnector(nil)
	}
	return func(cfg channel.Config) (channel.Channel, error) {
		var ic installConfig
		if err := json.Unmarshal(cfg.Raw, &ic); err != nil {
			return nil, fmt.Errorf("discord: decode installation config: %w", err)
		}
		token, err := decryptToken(ic.BotTokenEncrypted, deps.Decrypt)
		if err != nil {
			return nil, fmt.Errorf("discord: decrypt bot token: %w", err)
		}
		if token == "" {
			return nil, errors.New("discord: installation has no bot token")
		}
		if ic.AppID == "" {
			return nil, errors.New("discord: installation has no app_id")
		}
		dc := &discordChannel{
			appID:          ic.AppID,
			botUsername:    ic.BotUsername,
			botToken:       token,
			handler:        cfg.Handler,
			logger:         logger,
			installationID: cfg.ID,
			resumeCache:    resumeCache,
			reconnector:    reconnector,
			gatewayURL:     deps.GatewayURL,
		}
		// Task 3.1/3.2's wiring into connect.go's onMessageCreate seam (see
		// inbound.go's handleMessageCreate). Set after construction, not in
		// the struct literal above, because the closure needs to capture dc
		// itself.
		dc.onMessageCreate = dc.handleMessageCreate
		return dc, nil
	}
}
