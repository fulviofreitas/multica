package discord

// persist_test.go covers persist.go's two halves separately:
//
//   - The unique-violation classifier and the owner-conflict classifier are
//     pure functions (no I/O), so they run unconditionally here — this is
//     the ONE place in this file that proves anything without a database.
//   - Register/Persist's wiring (PrepareInstall -> json.Marshal -> Persist,
//     and malformed-token short-circuiting before persistence) is exercised
//     against fakePersistQueries/fakeTxStarter, also unconditionally: no
//     database is needed to prove the plumbing calls the right things in
//     the right order.
//   - The actual transaction behavior (reclaim-then-upsert, the 23505 path
//     end to end, and each ownership-conflict scenario against real rows)
//     needs Postgres and is gated by discordPersistTestDB, mirroring
//     wecom/installation_reclaim_test.go and
//     dingtalk/byo_install_db_test.go. These SKIP in this sandbox (no
//     DATABASE_URL, no Postgres) and RUN in CI.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- pure classifiers (unconditional, no database) -----------------------

func TestClassifyUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "23505 on the app_id routing index", err: &pgconn.PgError{Code: "23505"}, want: true},
		{name: "wrapped 23505", err: fmt.Errorf("upsert discord installation: %w", &pgconn.PgError{Code: "23505"}), want: true},
		{name: "different pg error code", err: &pgconn.PgError{Code: "23503"}, want: false},
		{name: "no rows is not a unique violation", err: pgx.ErrNoRows, want: false},
		{name: "nil error", err: nil, want: false},
		{name: "non-pg error", err: errors.New("boom"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUniqueViolation(tc.err); got != tc.want {
				t.Fatalf("classifyUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyOwnerConflict(t *testing.T) {
	workspaceID := discordTestUUID(1)
	tests := []struct {
		name  string
		owner db.GetChannelInstallationOwnerByAppIDRow
		want  error
	}{
		{
			name:  "same workspace, live agent",
			owner: db.GetChannelInstallationOwnerByAppIDRow{WorkspaceID: workspaceID},
			want:  ErrBotOwnedBySameWorkspace,
		},
		{
			name: "same workspace, archived agent",
			owner: db.GetChannelInstallationOwnerByAppIDRow{
				WorkspaceID:     workspaceID,
				AgentArchivedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			},
			want: ErrBotOwnedByArchivedAgent,
		},
		{
			name:  "different workspace",
			owner: db.GetChannelInstallationOwnerByAppIDRow{WorkspaceID: discordTestUUID(2)},
			want:  ErrBotOwnedByAnotherWorkspace,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := classifyOwnerConflict(tc.owner, workspaceID); !errors.Is(err, tc.want) {
				t.Fatalf("classifyOwnerConflict(%+v) = %v, want %v", tc.owner, err, tc.want)
			}
		})
	}
}

func TestLiveOwnerConflictErrDegradesSafelyOnLookupFailure(t *testing.T) {
	// A failed owner lookup (including pgx.ErrNoRows, which should not
	// happen right after a 23505 but must still be handled) must not be
	// reported as a same-workspace or archived-agent conflict it never
	// observed — see liveOwnerConflictErr's doc comment.
	q := &fakePersistQueries{ownerErr: errors.New("database unavailable")}
	err := liveOwnerConflictErr(context.Background(), q, discordTestUUID(1), "app-id")
	if !errors.Is(err, ErrBotOwnedByAnotherWorkspace) {
		t.Fatalf("liveOwnerConflictErr on lookup failure = %v, want ErrBotOwnedByAnotherWorkspace", err)
	}
}

// ---- Register/Persist wiring (unconditional, fakes only) -----------------

type fakePersistQueries struct {
	reclaimErr error

	upsertCalled bool
	upsert       db.UpsertChannelInstallationParams
	rowID        pgtype.UUID
	upsertErr    error

	owner    db.GetChannelInstallationOwnerByAppIDRow
	ownerErr error
}

func (f *fakePersistQueries) WithTx(pgx.Tx) persistQueries { return f }

func (f *fakePersistQueries) ReclaimDeadChannelInstallationByAppID(context.Context, db.ReclaimDeadChannelInstallationByAppIDParams) (pgtype.UUID, error) {
	if f.reclaimErr != nil {
		return pgtype.UUID{}, f.reclaimErr
	}
	return pgtype.UUID{}, pgx.ErrNoRows
}

func (f *fakePersistQueries) UpsertChannelInstallation(_ context.Context, p db.UpsertChannelInstallationParams) (db.ChannelInstallation, error) {
	f.upsertCalled, f.upsert = true, p
	if f.upsertErr != nil {
		return db.ChannelInstallation{}, f.upsertErr
	}
	return db.ChannelInstallation{
		ID: f.rowID, WorkspaceID: p.WorkspaceID, AgentID: p.AgentID,
		ChannelType: p.ChannelType, Config: p.Config, InstallerUserID: p.InstallerUserID,
		Status: "active",
	}, nil
}

func (f *fakePersistQueries) GetChannelInstallationOwnerByAppID(context.Context, db.GetChannelInstallationOwnerByAppIDParams) (db.GetChannelInstallationOwnerByAppIDRow, error) {
	return f.owner, f.ownerErr
}

type fakeTx struct {
	pgx.Tx
	committed bool
}

func (t *fakeTx) Commit(context.Context) error   { t.committed = true; return nil }
func (t *fakeTx) Rollback(context.Context) error { return nil }

type fakeTxStarter struct{ tx *fakeTx }

func (f fakeTxStarter) Begin(context.Context) (pgx.Tx, error) { return f.tx, nil }

func discordPersistTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func discordVerifyAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"999888777","username":"multica_test_bot","bot":true}`))
	}))
}

func discordTestUUID(n byte) pgtype.UUID {
	var b [16]byte
	b[15] = n
	return pgtype.UUID{Bytes: b, Valid: true}
}

func TestRegisterComposesPrepareInstallAndPersist(t *testing.T) {
	srv := discordVerifyAPIServer(t)
	defer srv.Close()

	svc, err := NewInstallService(discordPersistTestBox(t), srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.apiBase = srv.URL

	q := &fakePersistQueries{rowID: discordTestUUID(9)}
	txStarter := fakeTxStarter{tx: &fakeTx{}}

	p := RegisterParams{
		WorkspaceID: discordTestUUID(1),
		AgentID:     discordTestUUID(2),
		InstallerID: discordTestUUID(3),
		BotToken:    "aaaaaa.bbbb.cccccccccccccccccccc",
	}
	row, err := Register(context.Background(), svc, q, txStarter, p)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !q.upsertCalled || row.ID != q.rowID || q.upsert.ChannelType != string(TypeDiscord) {
		t.Fatalf("persisted row = %+v, upsert params = %+v", row, q.upsert)
	}
	if q.upsert.WorkspaceID != p.WorkspaceID || q.upsert.AgentID != p.AgentID || q.upsert.InstallerUserID != p.InstallerID {
		t.Fatalf("upsert identity params = %+v, want workspace=%v agent=%v installer=%v", q.upsert, p.WorkspaceID, p.AgentID, p.InstallerID)
	}
	if !txStarter.tx.committed {
		t.Fatal("Register did not commit the install transaction")
	}

	var cfg installConfig
	if err := json.Unmarshal(q.upsert.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != "999888777" || cfg.BotUsername != "multica_test_bot" {
		t.Fatalf("persisted config = %+v", cfg)
	}
	plain, err := decryptToken(cfg.BotTokenEncrypted, svc.box.Open)
	if err != nil || plain != p.BotToken {
		t.Fatalf("decrypted token = %q, %v, want %q", plain, err, p.BotToken)
	}
}

func TestRegisterRejectsMalformedTokenBeforePersistence(t *testing.T) {
	svc, err := NewInstallService(discordPersistTestBox(t), http.DefaultClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := &fakePersistQueries{}
	txStarter := fakeTxStarter{tx: &fakeTx{}}

	_, err = Register(context.Background(), svc, q, txStarter, RegisterParams{BotToken: "not-a-token"})
	if !errors.Is(err, ErrInvalidBotToken) {
		t.Fatalf("Register error = %v, want ErrInvalidBotToken", err)
	}
	if q.upsertCalled {
		t.Fatal("malformed token reached persistence")
	}
}

func TestPersistClassifiesUniqueViolationOnUpsert(t *testing.T) {
	owner := db.GetChannelInstallationOwnerByAppIDRow{WorkspaceID: discordTestUUID(2)}
	q := &fakePersistQueries{upsertErr: &pgconn.PgError{Code: pgUniqueViolation}, owner: owner}
	txStarter := fakeTxStarter{tx: &fakeTx{}}

	_, err := Persist(context.Background(), q, txStarter, installPersist{
		wsID:     discordTestUUID(1),
		agentID:  discordTestUUID(2),
		appIDKey: "app-id",
	})
	if !errors.Is(err, ErrBotOwnedByAnotherWorkspace) {
		t.Fatalf("Persist error = %v, want ErrBotOwnedByAnotherWorkspace", err)
	}
	if txStarter.tx.committed {
		t.Fatal("Persist committed a transaction that hit a unique violation")
	}
}

// ---- DB-backed transaction behavior (skip without DATABASE_URL) ----------

func discordPersistTestDB(t *testing.T) *pgxpool.Pool {
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

// discordPersistFixture wires a testutil.Fixture to a fresh workspace/user
// and returns the pieces a persist test needs to build agents and run
// Persist against real rows. Rows are removed by testutil's own
// t.Cleanup-based teardown in reverse insertion order (see
// server/internal/testutil/db.go), plus an explicit channel_installation
// sweep here since Persist itself inserts rows testutil never created.
var discordPersistFixtureSeq atomic.Int64

// uniqueSuffix returns a per-process-unique token so two fixtures built by
// the same test (the cross-workspace/cross-agent conflict tests each need
// two) never collide on the user table's UNIQUE email or the workspace
// table's UNIQUE slug.
func uniqueSuffix() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), discordPersistFixtureSeq.Add(1))
}

func discordPersistFixture(t *testing.T, pool *pgxpool.Pool) (*testutil.Fixture, string) {
	t.Helper()
	suffix := uniqueSuffix()
	f := testutil.New(pool, "", "")
	f.UserID = f.User(t, "Discord Persist Test", "discord-persist-test-"+suffix+"@multica.ai")
	f.WorkspaceID = f.Workspace(t, "Discord Persist Test", "discord-persist-test-"+suffix)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_installation WHERE workspace_id = $1`, f.WorkspaceID)
	})
	return f, f.UserID
}

