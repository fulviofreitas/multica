package discord

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is Task Master task 8: the Discord user-binding token flow,
// mirroring telegram/binding.go on the generic channel_* tables with
// channel_type='discord': an unbound Discord user who DMs the bot gets a
// "link your account" prompt from replier.go's sendBindingPrompt, clicks
// through to the in-product redeem page, and their Discord user id is bound
// to their Multica account.
//
// The redeemer's Multica identity is ALWAYS taken from the authenticated
// session (the caller-supplied multicaUserID), never from the token's
// payload: a token that carried its own identity would let anyone who
// obtained (or intercepted) the bind link redeem it as whichever Multica
// user the token claimed, rather than as the person who is actually logged
// in when they click it.

// BindingTokenTTL bounds a token's life; the channel_binding_token CHECK
// (channel_binding_token_ttl_cap) enforces the same 15-minute cap.
const BindingTokenTTL = 15 * time.Minute

var (
	// ErrBindingTokenInvalid: token unknown / already consumed / expired /
	// minted for a different channel adapter. One opaque error avoids a
	// replay timing oracle and keeps a Telegram token from being usable here.
	ErrBindingTokenInvalid = errors.New("discord: binding token invalid or expired")
	// ErrBindingAlreadyAssigned: this Discord user id is already bound to a
	// different Multica user.
	ErrBindingAlreadyAssigned = errors.New("discord: user id is already bound to a different user")
	// ErrBindingNotWorkspaceMember: the redeemer is not a member of the
	// token's workspace.
	ErrBindingNotWorkspaceMember = errors.New("discord: redeemer is not a workspace member")
)

// RedeemedBindingToken is returned after a successful redemption.
type RedeemedBindingToken struct {
	WorkspaceID    pgtype.UUID
	InstallationID pgtype.UUID
	DiscordUserID  string
}

// BindingTokenService mints and redeems Discord binding tokens. Redemption
// is transactional: consuming the token and inserting the binding row commit
// together, so a failed bind (non-member, already-assigned, or any other
// error before Commit) never burns the token — the surrounding rollback
// undoes the ConsumeChannelBindingToken update, leaving the token available
// for the next attempt.
type BindingTokenService struct {
	q   *db.Queries
	tx  engine.TxStarter
	now func() time.Time
}

// NewBindingTokenService constructs the service.
func NewBindingTokenService(q *db.Queries, tx engine.TxStarter) *BindingTokenService {
	return &BindingTokenService{q: q, tx: tx, now: time.Now}
}

// Mint creates a single-use binding token for (installation, discordUserID).
// Only the token's SHA-256 hash is persisted; the raw value is returned
// exactly once so the caller (replier.go's sendBindingPrompt) can put it in
// the DM link — it is never stored or logged anywhere else.
func (s *BindingTokenService) Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, discordUserID string) (BindingToken, error) {
	raw, err := randomBindingToken(32)
	if err != nil {
		return BindingToken{}, fmt.Errorf("generate token: %w", err)
	}
	expiresAt := s.now().Add(BindingTokenTTL)
	if _, err := s.q.CreateChannelBindingToken(ctx, db.CreateChannelBindingTokenParams{
		TokenHash:      hashBindingToken(raw),
		WorkspaceID:    workspaceID,
		InstallationID: installationID,
		ChannelType:    string(TypeDiscord),
		ChannelUserID:  discordUserID,
		ExpiresAt:      pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return BindingToken{}, fmt.Errorf("persist token: %w", err)
	}
	return BindingToken{Raw: raw}, nil
}

// RedeemAndBind atomically consumes a raw token and binds the Discord user
// id it carries to multicaUserID (taken from the session, never from the
// token). See the file doc comment for why identity must come from the
// caller, not the token.
func (s *BindingTokenService) RedeemAndBind(ctx context.Context, raw string, multicaUserID pgtype.UUID) (RedeemedBindingToken, error) {
	if s.tx == nil {
		return RedeemedBindingToken{}, errors.New("discord: BindingTokenService missing TxStarter")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return RedeemedBindingToken{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	row, err := qtx.ConsumeChannelBindingToken(ctx, hashBindingToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RedeemedBindingToken{}, ErrBindingTokenInvalid
		}
		return RedeemedBindingToken{}, fmt.Errorf("consume token: %w", err)
	}
	if err := validateBindingTokenChannel(row); err != nil {
		// The token table is shared across channel adapters. Keep a token
		// minted by another adapter (e.g. Telegram) from being redeemed
		// through Discord; returning here rolls the consume back with the
		// surrounding transaction, so the deferred Rollback (not Commit) is
		// what actually happens.
		return RedeemedBindingToken{}, err
	}

	// Explicit membership gate (no member FK): returning before Commit rolls
	// the consume back, so a non-member's attempt does not burn the token.
	if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      multicaUserID,
		WorkspaceID: row.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RedeemedBindingToken{}, ErrBindingNotWorkspaceMember
		}
		return RedeemedBindingToken{}, fmt.Errorf("check membership: %w", err)
	}

	if _, err := qtx.CreateChannelUserBinding(ctx, db.CreateChannelUserBindingParams{
		WorkspaceID:    row.WorkspaceID,
		MulticaUserID:  multicaUserID,
		InstallationID: row.InstallationID,
		ChannelType:    string(TypeDiscord),
		ChannelUserID:  row.ChannelUserID,
		Config:         []byte(`{}`),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RedeemedBindingToken{}, ErrBindingAlreadyAssigned
		}
		return RedeemedBindingToken{}, fmt.Errorf("create binding: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RedeemedBindingToken{}, fmt.Errorf("commit: %w", err)
	}
	return RedeemedBindingToken{
		WorkspaceID:    row.WorkspaceID,
		InstallationID: row.InstallationID,
		DiscordUserID:  row.ChannelUserID,
	}, nil
}

func validateBindingTokenChannel(row db.ChannelBindingToken) error {
	if row.ChannelType != string(TypeDiscord) {
		return ErrBindingTokenInvalid
	}
	return nil
}

func randomBindingToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashBindingToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
