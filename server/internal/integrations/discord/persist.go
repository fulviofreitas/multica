package discord

// This file is subtask 4.3, the seam PrepareInstall's doc comment names:
// given a PreparedInstall (already validated live and encrypted at rest by
// install_service.go's PrepareInstall), run the shared install transaction
// and classify a routing-slot conflict.
//
// It deliberately mirrors telegram.InstallService.persistInstall and
// liveOwnerConflictErr (server/internal/integrations/telegram/install.go)
// field-for-field: same reclaim-then-upsert shape, same 23505 handling, same
// three-way conflict classification. Discord has no webhook concept, so
// telegram.ErrWebhookConfigured has no counterpart here.
//
// install_service.go is owned by another subtask and is not edited here:
// InstallService carries only the encryption box and the live-verification
// HTTP client, not a *db.Queries or a tx starter. Persist and Register below
// take the service, the queries and the tx starter as separate parameters
// instead of being methods on InstallService. Folding this into
// InstallService (once its constructor grows a queries/tx-starter
// parameter) is a follow-up.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	// ErrBotOwnedByAnotherWorkspace: the Discord application is already
	// connected to a live owner in a DIFFERENT Multica workspace.
	ErrBotOwnedByAnotherWorkspace = errors.New("discord: this bot is already connected to a different Multica workspace")
	// ErrBotOwnedBySameWorkspace: the application is already connected to a
	// different LIVE (non-archived) agent in the SAME workspace.
	ErrBotOwnedBySameWorkspace = errors.New("discord: this bot is already connected to another agent in this workspace")
	// ErrBotOwnedByArchivedAgent: the application's owning agent in this
	// workspace is archived. Archive is reversible, so the bot stays owned
	// by that agent rather than being silently reassigned; recovery is
	// unarchiving the agent or disconnecting the bot first.
	ErrBotOwnedByArchivedAgent = errors.New("discord: this bot is connected to an archived agent in this workspace")
)

const pgUniqueViolation = "23505"

// persistQueries is the slice of generated queries the persist step needs,
// interface-shaped so tests inject a fake — mirrors telegram.installQueries.
type persistQueries interface {
	WithTx(tx pgx.Tx) persistQueries
	UpsertChannelInstallation(ctx context.Context, arg db.UpsertChannelInstallationParams) (db.ChannelInstallation, error)
	ReclaimDeadChannelInstallationByAppID(ctx context.Context, arg db.ReclaimDeadChannelInstallationByAppIDParams) (pgtype.UUID, error)
	GetChannelInstallationOwnerByAppID(ctx context.Context, arg db.GetChannelInstallationOwnerByAppIDParams) (db.GetChannelInstallationOwnerByAppIDRow, error)
}

// dbPersistQueries adapts *db.Queries to persistQueries. Production callers
// wrap their *db.Queries with this; tests inject a fake directly.
type dbPersistQueries struct{ *db.Queries }

func (q dbPersistQueries) WithTx(tx pgx.Tx) persistQueries {
	return dbPersistQueries{q.Queries.WithTx(tx)}
}

// NewPersistQueries wraps a *db.Queries for use with Persist/Register.
func NewPersistQueries(q *db.Queries) persistQueries {
	return dbPersistQueries{q}
}

// RegisterParams are the inputs a caller (the install handler) supplies for
// a Discord bot install: which agent the bot represents, who is installing,
// and the pasted bot token. wsID/agentID/installerID are all trusted UUIDs
// from the request's already-resolved identity (workspace membership,
// loadAgentForUser) — see CLAUDE.md's Backend UUID Rules — so this package
// never re-parses a path/body string into a UUID itself.
type RegisterParams struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	InstallerID pgtype.UUID
	BotToken    string
}

// Register composes PrepareInstall (live verification + at-rest encryption,
// owned by install_service.go) with Persist (the install transaction,
// owned by this file). It is a package-level function rather than an
// InstallService method because InstallService does not hold a *db.Queries
// or tx starter; see the file doc comment.
func Register(ctx context.Context, svc *InstallService, q persistQueries, tx engine.TxStarter, p RegisterParams) (db.ChannelInstallation, error) {
	prepared, err := svc.PrepareInstall(ctx, p.BotToken)
	if err != nil {
		return db.ChannelInstallation{}, err
	}
	cfgJSON, err := json.Marshal(prepared.Config)
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encode discord installation config: %w", err)
	}
	return Persist(ctx, q, tx, installPersist{
		wsID:        p.WorkspaceID,
		agentID:     p.AgentID,
		installerID: p.InstallerID,
		appIDKey:    prepared.AppIDKey,
		configJSON:  cfgJSON,
	})
}

