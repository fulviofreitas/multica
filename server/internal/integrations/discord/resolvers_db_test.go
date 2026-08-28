package discord

// resolvers_db_test.go covers the Postgres-backed halves of Task Master
// subtask 3.4's ResolverSet: installation resolution by app_id, sender
// identity resolution (bound + unbound + membership re-check), dedup
// claim/mark/release, EnsureSession/StartSession's binding-key/chat_type
// wiring, and RecordDrop's audit write. Everything here needs real rows and
// a real unique index (idx_channel_installation_type_appid,
// channel_user_binding's UNIQUE(installation_id, channel_user_id),
// channel_inbound_message_dedup's PK), so it is gated by discordResolversTestDB
// exactly like persist_test.go's discordPersistTestDB: SKIP without
// DATABASE_URL/Postgres (this sandbox), RUN in CI.
//
// The pure routing/config-shape assertions already covered by
// TestDiscordSessionRouting* (resolvers_test.go) and the wiring/no-op
// assertions in TestNewDiscordResolverSetWiresOriginTypeAndPassthroughs /
// TestDiscordSessionBinderBindMediaIsNoOp are NOT re-asserted here — this
// file only proves the seams that actually touch the database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func discordResolversTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	var migrated bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.channel_installation') IS NOT NULL`).Scan(&migrated); err != nil || !migrated {
		pool.Close()
		t.Skip("channel_installation not present (database not migrated)")
	}
	t.Cleanup(pool.Close)
	return pool
}

// discordResolversFixture is one workspace/user/agent/installation set,
// mirroring discordPersistFixture (persist_test.go) but adding the
// channel_installation row every resolver test needs and its own cleanup —
// resolvers_db_test.go inserts rows testutil's Fixture builders do not know
// about (channel_installation, channel_user_binding), so it removes them
// itself rather than relying on testutil's generic table cleanup.
type discordResolversFixture struct {
	f              *testutil.Fixture
	pool           *pgxpool.Pool
	workspaceID    string
	userID         string
	agentID        string
	installationID string
	appID          string
}

var discordResolversFixtureSeq int64

func discordResolversUniqueSuffix() string {
	discordResolversFixtureSeq++
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), discordResolversFixtureSeq)
}

func newDiscordResolversFixture(t *testing.T, pool *pgxpool.Pool) *discordResolversFixture {
	t.Helper()
	suffix := discordResolversUniqueSuffix()
	f := testutil.New(pool, "", "")
	f.UserID = f.User(t, "Discord Resolvers Test", "discord-resolvers-test-"+suffix+"@multica.ai")
	f.WorkspaceID = f.Workspace(t, "Discord Resolvers Test", "discord-resolvers-test-"+suffix)
	// GetMemberByUserAndWorkspace re-verification (discordIdentityResolver)
	// needs a real membership row; discordPersistFixture (persist_test.go)
	// does not need this because Persist never checks membership.
	f.Member(t, f.WorkspaceID, f.UserID, "owner")
	agentID := f.Agent(t, "discord-resolvers-agent-"+suffix, "")

	appID := "discord-app-" + suffix
	installationID := f.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id":      f.WorkspaceID,
		"agent_id":          agentID,
		"channel_type":      string(TypeDiscord),
		"config":            testutil.Raw(fmt.Sprintf(`'{"app_id":"%s"}'::jsonb`, appID)),
		"status":            "active",
		"installer_user_id": f.UserID,
	})

	return &discordResolversFixture{
		f: f, pool: pool,
		workspaceID: f.WorkspaceID, userID: f.UserID, agentID: agentID,
		installationID: installationID, appID: appID,
	}
}

// bindChannelUser inserts a channel_user_binding row mapping discordUserID to
// the fixture's Multica user, in the fixture's installation.
func (rf *discordResolversFixture) bindChannelUser(t *testing.T, discordUserID string) {
	t.Helper()
	rf.f.Insert(t, "channel_user_binding", testutil.Cols{
		"workspace_id":    rf.workspaceID,
		"multica_user_id": rf.userID,
		"installation_id": rf.installationID,
		"channel_type":    string(TypeDiscord),
		"channel_user_id": discordUserID,
	})
}