func discordPersistSvc(t *testing.T, pool *pgxpool.Pool) persistQueries {
	t.Helper()
	return NewPersistQueries(db.New(pool))
}

func discordPersistCfg(t *testing.T, appID string) []byte {
	t.Helper()
	b, err := json.Marshal(installConfig{AppID: appID, BotUsername: "botuser", BotTokenEncrypted: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPersistDB_FreshInstallSucceeds(t *testing.T) {
	pool := discordPersistTestDB(t)
	f, userID := discordPersistFixture(t, pool)
	agentID := f.Agent(t, "fresh-install-agent", "")
	q := discordPersistSvc(t, pool)

	inst, err := Persist(context.Background(), q, pool, installPersist{
		wsID:        util.MustParseUUID(f.WorkspaceID),
		agentID:     util.MustParseUUID(agentID),
		installerID: util.MustParseUUID(userID),
		appIDKey:    "discord-app-fresh",
		configJSON:  discordPersistCfg(t, "discord-app-fresh"),
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if inst.Status != "active" || inst.ChannelType != string(TypeDiscord) {
		t.Fatalf("installation = %+v", inst)
	}
}

func TestPersistDB_ReinstallSameWorkspaceAgentUpdatesInPlace(t *testing.T) {
	pool := discordPersistTestDB(t)
	f, userID := discordPersistFixture(t, pool)
	agentID := f.Agent(t, "reinstall-agent", "")
	q := discordPersistSvc(t, pool)
	ws, ag, inst := util.MustParseUUID(f.WorkspaceID), util.MustParseUUID(agentID), util.MustParseUUID(userID)

	first, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: ag, installerID: inst,
		appIDKey: "discord-app-reinstall", configJSON: discordPersistCfg(t, "discord-app-reinstall"),
	})
	if err != nil {
		t.Fatalf("first Persist: %v", err)
	}

	second, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: ag, installerID: inst,
		appIDKey: "discord-app-reinstall", configJSON: discordPersistCfg(t, "discord-app-reinstall"),
	})
	if err != nil {
		t.Fatalf("second Persist: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("reinstall created a new row: first=%v second=%v", first.ID, second.ID)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM channel_installation WHERE workspace_id = $1 AND agent_id = $2 AND channel_type = 'discord'`,
		f.WorkspaceID, agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("row count after reinstall = %d, want 1", n)
	}
}

func TestPersistDB_RefusesLiveOwnerInAnotherWorkspace(t *testing.T) {
	pool := discordPersistTestDB(t)
	ownerF, ownerUser := discordPersistFixture(t, pool)
	ownerAgent := ownerF.Agent(t, "owner-agent", "")
	q := discordPersistSvc(t, pool)

	const appID = "discord-app-cross-workspace"
	if _, err := Persist(context.Background(), q, pool, installPersist{
		wsID: util.MustParseUUID(ownerF.WorkspaceID), agentID: util.MustParseUUID(ownerAgent),
		installerID: util.MustParseUUID(ownerUser), appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	}); err != nil {
		t.Fatalf("seed owner installation: %v", err)
	}

	requesterF, requesterUser := discordPersistFixture(t, pool)
	requesterAgent := requesterF.Agent(t, "requester-agent", "")

	_, err := Persist(context.Background(), q, pool, installPersist{
		wsID: util.MustParseUUID(requesterF.WorkspaceID), agentID: util.MustParseUUID(requesterAgent),
		installerID: util.MustParseUUID(requesterUser), appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	})
	if !errors.Is(err, ErrBotOwnedByAnotherWorkspace) {
		t.Fatalf("Persist error = %v, want ErrBotOwnedByAnotherWorkspace", err)
	}
}

func TestPersistDB_RefusesLiveOwnerOnDifferentAgentSameWorkspace(t *testing.T) {
	pool := discordPersistTestDB(t)
	f, userID := discordPersistFixture(t, pool)
	ownerAgent := f.Agent(t, "owner-agent-same-ws", "")
	otherAgent := f.Agent(t, "other-agent-same-ws", "")
	q := discordPersistSvc(t, pool)
	ws, inst := util.MustParseUUID(f.WorkspaceID), util.MustParseUUID(userID)

	const appID = "discord-app-same-workspace"
	if _, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: util.MustParseUUID(ownerAgent), installerID: inst,
		appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	}); err != nil {
		t.Fatalf("seed owner installation: %v", err)
	}

	_, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: util.MustParseUUID(otherAgent), installerID: inst,
		appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	})
	if !errors.Is(err, ErrBotOwnedBySameWorkspace) {
		t.Fatalf("Persist error = %v, want ErrBotOwnedBySameWorkspace", err)
	}
}