// installPersist carries the resolved fields Persist writes. All three UUID
// fields are already-resolved, trusted identities (see RegisterParams doc);
// appIDKey is untrusted Discord-supplied data that only ever appears on the
// right-hand side of a query parameter, never parsed as a UUID.
type installPersist struct {
	wsID        pgtype.UUID
	agentID     pgtype.UUID
	installerID pgtype.UUID
	appIDKey    string
	configJSON  []byte
}

// Persist upserts the installation keyed by (workspace_id, agent_id,
// channel_type): one Discord application per agent. Steps, in ONE
// transaction:
//
//  1. ReclaimDeadChannelInstallationByAppID frees the (channel_type, app_id)
//     routing slot from any DEAD prior owner (a revoked row held by a
//     different agent, or an orphaned row whose workspace/agent no longer
//     exists). A pgx.ErrNoRows return means nothing was dead — not an error.
//  2. UpsertChannelInstallation writes the row. Re-installing the same
//     (workspace, agent) pair updates in place.
//  3. A 23505 unique violation on idx_channel_installation_type_appid means
//     the app_id is still held by a LIVE owner after the reclaim: refuse
//     rather than steal, classified by liveOwnerConflictErr.
//
// Mirrors telegram.InstallService.persistInstall exactly (see that
// function's doc comment for the "why one transaction" and "why the reclaim
// runs first" rationale, which is channel-agnostic and not repeated here).
func Persist(ctx context.Context, q persistQueries, txStarter engine.TxStarter, p installPersist) (db.ChannelInstallation, error) {
	tx, err := txStarter.Begin(ctx)
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("begin discord install tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := q.WithTx(tx)

	if _, err := qtx.ReclaimDeadChannelInstallationByAppID(ctx, db.ReclaimDeadChannelInstallationByAppIDParams{
		ChannelType: string(TypeDiscord),
		AppID:       p.appIDKey,
		WorkspaceID: p.wsID,
		AgentID:     p.agentID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.ChannelInstallation{}, fmt.Errorf("reclaim dead discord installation: %w", err)
	}

	inst, err := qtx.UpsertChannelInstallation(ctx, db.UpsertChannelInstallationParams{
		WorkspaceID:     p.wsID,
		AgentID:         p.agentID,
		ChannelType:     string(TypeDiscord),
		Config:          p.configJSON,
		InstallerUserID: p.installerID,
	})
	if err != nil {
		if classifyUniqueViolation(err) {
			return db.ChannelInstallation{}, liveOwnerConflictErr(ctx, q, p.wsID, p.appIDKey)
		}
		return db.ChannelInstallation{}, fmt.Errorf("upsert discord installation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("commit discord install: %w", err)
	}
	return inst, nil
}

// liveOwnerConflictErr classifies who currently holds the (channel_type,
// app_id) routing slot into one of the three ownership-conflict sentinels,
// so the handler can render an accurate message instead of a catch-all
// "already connected". A lookup failure (including pgx.ErrNoRows, which
// should not happen right after a 23505 but is handled defensively rather
// than panicking) degrades to the cross-workspace sentinel: the safest
// default, since it never claims same-workspace/archived-agent state it did
// not actually observe. Mirrors telegram.InstallService.liveOwnerConflictErr.
func liveOwnerConflictErr(ctx context.Context, q persistQueries, requestingWorkspaceID pgtype.UUID, appID string) error {
	owner, err := q.GetChannelInstallationOwnerByAppID(ctx, db.GetChannelInstallationOwnerByAppIDParams{
		ChannelType: string(TypeDiscord),
		AppID:       appID,
	})
	if err != nil {
		return ErrBotOwnedByAnotherWorkspace
	}
	return classifyOwnerConflict(owner, requestingWorkspaceID)
}

// classifyOwnerConflict is the pure decision liveOwnerConflictErr wraps
// around a database round trip: given the resolved current owner row and
// the requesting workspace, pick the ownership-conflict sentinel. Split out
// so it is testable with a synthetic row and no database — see
// TestClassifyOwnerConflict in persist_test.go.
func classifyOwnerConflict(owner db.GetChannelInstallationOwnerByAppIDRow, requestingWorkspaceID pgtype.UUID) error {
	switch {
	case owner.WorkspaceID != requestingWorkspaceID:
		return ErrBotOwnedByAnotherWorkspace
	case owner.AgentArchivedAt.Valid:
		return ErrBotOwnedByArchivedAgent
	default:
		return ErrBotOwnedBySameWorkspace
	}
}

// classifyUniqueViolation reports whether err is the specific 23505 unique
// violation Persist expects from the (channel_type, app_id) routing index —
// split out from Persist's inline errors.As so it is independently testable
// against a synthetic *pgconn.PgError without a database (see
// TestClassifyUniqueViolation in persist_test.go). Any other error
// (including a 23505 on a different constraint, which the code column alone
// cannot distinguish) is still routed to the same handling in Persist today;
// this helper only names the check, it does not change its precision.
func classifyUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
