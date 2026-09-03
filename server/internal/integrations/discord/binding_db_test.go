package discord

// binding_db_test.go covers the Postgres-backed half of binding.go: the real
// RedeemAndBind transaction (ConsumeChannelBindingToken /
// GetMemberByUserAndWorkspace / CreateChannelUserBinding) and the rollback
// guarantees around it. Mirrors resolvers_db_test.go's
// discordResolversTestDB gate exactly: SKIP without DATABASE_URL/Postgres
// (this sandbox), RUN in CI. It reuses resolvers_db_test.go's
// discordResolversTestDB and newDiscordResolversFixture (same package, same
// workspace/user/member/installation shape RedeemAndBind needs) instead of
// standing up a second, parallel fixture builder.
//
// Everything provable without a database (token randomness, hash-only
// persistence, the TTL cap, sentinel distinctness, the missing-TxStarter
// guard) lives in binding_test.go instead.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// insertLiveDiscordBindingToken writes a channel_binding_token row directly
// (bypassing Mint) so a test can control expires_at/consumed_at precisely.
// channel_binding_token has no id column (its primary key is token_hash), so
// this uses InsertNoID rather than Fixture.Insert.
func insertLiveDiscordBindingToken(t *testing.T, rf *discordResolversFixture, raw, channelUserID string, over testutil.Cols) {
	t.Helper()
	tokenHash := hashBindingToken(raw)
	cols := testutil.Cols{
		"token_hash":      tokenHash,
		"workspace_id":    rf.workspaceID,
		"installation_id": rf.installationID,
		"channel_type":    string(TypeDiscord),
		"channel_user_id": channelUserID,
		"expires_at":      testutil.Raw("now() + interval '5 minutes'"),
	}
	for k, v := range over {
		cols[k] = v
	}
	rf.f.InsertNoID(t, "channel_binding_token", cols, "token_hash = $1", tokenHash)
}

func bindingTokenConsumedAt(t *testing.T, pool *pgxpool.Pool, raw string) (consumed bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT consumed_at IS NOT NULL FROM channel_binding_token WHERE token_hash = $1`,
		hashBindingToken(raw)).Scan(&consumed); err != nil {
		t.Fatalf("query channel_binding_token.consumed_at: %v", err)
	}
	return consumed
}

// TestDiscordBindingDB_MintThenRedeemBindsUser is the happy path: Mint writes
// a live token, RedeemAndBind consumes it and creates the channel_user_binding
// row linking the Discord user id to the redeeming Multica user (taken from
// multicaUserID, i.e. the caller/session — never from the token payload).
func TestDiscordBindingDB_MintThenRedeemBindsUser(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	svc := NewBindingTokenService(db.New(pool), pool)
	ctx := context.Background()
	const discordUserID = "discord-user-happy-path"

	token, err := svc.Mint(ctx, util.MustParseUUID(rf.workspaceID), util.MustParseUUID(rf.installationID), discordUserID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token.Raw == "" {
		t.Fatal("Mint returned an empty raw token")
	}

	redeemed, err := svc.RedeemAndBind(ctx, token.Raw, util.MustParseUUID(rf.userID))
	if err != nil {
		t.Fatalf("RedeemAndBind: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM channel_user_binding WHERE installation_id = $1 AND channel_user_id = $2`,
			rf.installationID, discordUserID)
	})

	if redeemed.WorkspaceID != util.MustParseUUID(rf.workspaceID) {
		t.Fatalf("redeemed.WorkspaceID = %v, want %v", redeemed.WorkspaceID, rf.workspaceID)
	}
	if redeemed.InstallationID != util.MustParseUUID(rf.installationID) {
		t.Fatalf("redeemed.InstallationID = %v, want %v", redeemed.InstallationID, rf.installationID)
	}
	if redeemed.DiscordUserID != discordUserID {
		t.Fatalf("redeemed.DiscordUserID = %q, want %q", redeemed.DiscordUserID, discordUserID)
	}

	var boundUserID string
	if err := pool.QueryRow(ctx,
		`SELECT multica_user_id::text FROM channel_user_binding WHERE installation_id = $1 AND channel_user_id = $2`,
		rf.installationID, discordUserID).Scan(&boundUserID); err != nil {
		t.Fatalf("query channel_user_binding: %v", err)
	}
	if boundUserID != rf.userID {
		t.Fatalf("channel_user_binding.multica_user_id = %q, want %q", boundUserID, rf.userID)
	}

	if !bindingTokenConsumedAt(t, pool, token.Raw) {
		t.Fatal("a successfully redeemed token must have consumed_at set")
	}
}