func (rf *discordResolversFixture) resolvedInstallation(t *testing.T) engine.ResolvedInstallation {
	t.Helper()
	return engine.ResolvedInstallation{
		ID:          util.MustParseUUID(rf.installationID),
		WorkspaceID: util.MustParseUUID(rf.workspaceID),
		AgentID:     util.MustParseUUID(rf.agentID),
		Active:      true,
	}
}

func discordInboundMsg(t *testing.T, botID, senderID, chatID, messageID string, chatType channel.ChatType) channel.InboundMessage {
	t.Helper()
	raw, err := json.Marshal(discordRawEvent{BotID: botID, EventType: "MESSAGE_CREATE"})
	if err != nil {
		t.Fatalf("marshal discordRawEvent: %v", err)
	}
	return channel.InboundMessage{
		EventID:   messageID,
		MessageID: messageID,
		Type:      channel.MsgTypeText,
		Text:      "hello",
		Source: channel.Source{
			ChannelType:    TypeDiscord,
			ChatID:         chatID,
			ChatType:       chatType,
			SenderID:       senderID,
			SenderStableID: senderID,
		},
		Raw: raw,
	}
}

// ---- installation resolution -----------------------------------------------

func TestDiscordResolversDB_InstallationResolvesByAppID(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	resolver := &discordInstallationResolver{q: db.New(pool)}

	msg := discordInboundMsg(t, rf.appID, "sender-1", "chat-1", "msg-1", channel.ChatTypeGroup)
	inst, err := resolver.ResolveInstallation(context.Background(), msg)
	if err != nil {
		t.Fatalf("ResolveInstallation: %v", err)
	}
	if inst.ID != util.MustParseUUID(rf.installationID) {
		t.Fatalf("installation ID = %v, want %v", inst.ID, rf.installationID)
	}
	if inst.WorkspaceID != util.MustParseUUID(rf.workspaceID) {
		t.Fatalf("installation WorkspaceID = %v, want %v", inst.WorkspaceID, rf.workspaceID)
	}
	if !inst.Active {
		t.Fatal("installation Active = false, want true")
	}
}

func TestDiscordResolversDB_InstallationNotFoundForUnknownAppID(t *testing.T) {
	pool := discordResolversTestDB(t)
	_ = newDiscordResolversFixture(t, pool) // any row must not match an unrelated app id
	resolver := &discordInstallationResolver{q: db.New(pool)}

	msg := discordInboundMsg(t, "no-such-app-id", "sender-1", "chat-1", "msg-1", channel.ChatTypeGroup)
	_, err := resolver.ResolveInstallation(context.Background(), msg)
	if !errors.Is(err, engine.ErrInstallationNotFound) {
		t.Fatalf("ResolveInstallation error = %v, want engine.ErrInstallationNotFound", err)
	}
}

// ---- identity resolution ----------------------------------------------------

func TestDiscordResolversDB_UnboundSenderReturnsErrSenderUnbound(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	resolver := &discordIdentityResolver{q: db.New(pool)}

	msg := discordInboundMsg(t, rf.appID, "unbound-discord-user", "chat-1", "msg-1", channel.ChatTypeGroup)
	_, err := resolver.ResolveSender(context.Background(), rf.resolvedInstallation(t), msg)
	if !errors.Is(err, engine.ErrSenderUnbound) {
		t.Fatalf("ResolveSender error = %v, want engine.ErrSenderUnbound", err)
	}
}

func TestDiscordResolversDB_BoundSenderResolvesAndReverifiesMembership(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	const discordUserID = "bound-discord-user"
	rf.bindChannelUser(t, discordUserID)
	resolver := &discordIdentityResolver{q: db.New(pool)}

	msg := discordInboundMsg(t, rf.appID, discordUserID, "chat-1", "msg-1", channel.ChatTypeGroup)
	identity, err := resolver.ResolveSender(context.Background(), rf.resolvedInstallation(t), msg)
	if err != nil {
		t.Fatalf("ResolveSender: %v", err)
	}
	if identity.UserID != util.MustParseUUID(rf.userID) {
		t.Fatalf("ResolveSender UserID = %v, want %v", identity.UserID, rf.userID)
	}
}