func TestPersistDB_RefusesArchivedAgentOwner(t *testing.T) {
	pool := discordPersistTestDB(t)
	f, userID := discordPersistFixture(t, pool)
	archivedAgent := f.Agent(t, "archived-agent", "", testutil.Cols{"archived_at": testutil.Raw("now()")})
	otherAgent := f.Agent(t, "other-agent-archived-case", "")
	q := discordPersistSvc(t, pool)
	ws, inst := util.MustParseUUID(f.WorkspaceID), util.MustParseUUID(userID)

	const appID = "discord-app-archived-owner"
	if _, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: util.MustParseUUID(archivedAgent), installerID: inst,
		appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	}); err != nil {
		t.Fatalf("seed archived owner installation: %v", err)
	}

	_, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: util.MustParseUUID(otherAgent), installerID: inst,
		appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	})
	if !errors.Is(err, ErrBotOwnedByArchivedAgent) {
		t.Fatalf("Persist error = %v, want ErrBotOwnedByArchivedAgent", err)
	}
}

func TestPersistDB_ReclaimsDeadRevokedSlot(t *testing.T) {
	pool := discordPersistTestDB(t)
	f, userID := discordPersistFixture(t, pool)
	deadAgent := f.Agent(t, "dead-owner-agent", "")
	newAgent := f.Agent(t, "reclaiming-agent", "")
	q := discordPersistSvc(t, pool)
	ws, inst := util.MustParseUUID(f.WorkspaceID), util.MustParseUUID(userID)

	const appID = "discord-app-dead-slot"
	if _, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: util.MustParseUUID(deadAgent), installerID: inst,
		appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	}); err != nil {
		t.Fatalf("seed dead-to-be installation: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE channel_installation SET status = 'revoked' WHERE workspace_id = $1 AND agent_id = $2 AND channel_type = 'discord'`,
		f.WorkspaceID, deadAgent); err != nil {
		t.Fatalf("revoke seed installation: %v", err)
	}

	got, err := Persist(context.Background(), q, pool, installPersist{
		wsID: ws, agentID: util.MustParseUUID(newAgent), installerID: inst,
		appIDKey: appID, configJSON: discordPersistCfg(t, appID),
	})
	if err != nil {
		t.Fatalf("Persist over a dead revoked slot: %v", err)
	}
	if got.AgentID != util.MustParseUUID(newAgent) {
		t.Fatalf("new owner agent = %v, want %v", got.AgentID, newAgent)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM channel_installation WHERE config->>'app_id' = $1`, appID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("row count for reclaimed app_id = %d, want 1 (dead owner not cleared)", n)
	}
}