// TestDiscordBindingDB_ExpiredTokenRejected: ConsumeChannelBindingToken's own
// `expires_at > now()` predicate must reject a token past its TTL.
func TestDiscordBindingDB_ExpiredTokenRejected(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	svc := NewBindingTokenService(db.New(pool), pool)
	const raw = "expired-raw-token-sentinel"
	insertLiveDiscordBindingToken(t, rf, raw, "discord-user-expired", testutil.Cols{
		"expires_at": testutil.Raw("now() - interval '1 minute'"),
	})

	_, err := svc.RedeemAndBind(context.Background(), raw, util.MustParseUUID(rf.userID))
	if !errors.Is(err, ErrBindingTokenInvalid) {
		t.Fatalf("RedeemAndBind error = %v, want ErrBindingTokenInvalid", err)
	}
}

// TestDiscordBindingDB_AlreadyConsumedTokenRejected: redeeming the same raw
// token twice must fail the second time — ConsumeChannelBindingToken's
// `consumed_at IS NULL` predicate is what makes a token single-use.
func TestDiscordBindingDB_AlreadyConsumedTokenRejected(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	svc := NewBindingTokenService(db.New(pool), pool)
	ctx := context.Background()
	const discordUserID = "discord-user-reused-token"

	token, err := svc.Mint(ctx, util.MustParseUUID(rf.workspaceID), util.MustParseUUID(rf.installationID), discordUserID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := svc.RedeemAndBind(ctx, token.Raw, util.MustParseUUID(rf.userID)); err != nil {
		t.Fatalf("first RedeemAndBind: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM channel_user_binding WHERE installation_id = $1 AND channel_user_id = $2`,
			rf.installationID, discordUserID)
	})

	_, err = svc.RedeemAndBind(ctx, token.Raw, util.MustParseUUID(rf.userID))
	if !errors.Is(err, ErrBindingTokenInvalid) {
		t.Fatalf("second RedeemAndBind error = %v, want ErrBindingTokenInvalid", err)
	}
}

// TestDiscordBindingDB_NonMemberRedeemDoesNotConsumeToken covers two of the
// task's bullets at once because they are the same causal scenario: a
// redeemer who is not a workspace member (i) gets ErrBindingNotWorkspaceMember,
// and (ii) does not burn the token doing so — RedeemAndBind's explicit
// membership gate returns before Commit, so the deferred tx.Rollback (not the
// already-applied UPDATE) is what actually lands. The second RedeemAndBind
// call proves that directly: it succeeds, which is only possible if the first
// attempt's consume never committed.
func TestDiscordBindingDB_NonMemberRedeemDoesNotConsumeToken(t *testing.T) {
	pool := discordResolversTestDB(t)
	rf := newDiscordResolversFixture(t, pool)
	svc := NewBindingTokenService(db.New(pool), pool)
	ctx := context.Background()
	const discordUserID = "discord-user-non-member-attempt"

	// A second user who is never added as a member of rf.workspaceID.
	nonMemberUserID := rf.f.User(t, "Discord Binding Non-Member", "discord-binding-non-member-"+discordResolversUniqueSuffix()+"@multica.ai")

	token, err := svc.Mint(ctx, util.MustParseUUID(rf.workspaceID), util.MustParseUUID(rf.installationID), discordUserID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = svc.RedeemAndBind(ctx, token.Raw, util.MustParseUUID(nonMemberUserID))
	if !errors.Is(err, ErrBindingNotWorkspaceMember) {
		t.Fatalf("RedeemAndBind (non-member) error = %v, want ErrBindingNotWorkspaceMember", err)
	}
	if bindingTokenConsumedAt(t, pool, token.Raw) {
		t.Fatal("a failed (non-member) bind must not consume the token")
	}

	// The token must still be live: a legitimate member can redeem it.
	if _, err := svc.RedeemAndBind(ctx, token.Raw, util.MustParseUUID(rf.userID)); err != nil {
		t.Fatalf("RedeemAndBind by the actual member after the failed non-member attempt: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM channel_user_binding WHERE installation_id = $1 AND channel_user_id = $2`,
			rf.installationID, discordUserID)
	})
}
