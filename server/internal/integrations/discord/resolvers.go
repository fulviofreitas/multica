// resolvers.go — Task Master subtask 3.3: the pure routing function that
// derives, from one inbound Discord message, the chat-session binding key
// (channel_chat_id), the chat_type, and the binding config JSON. This file
// intentionally implements ONLY that seam, not the full engine.ResolverSet
// (installation/identity/dedup/session/media/audit/replier/typing — subtask
// 3.4) and not typing indicators (subtask 3.5). It touches no database.
//
// # The thread-composite question (read before touching this file)
//
// The brief this file was written against assumed Discord threads need a
// composite "parentChannelID:threadID" binding key, mirroring Telegram's
// forum-topic isolation (telegram/resolvers.go's telegramSessionRouting,
// key = "chat:thread") and Slack's channel-thread isolation (see
// channel.sql's CreateChannelChatSessionBinding comment: "Slack passes a
// stable key that, for channels, includes the thread root"). That
// assumption does NOT hold for Discord, and was carried over from
// Telegram/Slack without re-checking the premise:
//
//   - Telegram/Slack chat and message ids are scoped PER CHAT — the same
//     numeric id can exist, meaning something different, in unrelated
//     chats. Isolating a thread/topic there requires combining the parent
//     chat id with the thread/topic id, or the two collide.
//   - Discord snowflake ids are GLOBALLY unique across every channel,
//     thread, message and guild on the entire platform (a snowflake
//     encodes a timestamp + internal worker id + sequence — it is not
//     scoped to a parent). A Discord thread's channel_id can never
//     collide with any other channel's or thread's id, in this guild or
//     any other. The thread id ALONE already gives perfect session
//     isolation. inbound.go already documents this at Source.ChatID's
//     assignment: "a thread is itself a channel with its own channel_id
//     ... so this check gates threads exactly like top-level guild
//     channels, with no separate thread detection needed."
//
// So discordSessionRouting below uses msg.Source.ChatID — Discord's
// channel_id, which IS the thread's own id when the message was sent
// inside a thread — as the binding key, unchanged for every Discord
// surface (DM, guild channel, thread). No composite is built, and none is
// needed for isolation.
//
// This also sidesteps a real data-availability gap, which would have
// blocked building "parentChannelID:threadID" even if a composite were
// wanted: MESSAGE_CREATE's payload carries channel_id but never parent_id
// — the parent channel id lives on the CHANNEL object, not the message
// object (identify.go's MessageCreateEvent, mirroring Discord's own wire
// shape, has no such field). Three options were weighed:
//
//   - (a) use the thread/channel id alone as the binding key — chosen.
//     Sound because of global snowflake uniqueness above; no schema or
//     wire change needed.
//   - (b) decode more fields off the raw MESSAGE_CREATE payload hoping
//     Discord includes a parent/thread hint — rejected: Discord does not
//     send parent_id on the message object, so there is nothing extra to
//     decode here (this is not a "we didn't get around to it" gap, it is
//     a Discord API property).
//   - (c) fetch the channel object via REST (GET /channels/{id}) to learn
//     parent_id, cached to avoid refetching per message — rejected: adds
//     a network call plus a cache-invalidation surface (parent_id is
//     immutable per channel, so a cache is at least safe, but this is
//     real complexity) for zero session-isolation benefit, since (a)
//     already isolates correctly. Worth revisiting only if a future
//     feature needs the parent channel id for something other than
//     routing (e.g. "reply in the thread's parent channel" outbound
//     behavior), which is out of scope here.
//
// # Distinguishing a thread from a regular guild channel
//
// MESSAGE_CREATE carries no reliable thread indicator (no "is this a
// thread" flag, no parent_id). inbound.go already relies on exactly this
// fact: it uses only guild_id presence/absence to distinguish a DM from a
// guild message, deliberately treating a thread post and a top-level guild
// channel message the same way at that layer. This file makes the same
// choice for routing: chat_type is "group" and the binding key is
// Source.ChatID for both a thread post and a regular guild channel
// message — there is no separate thread branch. This is safe specifically
// because Discord ids are globally unique (see above); a Telegram- or
// Slack-shaped adapter could not make the same simplification.
//
// # Task Master subtask 3.4 — the full engine.ResolverSet
//
// Everything below discordSessionRouting is 3.4: the platform-specific seams
// engine.Router runs the inbound pipeline through, wired to the generic
// channel_* queries exactly as telegram/resolvers.go does (see that file's
// doc comment; this one mirrors it field-for-field, including which
// generated query backs each seam). No new query, no schema change.
//
// BindMedia is a documented no-op here: MessageCreateEvent (identify.go)
// decodes no attachment fields at all — see inbound.go's own "Deliberately
// OUT of scope" note ("MessageCreateEvent carries no attachment data yet, so
// InboundMessage.MediaRefs is always left empty here"). Since inbound.go
// never populates MediaRefs, BindMedia here is never called with anything to
// bind; it returns an empty result rather than pretending to wire the
// generic media path to a source that cannot supply refs. Wiring real
// Discord attachment decoding is future work (identify.go's
// MessageCreateEvent needs an Attachments field first).
package discord

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// originDiscordChat is the issue.origin_type label for issues created via the
// Discord /issue command. MUST match the literal in
// server/migrations/900_issue_origin_discord_chat.up.sql's widened
// issue_origin_type_check CHECK constraint exactly — see
// TestOriginDiscordChatMatchesMigration900 in resolvers_test.go, which guards
// this at the string level.
const originDiscordChat = "discord_chat"

