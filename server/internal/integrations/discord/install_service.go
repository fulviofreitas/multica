package discord

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// This file is the Discord install-time encryption step, mirroring
// telegram.InstallService: given a pasted bot token, validate it live
// (install.go's VerifyBotToken) and seal it with secretbox before it is
// ever assembled into the JSON blob that gets written to
// channel_installation.config. Everything after "assemble the config" —
// the install transaction, the routing-slot upsert, and the 23505
// ownership-conflict classification (ErrBotOwnedByAnotherWorkspace /
// ErrBotOwnedBySameWorkspace / ErrBotOwnedByArchivedAgent in Telegram's
// vocabulary) — is a later subtask; see PrepareInstall's doc comment for
// the exact seam.

// InstallService owns the at-rest encryption of a pasted Discord bot token
// and the live verification step. The box MUST be non-nil: an installation
// whose token is stored unencrypted is a security defect, not a dev
// convenience, so construction is refused rather than silently falling
// back to plaintext storage — mirrors
// telegram.NewInstallService/newInstallService's refusal.
type InstallService struct {
	box        *secretbox.Box
	httpClient *http.Client
	logger     *slog.Logger

	// apiBase overrides the Discord REST API host for the live
	// verification call. Empty uses the production host
	// (install.go's defaultAPIBase). Tests point it at an httptest
	// server; there is no exported setter because production callers
	// never need one.
	apiBase string
}

// NewInstallService binds the service to an encryption box and an optional
// http.Client override (nil uses install.go's bounded-timeout default,
// newCredentialVerificationClient).
func NewInstallService(box *secretbox.Box, httpClient *http.Client, logger *slog.Logger) (*InstallService, error) {
	if box == nil {
		return nil, errors.New("discord: InstallService requires a non-nil secretbox.Box")
	}
	if httpClient == nil {
		httpClient = newCredentialVerificationClient()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &InstallService{box: box, httpClient: httpClient, logger: logger}, nil
}

// PreparedInstall is the outcome of validating and encrypting a pasted bot
// token: the installConfig ready to be JSON-marshalled into
// channel_installation.config, plus the fields the persist step (subtask
// 4.3) needs for the routing-slot upsert without re-parsing the config
// JSON it just received.
type PreparedInstall struct {
	// Config is the fully assembled, ready-to-persist installation config.
	// BotTokenEncrypted is base64-encoded secretbox ciphertext; the
	// plaintext token does not appear anywhere else in this struct.
	Config installConfig
	// AppIDKey is the Discord application/bot id — the value that fills
	// the generic (channel_type, config->>'app_id') routing-slot unique
	// index. Equal to Config.AppID; surfaced separately so the persist
	// step does not need to re-derive it from the JSON it is about to
	// write.
	AppIDKey string
}

// PrepareInstall validates a pasted Discord bot token live against the
// Discord API (VerifyBotToken in install.go), then encrypts it at rest and
// assembles the installConfig subtask 4.3 persists. It does not touch the
// database, and it is the only place in this package that populates
// installConfig.BotTokenEncrypted outside of tests — no caller can produce
// a config with a plaintext token by going through this method.
//
// The plaintext token is never returned in an error and never logged: the
// returned error is either VerifyBotToken's sentinel-wrapped classification
// (which only ever echoes Discord's HTTP status/body, not the request
// Authorization header) or a fixed encryption-failure message with no
// token interpolation.
//
// SEAM FOR SUBTASK 4.3: call this, then persist the resulting
// PreparedInstall by JSON-marshalling PreparedInstall.Config and running
// the shared install transaction — begin tx, reclaim any DEAD prior owner
// of the (channel_type, app_id) routing slot, upsert the installation
// keyed by (workspace, agent, channel_type), and on a 23505 unique
// violation classify the live owner (same workspace / different workspace
// / archived agent) via a GetChannelInstallationOwnerByAppID-shaped query —
// mirroring telegram.InstallService.persistInstall and
// liveOwnerConflictErr in server/internal/integrations/telegram/install.go.
// This method intentionally stops before that transaction; it does not
// hold a *db.Queries or tx starter because this subtask does not implement
// persistence.
func (s *InstallService) PrepareInstall(ctx context.Context, rawToken string) (PreparedInstall, error) {
	token := strings.TrimSpace(rawToken)

	verified, err := VerifyBotToken(ctx, token, s.apiBase, s.httpClient)
	if err != nil {
		return PreparedInstall{}, err
	}

	sealed, err := s.box.Seal([]byte(token))
	if err != nil {
		// Deliberately no %v/%w of the underlying error's context beyond a
		// fixed message: secretbox.Seal's only failure mode is a nonce
		// read failure, which carries no token material, but keeping the
		// message fixed here avoids ever having to reason about what a
		// future Seal error might embed.
		return PreparedInstall{}, errors.New("discord: encrypt bot token failed")
	}

	cfg := installConfig{
		AppID:             verified.AppID,
		BotUsername:       verified.Username,
		BotTokenEncrypted: base64.StdEncoding.EncodeToString(sealed),
	}
	return PreparedInstall{Config: cfg, AppIDKey: verified.AppID}, nil
}
