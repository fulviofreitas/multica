// This file is Task Master task 7's second half: the Discord
// engine.OutboundReplier — the seam that delivers a Router VERDICT reply
// (binding prompt, offline/archived notice, /issue confirmation) back into
// Discord, mirroring telegram/replier.go's outcome switch closely. It is
// entirely separate from outbound.go's agent-chat streaming path: Reply is
// driven synchronously off the Router's ACK critical path (see
// engine.OutboundReplier's doc comment), one short message, no chunking, no
// streaming, no retry — matching every other adapter's replier.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	msgAgentOffline     = "⚠️ The agent is offline right now. Your message was received and will be handled once it's back online."
	msgAgentArchived    = "⚠️ This agent has been archived and can't respond. Please contact your workspace admin."
	msgFreshPending     = "✅ Fresh start ready. Your next chat message will run without previous context."
	msgChatStarted      = "✅ Started a new Multica chat. Your next message will enter it."
	msgIssueUsage       = "Please include an issue title. Use:\n\n/issue <title>\n[description] (optional)"
	msgIssueNotMember   = "You're not a member of this Multica workspace, so I can't file an issue for you. Ask a workspace admin to invite you, then send the command again."
	msgIssueDisabled    = "This Discord bot isn't connected to Multica (or was disconnected). Ask a workspace admin to reconnect it."
	msgBindingGroupHint = "Please message me in a direct message first, then link your Multica account."
)

// bindingMinter is the binding-token surface the replier needs to mint a
// "link your account" token. Mirrors telegram.bindingMinter's shape exactly
// so a future discord/binding.go's *BindingTokenService satisfies it without
// this file changing. No such service exists yet for Discord (Task Master
// task 7 is scoped to outbound delivery, not the binding-token flow) — the
// zero value (Binding: nil) is a supported configuration: sendBindingPrompt
// degrades to logging "binding service not configured" and every other
// outcome still replies normally, matching telegram.NewOutboundReplier's own
// documented degrade-without-Binding behavior.
type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, discordUserID string) (BindingToken, error)
}

// controlAckLedger is the ledger-write surface post (below) needs to record a
// verdict-driven acknowledgement, mirroring slack.controlAckLedger
// (slack/replier.go) exactly. *db.Queries satisfies it.
type controlAckLedger interface {
	RecordChannelOutboundMessage(ctx context.Context, arg db.RecordChannelOutboundMessageParams) error
}

// BindingToken is the minted token shape sendBindingPrompt renders into the
// redeem link. Defined here (not in a binding.go) so this file compiles and
// is fully testable before that service exists; a future
// discord.BindingTokenService.Mint can return this type directly.
type BindingToken struct {
	Raw string
}

// OutboundReplier implements engine.OutboundReplier for Discord.
type OutboundReplier struct {
	binding     bindingMinter
	decrypt     Decrypter
	appURL      string
	bindingPath string
	apiBase     string
	client      *http.Client
	logger      *slog.Logger
	ledger      controlAckLedger
}

