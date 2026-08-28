package discord

// binding_test.go covers binding.go WITHOUT a database:
//   - token generation is random and URL-safe (high entropy input to
//     hashBindingToken)
//   - hashBindingToken is deterministic and never returns the raw value
//   - Mint persists ONLY the hash: fakeBindingMintDB captures exactly the
//     positional args CreateChannelBindingToken's generated code sends to
//     the DB layer (db.Queries never sees the raw token itself), and the
//     test asserts the raw value appears nowhere in them
//   - the TTL Mint requests never exceeds BindingTokenTTL (15 minutes),
//     matching the channel_binding_token_ttl_cap CHECK
//     (migrations/124_channel_generalization.up.sql)
//   - RedeemAndBind refuses to run without a TxStarter
//   - the three sentinel errors stay distinct from one another
//
// The redeem transaction itself (ConsumeChannelBindingToken /
// GetMemberByUserAndWorkspace / CreateChannelUserBinding, and the rollback
// guarantees around them) needs a real Postgres transaction and is covered
// by binding_db_test.go instead.
//
// The bind prompt's message text (bind path + token) is replier.go's
// responsibility, not binding.go's — Mint here only returns a raw secret,
// it never renders a message. That text is already covered unconditionally
// by TestReplier_NeedsBinding_DirectMessageMintsBearerLink in
// replier_test.go; asserting it again here would just be the same product
// behavior checked a second time in a different file for no new coverage.

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- fakeBindingMintDB: captures what Mint sends to the DB layer, without a
// database. Mint discards CreateChannelBindingToken's returned row (only its
// error), so Scan can be a no-op — the args QueryRow was called with are the
// whole point of this fake. ---------------------------------------------

type fakeBindingMintDB struct {
	db.DBTX
	calls    int
	lastArgs []any
	err      error
}

func (f *fakeBindingMintDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	f.calls++
	f.lastArgs = args
	return &fakeBindingMintRow{err: f.err}
}

type fakeBindingMintRow struct {
	pgx.Row
	err error
}

func (r *fakeBindingMintRow) Scan(...any) error { return r.err }

func mustMintService(fake *fakeBindingMintDB) *BindingTokenService {
	return NewBindingTokenService(db.New(fake), nil)
}

func TestRandomBindingTokenIsUniqueAndURLSafe(t *testing.T) {
	first, err := randomBindingToken(32)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	second, err := randomBindingToken(32)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if first == second {
		t.Fatal("two random binding tokens were identical")
	}
	if len(first) < 32 {
		t.Fatalf("token %q looks too short for 32 bytes of entropy", first)
	}
	if ok := regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(first); !ok {
		t.Fatalf("token is not raw URL-safe base64: %q", first)
	}
}

func TestHashBindingTokenIsDeterministicAndDoesNotRetainRawValue(t *testing.T) {
	const raw = "discord-binding-token-sentinel"
	first := hashBindingToken(raw)
	second := hashBindingToken(raw)
	if first != second {
		t.Fatalf("hash changed between calls: %q != %q", first, second)
	}
	if first == raw || len(first) != 64 {
		t.Fatalf("unexpected SHA-256 hex representation: %q", first)
	}
}

// TestMintPersistsOnlyTheHashNeverTheRawToken drives the real Mint code path
// (production db.Queries wired to a fake DBTX) and inspects the exact
// positional args db.Queries.CreateChannelBindingToken sent to QueryRow —
// the DB layer's actual boundary. The raw token must not appear anywhere in
// them; only its SHA-256 hex hash may.
func TestMintPersistsOnlyTheHashNeverTheRawToken(t *testing.T) {
	fake := &fakeBindingMintDB{}
	svc := mustMintService(fake)

	token, err := svc.Mint(context.Background(), discordTestUUID(1), discordTestUUID(2), "discord-user-1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token.Raw == "" {
		t.Fatal("Mint returned an empty raw token")
	}
	if fake.calls != 1 {
		t.Fatalf("CreateChannelBindingToken called %d times, want 1", fake.calls)
	}
	if len(fake.lastArgs) != 6 {
		t.Fatalf("CreateChannelBindingToken args = %d, want 6 (token_hash, workspace_id, installation_id, channel_type, channel_user_id, expires_at)", len(fake.lastArgs))
	}

	tokenHash, ok := fake.lastArgs[0].(string)
	if !ok {
		t.Fatalf("first arg (token_hash) is %T, want string", fake.lastArgs[0])
	}
	if tokenHash == token.Raw {
		t.Fatal("the raw token was passed to the DB layer instead of its hash")
	}
	if tokenHash != hashBindingToken(token.Raw) {
		t.Fatalf("persisted hash %q does not match hashBindingToken(raw)", tokenHash)
	}
	if len(tokenHash) != 64 {
		t.Fatalf("persisted token_hash %q is not a 64-char SHA-256 hex digest", tokenHash)
	}

	// Defense in depth: the raw value must not leak into any other
	// positional argument either.
	for i, arg := range fake.lastArgs {
		if s, ok := arg.(string); ok && s == token.Raw {
			t.Fatalf("raw token leaked into DB arg[%d]: %v", i, fake.lastArgs)
		}
	}
}