// NewDiscordResolverSet assembles the Discord ResolverSet, mirroring
// telegram.NewTelegramResolverSet exactly. Pass a nil replier to disable
// outbound verdict notices; typing is subtask 3.5's responsibility and is
// taken here only as a pass-through parameter so this constructor does not
// need to change again once 3.5 lands.
func NewDiscordResolverSet(q *db.Queries, tx engine.TxStarter, replier engine.OutboundReplier, typing engine.TypingNotifier) engine.ResolverSet {
	return engine.ResolverSet{
		Installation: &discordInstallationResolver{q: q},
		Identity:     &discordIdentityResolver{q: q},
		Dedup:        &discordDeduper{q: q},
		Session: &discordSessionBinder{session: engine.NewChatSession(q, tx, TypeDiscord, engine.SessionTitles{
			Group:    "Discord channel",
			Direct:   "Discord direct message",
			Fallback: "Discord chat",
		})},
		Audit:      &discordAuditor{q: q},
		Replier:    replier,
		Typing:     typing,
		OriginType: originDiscordChat,
	}
}

var (
	_ engine.InstallationResolver = (*discordInstallationResolver)(nil)
	_ engine.IdentityResolver     = (*discordIdentityResolver)(nil)
	_ engine.Deduper              = (*discordDeduper)(nil)
	_ engine.SessionBinder        = (*discordSessionBinder)(nil)
	_ engine.Auditor              = (*discordAuditor)(nil)
)

// decodeDiscordRaw decodes msg.Raw into discordRawEvent, mirroring
// telegram/resolvers.go's decodeTelegramRaw (same empty/malformed-Raw error
// handling), used by the seams below that need BotID/EventType off Raw
// rather than Source.
func decodeDiscordRaw(msg channel.InboundMessage) (discordRawEvent, error) {
	if len(msg.Raw) == 0 {
		return discordRawEvent{}, errors.New("discord: inbound message Raw is empty")
	}
	var raw discordRawEvent
	if err := json.Unmarshal(msg.Raw, &raw); err != nil {
		return discordRawEvent{}, err
	}
	return raw, nil
}

func discordNullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// ---- installation routing ----

// discordInstallationResolver routes an inbound message to its installation
// by (channel_type='discord', app_id=bot_id), mirroring
// telegram.installationResolver.ResolveInstallation exactly — same query,
// same routing key shape (the bot's own id, decoded off Raw since Discord
// snowflakes never round-trip through channel.Source).
type discordInstallationResolver struct{ q *db.Queries }

func (r *discordInstallationResolver) ResolveInstallation(ctx context.Context, msg channel.InboundMessage) (engine.ResolvedInstallation, error) {
	raw, err := decodeDiscordRaw(msg)
	if err != nil {
		return engine.ResolvedInstallation{}, err
	}
	inst, err := r.q.GetChannelInstallationByAppID(ctx, db.GetChannelInstallationByAppIDParams{
		ChannelType: string(TypeDiscord),
		AppID:       raw.BotID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedInstallation{}, engine.ErrInstallationNotFound
		}
		return engine.ResolvedInstallation{}, err
	}
	return engine.ResolvedInstallation{
		ID:              inst.ID,
		WorkspaceID:     inst.WorkspaceID,
		AgentID:         inst.AgentID,
		InstallerUserID: inst.InstallerUserID,
		Active:          inst.Status == "active",
		Platform:        inst,
	}, nil
}

// ---- identity ----