// OutboundReplierConfig configures the replier, mirroring
// telegram.OutboundReplierConfig field-for-field.
type OutboundReplierConfig struct {
	Binding     bindingMinter
	Decrypt     Decrypter
	AppURL      string
	BindingPath string // default "/discord/bind"
	APIBase     string
	HTTPClient  *http.Client
	Logger      *slog.Logger
	// Queries mirrors slack.OutboundReplierConfig.Queries exactly: post
	// (below) records every delivered verdict-ack in the same
	// channel_outbound_message ledger the EventChatDone finalize path
	// (outbound.go) writes to. router.go's discord.NewOutboundReplier call
	// site sets this to the shared *db.Queries, the same way it wires
	// slack.NewOutboundReplier's Queries field — see
	// docs/discord-outbound-persistence-parity-decision-2026-09-04.md. Left
	// nil (e.g. in a test), control-acks simply are not ledgered, matching
	// Binding's own documented degrade-without-config behavior above.
	Queries *db.Queries
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

// NewOutboundReplier builds the replier.
func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bindingPath := cfg.BindingPath
	if bindingPath == "" {
		bindingPath = "/discord/bind"
	}
	if !strings.HasPrefix(bindingPath, "/") {
		bindingPath = "/" + bindingPath
	}
	r := &OutboundReplier{
		binding:     cfg.Binding,
		decrypt:     cfg.Decrypt,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: bindingPath,
		apiBase:     cfg.APIBase,
		client:      cfg.HTTPClient,
		logger:      logger,
	}
	// Assign only when non-nil: cfg.Queries is a concrete *db.Queries, and
	// storing a nil *db.Queries directly into the controlAckLedger interface
	// field would produce a non-nil interface wrapping a nil pointer (the
	// classic Go "typed nil" trap), which would make post's `r.ledger == nil`
	// guard below false and panic on the first ledger write attempt instead
	// of skipping it. Guarding here keeps r.ledger a true nil interface when
	// Queries is not configured, exactly like this file's bindingMinter
	// degrade above.
	if cfg.Queries != nil {
		r.ledger = cfg.Queries
	}
	return r
}