// TestMintNeverExceedsFifteenMinuteTTL pins Mint's requested expiry to
// BindingTokenTTL, in lockstep with the channel_binding_token table's
// channel_binding_token_ttl_cap CHECK (expires_at <= created_at + INTERVAL
// '15 minutes', migrations/124_channel_generalization.up.sql) and the
// CreateChannelBindingToken query's own LEAST($6::timestamptz, now() +
// INTERVAL '15 minutes') clamp (channel.sql.go). A caller-requested TTL
// longer than that is clamped server-side; this test proves Mint itself
// never even asks for more.
func TestMintNeverExceedsFifteenMinuteTTL(t *testing.T) {
	if BindingTokenTTL != 15*time.Minute {
		t.Fatalf("BindingTokenTTL = %v, want 15m (must match channel_binding_token_ttl_cap)", BindingTokenTTL)
	}

	fake := &fakeBindingMintDB{}
	svc := mustMintService(fake)
	fixedNow := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedNow }

	if _, err := svc.Mint(context.Background(), discordTestUUID(1), discordTestUUID(2), "discord-user-ttl"); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(fake.lastArgs) != 6 {
		t.Fatalf("CreateChannelBindingToken args = %d, want 6", len(fake.lastArgs))
	}
	expiresAt, ok := fake.lastArgs[5].(pgtype.Timestamptz)
	if !ok {
		t.Fatalf("sixth arg (expires_at) is %T, want pgtype.Timestamptz", fake.lastArgs[5])
	}
	if !expiresAt.Valid {
		t.Fatal("expires_at is not valid")
	}
	if got := expiresAt.Time.Sub(fixedNow); got != BindingTokenTTL {
		t.Fatalf("Mint requested a %v TTL, want exactly %v", got, BindingTokenTTL)
	}
	if expiresAt.Time.Sub(fixedNow) > 15*time.Minute {
		t.Fatalf("Mint requested TTL %v, exceeds the 15-minute DB cap", expiresAt.Time.Sub(fixedNow))
	}
}

func TestMintPropagatesQueryError(t *testing.T) {
	sentinel := &pgconn.PgError{Code: "40001"} // serialization failure, arbitrary non-nil pg error
	fake := &fakeBindingMintDB{err: sentinel}
	svc := mustMintService(fake)

	_, err := svc.Mint(context.Background(), discordTestUUID(1), discordTestUUID(2), "discord-user-err")
	if err == nil {
		t.Fatal("expected an error when CreateChannelBindingToken fails")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the underlying query error", err)
	}
}

func TestRedeemAndBindRequiresTransactionStarter(t *testing.T) {
	service := &BindingTokenService{}
	_, err := service.RedeemAndBind(context.Background(), "token", pgtype.UUID{})
	if err == nil || err.Error() != "discord: BindingTokenService missing TxStarter" {
		t.Fatalf("error = %v", err)
	}
}

func TestBindingErrorSentinelsRemainDistinct(t *testing.T) {
	errs := []error{
		ErrBindingTokenInvalid,
		ErrBindingAlreadyAssigned,
		ErrBindingNotWorkspaceMember,
	}
	for i := range errs {
		for j := range errs {
			if i != j && errors.Is(errs[i], errs[j]) {
				t.Fatalf("binding errors %v and %v overlap", errs[i], errs[j])
			}
		}
	}
}