func TestDiscordResolversDB_BoundSenderRemovedFromWorkspaceReturnsErrSenderNotMember(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	const discordUserID = "bound-then-removed-discord-user"
	rf.bindChannelUser(t, discordUserID)

	// The binding survives; workspace membership does not — this is exactly
	// the case GetMemberByUserAndWorkspace's re-check exists to catch (see
	// channel_user_binding's migration 124 doc: "a row's existence no longer
	// proves current workspace membership").
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, rf.workspaceID, rf.userID); err != nil {
		t.Fatalf("remove membership: %v", err)
	}

	resolver := &discordIdentityResolver{q: db.New(pool)}
	msg := discordInboundMsg(t, rf.appID, discordUserID, "chat-1", "msg-1", channel.ChatTypeGroup)
	_, err := resolver.ResolveSender(context.Background(), rf.resolvedInstallation(t), msg)
	if !errors.Is(err, engine.ErrSenderNotMember) {
		t.Fatalf("ResolveSender error = %v, want engine.ErrSenderNotMember", err)
	}
}

// ---- dedup -------------------------------------------------------------------

func TestDiscordResolversDB_DedupClaimMarkRoundTrip(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	installationID := util.MustParseUUID(rf.installationID)
	deduper := &discordDeduper{q: db.New(pool)}
	ctx := context.Background()

	token, err := deduper.Claim(ctx, installationID, "dedup-msg-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// A second Claim on the same (installation, message_id) before Mark must
	// report the duplicate — the two-phase idempotency gate's whole point.
	if _, err := deduper.Claim(ctx, installationID, "dedup-msg-1"); !errors.Is(err, engine.ErrDuplicate) {
		t.Fatalf("second Claim error = %v, want engine.ErrDuplicate", err)
	}

	if err := deduper.Mark(ctx, installationID, "dedup-msg-1", token); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	var processedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT processed_at FROM channel_inbound_message_dedup WHERE installation_id = $1 AND message_id = $2`,
		rf.installationID, "dedup-msg-1").Scan(&processedAt); err != nil {
		t.Fatalf("query dedup row: %v", err)
	}
	if processedAt == nil {
		t.Fatal("processed_at is NULL after Mark, want set")
	}
}

func TestDiscordResolversDB_DedupRelease(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	installationID := util.MustParseUUID(rf.installationID)
	deduper := &discordDeduper{q: db.New(pool)}
	ctx := context.Background()

	token, err := deduper.Claim(ctx, installationID, "dedup-msg-release")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := deduper.Release(ctx, installationID, "dedup-msg-release", token); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Released claim frees the slot: a fresh Claim on the same key must
	// succeed again instead of reporting a duplicate.
	if _, err := deduper.Claim(ctx, installationID, "dedup-msg-release"); err != nil {
		t.Fatalf("re-Claim after Release: %v", err)
	}
}

// ---- session binding ----------------------------------------------------

func TestDiscordResolversDB_EnsureSessionCreatesGroupBindingThenStartSessionAppends(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	ctx := context.Background()
	queries := db.New(pool)
	binder := &discordSessionBinder{session: engine.NewChatSession(queries, pool, TypeDiscord, engine.SessionTitles{
		Group: "Discord channel", Direct: "Discord direct message", Fallback: "Discord chat",
	})}
	inst := rf.resolvedInstallation(t)
	userID := util.MustParseUUID(rf.userID)

	msg := discordInboundMsg(t, rf.appID, "session-sender", "session-chat-1", "session-msg-1", channel.ChatTypeGroup)

	sessionID, err := binder.EnsureSession(ctx, engine.EnsureSessionParams{
		Installation: inst, Sender: userID, Message: msg,
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if !sessionID.Valid {
		t.Fatal("EnsureSession returned an invalid session id")
	}

	var chatType, channelChatID string
	if err := pool.QueryRow(ctx,
		`SELECT chat_type, channel_chat_id FROM channel_chat_session_binding WHERE chat_session_id = $1`,
		sessionID).Scan(&chatType, &channelChatID); err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if chatType != "group" {
		t.Fatalf("chat_type = %q, want %q", chatType, "group")
	}
	if channelChatID != "session-chat-1" {
		t.Fatalf("channel_chat_id = %q, want %q (discordSessionRouting's binding key)", channelChatID, "session-chat-1")
	}

	// StartSession (/new route rotation) on the same message shape must
	// bind the same key/chat_type derivation and persist a fresh session.
	startMsg := discordInboundMsg(t, rf.appID, "session-sender", "session-chat-1", "session-msg-2", channel.ChatTypeGroup)
	result, err := binder.StartSession(ctx, engine.StartSessionParams{
		Installation: inst, Creator: userID, Sender: userID, Message: startMsg,
		PersistMessage: true,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if !result.SessionID.Valid {
		t.Fatal("StartSession returned an invalid session id")
	}
}

func TestDiscordResolversDB_EnsureSessionP2PBindsDirect(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	ctx := context.Background()
	queries := db.New(pool)
	binder := &discordSessionBinder{session: engine.NewChatSession(queries, pool, TypeDiscord, engine.SessionTitles{
		Group: "Discord channel", Direct: "Discord direct message", Fallback: "Discord chat",
	})}
	inst := rf.resolvedInstallation(t)
	userID := util.MustParseUUID(rf.userID)

	msg := discordInboundMsg(t, rf.appID, "dm-sender", "dm-channel-1", "dm-msg-1", channel.ChatTypeP2P)
	sessionID, err := binder.EnsureSession(ctx, engine.EnsureSessionParams{
		Installation: inst, Sender: userID, Message: msg,
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}

	var chatType string
	if err := pool.QueryRow(ctx,
		`SELECT chat_type FROM channel_chat_session_binding WHERE chat_session_id = $1`, sessionID).Scan(&chatType); err != nil {
		t.Fatalf("query binding: %v", err)
	}
	if chatType != "p2p" {
		t.Fatalf("chat_type = %q, want %q", chatType, "p2p")
	}
}

// ---- audit --------------------------------------------------------------

func TestDiscordResolversDB_RecordDropWritesAuditRow(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	ctx := context.Background()
	auditor := &discordAuditor{q: db.New(pool)}
	installationID := util.MustParseUUID(rf.installationID)

	msg := discordInboundMsg(t, rf.appID, "drop-sender", "drop-chat-1", "drop-msg-1", channel.ChatTypeGroup)
	if err := auditor.RecordDrop(ctx, installationID, msg, engine.DropReasonNotAddressedInGroup); err != nil {
		t.Fatalf("RecordDrop: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_inbound_audit WHERE installation_id = $1`, rf.installationID)
	})

	var dropReason, channelType, channelChatID, channelMessageID string
	if err := pool.QueryRow(ctx,
		`SELECT drop_reason, channel_type, channel_chat_id, channel_message_id
		 FROM channel_inbound_audit WHERE installation_id = $1 AND channel_message_id = $2`,
		rf.installationID, "drop-msg-1").Scan(&dropReason, &channelType, &channelChatID, &channelMessageID); err != nil {
		t.Fatalf("query audit row: %v", err)
	}
	if dropReason != string(engine.DropReasonNotAddressedInGroup) {
		t.Fatalf("drop_reason = %q, want %q", dropReason, engine.DropReasonNotAddressedInGroup)
	}
	if channelType != string(TypeDiscord) {
		t.Fatalf("channel_type = %q, want %q", channelType, TypeDiscord)
	}
	if channelChatID != "drop-chat-1" {
		t.Fatalf("channel_chat_id = %q, want %q", channelChatID, "drop-chat-1")
	}
	if channelMessageID != "drop-msg-1" {
		t.Fatalf("channel_message_id = %q, want %q", channelMessageID, "drop-msg-1")
	}
}