// Reply routes each outcome to its user-visible message. Errors are logged,
// not propagated: the replier runs detached from the inbound ACK path,
// mirroring telegram.OutboundReplier.Reply exactly.
func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		if err := r.sendBindingPrompt(ctx, inst, msg, res); err != nil {
			r.logger.WarnContext(ctx, "discord replier: binding prompt failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentOffline:
		if err := r.post(ctx, inst, msg, res, msgAgentOffline); err != nil {
			r.logger.WarnContext(ctx, "discord replier: offline notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentArchived:
		if err := r.post(ctx, inst, msg, res, msgAgentArchived); err != nil {
			r.logger.WarnContext(ctx, "discord replier: archived notice failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeFreshPending:
		if err := r.post(ctx, inst, msg, res, msgFreshPending); err != nil {
			r.logger.WarnContext(ctx, "discord replier: fresh-start confirmation failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeChatStarted:
		if err := r.post(ctx, inst, msg, res, msgChatStarted); err != nil {
			r.logger.WarnContext(ctx, "discord replier: new-chat confirmation failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIssueUsage:
		if err := r.post(ctx, inst, msg, res, msgIssueUsage); err != nil {
			r.logger.WarnContext(ctx, "discord replier: issue usage reply failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeIngested:
		if res.IssueID.Valid {
			text := issueCreatedText(res)
			if res.IssueDuplicate {
				text = issueDuplicateText(res)
			}
			if err := r.post(ctx, inst, msg, res, text); err != nil {
				r.logger.WarnContext(ctx, "discord replier: issue outcome reply failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	case engine.OutcomeDropped:
		if text := droppedReplyText(res, msg); text != "" {
			if err := r.post(ctx, inst, msg, res, text); err != nil {
				r.logger.WarnContext(ctx, "discord replier: drop refusal failed",
					"installation_id", util.UUIDToString(inst.ID), "error", err)
			}
		}
	}
}

func (r *OutboundReplier) sendBindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) error {
	// A guild-channel-visible bearer link can be redeemed by any other
	// member who reads it, binding the wrong Discord identity to the wrong
	// Multica user. Mirrors telegram.OutboundReplier.sendBindingPrompt: only
	// a direct-message prompt carries a redeem token.
	if msg.Source.ChatType == channel.ChatTypeGroup {
		return r.post(ctx, inst, msg, res, msgBindingGroupHint)
	}
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	if sender == "" {
		return errors.New("missing sender id")
	}
	if r.binding == nil {
		return errors.New("binding service not configured")
	}
	if r.appURL == "" {
		return errors.New("app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return fmt.Errorf("mint binding token: %w", err)
	}
	bindURL := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	text := "👋 To start chatting with me, link your Discord account to Multica:\n" + bindURL + "\n(This link expires in 15 minutes.)"
	return r.post(ctx, inst, msg, res, text)
}

// post resolves the installation's bot token from the carried platform row
// and sends plain text back into the originating channel, quote-replying to
// the triggering message when Discord supplied one. No chunking (verdict
// replies are always short, fixed strings) and no retry (best-effort,
// matching every other adapter's replier — the definitive agent reply, which
// DOES retry, is outbound.go's job).
//
// After a successful send, it records the delivered message id in the
// outbound ledger — mirroring slack.OutboundReplier.postResult
// (slack/replier.go:160-196) exactly, including its own res.ChannelBindingID
// guard and its "control_ack"/"issue_ack" kind values. Best-effort, same as
// Slack: a ledger-write failure here is logged, never returned, since the
// verdict reply has already been sent by the time this runs.
func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result, text string) error {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return errors.New("installation platform row unavailable")
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	if msg.Source.ChatID == "" {
		return errors.New("missing destination channel id")
	}
	payload := discordMessagePayload{Content: text}
	if msg.MessageID != "" {
		payload.MessageReference = &discordMessageReference{MessageID: msg.MessageID}
	}
	api := newDiscordAPI(r.apiBase, creds.BotToken, r.client)
	sent, err := api.CreateMessage(ctx, msg.Source.ChatID, payload)
	if err != nil {
		return fmt.Errorf("post discord reply: %w", err)
	}

	if r.ledger == nil || !res.ChannelBindingID.Valid || sent.ID == "" {
		return nil
	}
	kind := "control_ack"
	if res.Outcome == engine.OutcomeIngested && res.IssueID.Valid {
		kind = "issue_ack"
	}
	// NOTE (divergence from slack.postResult, and from outbound.go's own
	// persistDelivery comment above): this write is best-effort by the same
	// reasoning — the verdict reply already left api.CreateMessage
	// successfully above, so a ledger-write failure must not be surfaced as
	// a reply failure. Reply (the only caller) only logs errors it gets back
	// from post, so returning nil here (rather than an error slack.postResult
	// would return) keeps that logging path accurate: the reply DID succeed.
	if err := r.ledger.RecordChannelOutboundMessage(ctx, db.RecordChannelOutboundMessageParams{
		OutboundInstallationID: inst.ID,
		OutboundChannelType:    string(TypeDiscord),
		OutboundMessageID:      sent.ID,
		OutboundBindingID:      res.ChannelBindingID,
		OutboundRouteRevision:  res.ChannelRouteRevision,
		OutboundKind:           kind,
	}); err != nil {
		r.logger.WarnContext(ctx, "discord replier: record control acknowledgement failed",
			"installation_id", util.UUIDToString(inst.ID), "error", err)
	}
	return nil
}

func issueCreatedText(res engine.Result) string {
	id := issueResultIdentifier(res)
	title := strings.TrimSpace(res.IssueTitle)
	if title == "" {
		return "✅ Created " + id
	}
	return "✅ Created " + id + " — " + title
}

func issueDuplicateText(res engine.Result) string {
	id := issueResultIdentifier(res)
	title := strings.TrimSpace(res.IssueTitle)
	if title == "" {
		return "⚠️ Not created — active issue " + id + " already exists."
	}
	return "⚠️ Not created — active issue " + id + " already exists: " + title
}

func issueResultIdentifier(res engine.Result) string {
	if res.IssueIdentifier != "" {
		return res.IssueIdentifier
	}
	if res.IssueNumber > 0 {
		return fmt.Sprintf("#%d", res.IssueNumber)
	}
	return util.UUIDToString(res.IssueID)
}

func isAddressedIssueCommand(msg channel.InboundMessage) bool {
	if !msg.AddressedToBot {
		return false
	}
	source := msg.CommandText
	if source == "" {
		source = msg.Text
	}
	_, ok := engine.ParseIssueCommand(source)
	return ok
}

func droppedReplyText(res engine.Result, msg channel.InboundMessage) string {
	if !isAddressedIssueCommand(msg) {
		return ""
	}
	switch res.DropReason {
	case engine.DropReasonNonWorkspaceMember:
		return msgIssueNotMember
	case engine.DropReasonRevokedInstallation:
		return msgIssueDisabled
	default:
		return ""
	}
}