// discordIdentityResolver maps the Discord author id to a Multica user via
// channel_user_binding, re-verifying workspace membership — mirrors
// telegram.identityResolver.ResolveSender exactly, including the
// ErrSenderUnbound / ErrSenderNotMember sentinel mapping the Router depends
// on (an unbound sender MUST produce engine.ErrSenderUnbound so the Router
// emits the binding prompt; do not invent a Discord-specific sentinel here).
type discordIdentityResolver struct{ q *db.Queries }

func (r *discordIdentityResolver) ResolveSender(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage) (engine.ResolvedIdentity, error) {
	binding, err := r.q.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{
		InstallationID: inst.ID,
		ChannelUserID:  msg.Source.SenderID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
		}
		return engine.ResolvedIdentity{}, err
	}
	// Binding existence no longer proves membership (no FK); re-check.
	if _, err := r.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      binding.MulticaUserID,
		WorkspaceID: inst.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, engine.ErrSenderNotMember
		}
		return engine.ResolvedIdentity{}, err
	}
	return engine.ResolvedIdentity{UserID: binding.MulticaUserID}, nil
}

// ---- dedup ----

// discordDeduper is the two-phase idempotency seam on
// channel_inbound_message_dedup keyed by (installation_id, message_id) —
// Discord's globally-unique message snowflake id needs no adapter-specific
// composite. Mirrors telegram.deduper exactly.
type discordDeduper struct{ q *db.Queries }

func (r *discordDeduper) Claim(ctx context.Context, installationID pgtype.UUID, messageID string) (pgtype.UUID, error) {
	claim, err := r.q.ClaimChannelInboundDedup(ctx, db.ClaimChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, engine.ErrDuplicate
		}
		return pgtype.UUID{}, err
	}
	return claim.ClaimToken, nil
}

func (r *discordDeduper) Mark(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.MarkChannelInboundDedupProcessed(ctx, db.MarkChannelInboundDedupProcessedParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

func (r *discordDeduper) Release(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.ReleaseChannelInboundDedup(ctx, db.ReleaseChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

// ---- session bind / append ----

// discordChatSession is the shared engine session surface used by the
// Discord adapter, mirroring telegram.chatSession — kept as a narrow
// interface so the resolver's parameter mapping is testable without a
// database.
type discordChatSession interface {
	EnsureSession(ctx context.Context, in engine.EnsureSessionInput) (pgtype.UUID, error)
	StartSession(ctx context.Context, in engine.StartSessionInput) (engine.StartSessionResult, error)
	MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error
	AppendUserMessage(ctx context.Context, in engine.AppendInput) (engine.AppendResult, error)
	BindMediaRefs(ctx context.Context, in engine.BindMediaInput) error
}

// discordSessionBinder wires discordSessionRouting's binding key/chat_type/
// config into the shared engine.ChatSession, mirroring
// telegram.sessionBinder. Discord has no forum-topic-style reply thread
// (discordSessionRouting returns no replyThread value — see that function's
// doc comment: chat_type/key never distinguish a thread from its parent at
// this layer, and the binding key alone already isolates a thread from its
// parent channel), so ThreadID is left empty on every EnsureSession/
// StartSession/AppendMessage call below, unlike Telegram's forum-topic
// replyThread wiring.
type discordSessionBinder struct{ session discordChatSession }

func (r *discordSessionBinder) EnsureSession(ctx context.Context, p engine.EnsureSessionParams) (pgtype.UUID, error) {
	bindingKey, chatType, config := discordSessionRouting(p.Message)
	return r.session.EnsureSession(ctx, engine.EnsureSessionInput{
		WorkspaceID:    p.Installation.WorkspaceID,
		AgentID:        p.Installation.AgentID,
		InstallationID: p.Installation.ID,
		Sender:         p.Sender,
		BindingKey:     bindingKey,
		BindingConfig:  config,
		ChatType:       chatType,
	})
}

func (r *discordSessionBinder) StartSession(ctx context.Context, p engine.StartSessionParams) (engine.StartSessionResult, error) {
	bindingKey, chatType, config := discordSessionRouting(p.Message)
	result, err := r.session.StartSession(ctx, engine.StartSessionInput{
		EnsureSessionInput: engine.EnsureSessionInput{
			WorkspaceID: p.Installation.WorkspaceID, AgentID: p.Installation.AgentID,
			InstallationID: p.Installation.ID, Sender: p.Creator,
			BindingKey: bindingKey, BindingConfig: config, ChatType: chatType,
		},
		Initiator: p.Sender,
		Body:      p.Message.Text, MessageID: p.Message.MessageID,
		ClaimToken: p.ClaimToken, MediaPendingSeconds: p.MediaPendingSeconds,
		PersistMessage: p.PersistMessage, HistoryBoundaryPending: p.HistoryBoundaryPending,
		BeforeCommit: p.BeforeCommit,
	})
	return engine.StartSessionResult{
		SessionID: result.SessionID, BindingID: result.BindingID,
		RouteRevision: result.RouteRevision, Append: result.Append,
	}, err
}

func (r *discordSessionBinder) MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error {
	return r.session.MarkPendingFresh(ctx, sessionID, messageID)
}

func (r *discordSessionBinder) AppendMessage(ctx context.Context, p engine.AppendParams) (engine.AppendResult, error) {
	commandText := p.Message.CommandText
	if commandText == "" {
		commandText = p.Message.Text
	}
	return r.session.AppendUserMessage(ctx, engine.AppendInput{
		SessionID:           p.SessionID,
		Sender:              p.Sender,
		InstallationID:      p.InstallationID,
		Body:                p.Message.Text,
		CommandText:         commandText,
		MessageID:           p.Message.MessageID,
		ClaimToken:          p.ClaimToken,
		MediaPendingSeconds: p.MediaPendingSeconds,
		ForceFresh:          p.Message.ForceFresh,
	})
}

// BindMedia is a documented no-op: see this file's package doc for why
// MessageCreateEvent never carries attachment data yet, so this is never
// called with anything to bind.
func (r *discordSessionBinder) BindMedia(ctx context.Context, p engine.BindMediaParams) (engine.BindMediaResult, error) {
	return engine.BindMediaResult{}, nil
}

// ---- audit ----

// discordAuditor records a dropped inbound event via the generic
// channel_inbound_audit query, mirroring telegram.auditor exactly — so drops
// (including the Router's own DropReasonNotAddressedInGroup for an
// unaddressed group message, see inbound.go's package doc) are recorded for
// Discord exactly as for every other channel.
type discordAuditor struct{ q *db.Queries }

func (r *discordAuditor) RecordDrop(ctx context.Context, instID pgtype.UUID, msg channel.InboundMessage, reason engine.DropReason) error {
	raw, _ := decodeDiscordRaw(msg) // best-effort; a decode miss still audits
	return r.q.RecordChannelInboundDrop(ctx, db.RecordChannelInboundDropParams{
		ID:               dbid.NewV7(),
		ChannelType:      string(TypeDiscord),
		EventType:        raw.EventType,
		DropReason:       string(reason),
		InstallationID:   instID,
		ChannelChatID:    discordNullText(msg.Source.ChatID),
		ChannelEventID:   discordNullText(msg.EventID),
		ChannelMessageID: discordNullText(msg.MessageID),
	})
}

// discordBindingConfig is the opaque outbound routing persisted on the chat
// binding's config column. The binding key alone (Source.ChatID) is
// sufficient for session isolation — see this file's package doc — but the
// raw ids are still kept here because:
//   - outbound (Milestone 2) needs guild_id to build some Discord REST API
//     paths (e.g. channel-in-guild scoped requests), even though sending a
//     message only needs channel_id;
//   - an operator debugging a routing question needs the raw Discord ids
//     without decoding the binding key.
//
// GuildID is empty (and omitted from the JSON) for a DM, matching Discord's
// own wire shape where guild_id is absent on a DM message. ChannelID is
// always the Discord channel_id — a regular channel id or a thread id,
// Discord makes no distinction — mirroring Source.ChatID exactly.
type discordBindingConfig struct {
	GuildID   string `json:"guild_id,omitempty"`
	ChannelID string `json:"channel_id"`
}

// discordSessionRouting derives the session-isolation binding key, chat_type,
// and binding config from one inbound Discord message. Pure function, unit-
// tested without a DB — mirrors telegram/resolvers.go's
// telegramSessionRouting. See this file's package doc for why Discord, unlike
// Telegram/Slack, never needs a composite binding key: the binding key is
// always msg.Source.ChatID, and chat_type is always msg.Source.ChatType as
// already computed by inbound.go (DM -> p2p, everything else -> group).
//
// Deterministic: depends only on its argument, no map iteration or clock
// read, so the same message always produces the same key/config bytes.
func discordSessionRouting(msg channel.InboundMessage) (bindingKey string, chatType channel.ChatType, config []byte) {
	var raw discordRawEvent
	// Best-effort decode: a missing/malformed Raw just leaves GuildID empty,
	// which is the correct value for a DM anyway and never fails the whole
	// routing derivation over an audit-only field.
	_ = json.Unmarshal(msg.Raw, &raw)

	cfg, _ := json.Marshal(discordBindingConfig{
		GuildID:   raw.GuildID,
		ChannelID: msg.Source.ChatID,
	})

	return msg.Source.ChatID, msg.Source.ChatType, cfg
}
