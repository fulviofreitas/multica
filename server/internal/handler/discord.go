package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/integrations/discord"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This file is subtask 4.4: the HTTP surface over the Discord BYO-bot
// lifecycle built by install_service.go (live verify + at-rest encryption),
// persist.go (the install transaction + ownership-conflict classification)
// and config.go/invite.go (public config + invite URL) in
// internal/integrations/discord. It deliberately mirrors telegram.go
// (list / register / revoke) field-for-field — see that file's doc comments
// for the "why" behind each shape; this file only calls out where Discord
// differs.
//
// Unlike Telegram's InstallService, discord.InstallService does not hold a
// *db.Queries or tx starter (see install_service.go's doc comment on why:
// it is scoped to encryption + live verification only). So list/get/revoke
// here go straight through h.Queries using the generic
// channel_installation queries, scoped by channel_type=discord.TypeDiscord —
// the same rows telegram.InstallService.ListByWorkspace/GetInWorkspace/
// Revoke wrap for Telegram. Install still goes through the discord package's
// Register(ctx, svc, queries, txStarter, params), passing h.Queries (wrapped
// by discord.NewPersistQueries) and h.TxStarter explicitly, per Register's
// signature.

// DiscordInstallationResponse is the wire shape for a Discord installation
// row. The encrypted bot token in config is INTENTIONALLY absent — it is
// server-internal. WS lease columns are runtime state, not API surface.
type DiscordInstallationResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	AppID           string `json:"app_id"`
	BotUsername     string `json:"bot_username"`
	InstallerUserID string `json:"installer_user_id"`
	Status          string `json:"status"`
	InstalledAt     string `json:"installed_at"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	// InviteURL is the Discord OAuth2 "add bot to server" link
	// (discord.BuildInviteURL). Empty when the stored app id is empty — a
	// row this defensive/never-should-happen case still renders in the
	// list rather than being dropped.
	InviteURL string `json:"invite_url"`
}

func discordInstallationToResponse(row db.ChannelInstallation) DiscordInstallationResponse {
	info := discord.DecodePublicConfig(row.Config)
	// BuildInviteURL only fails on an empty app id, which a real install
	// never produces (VerifyBotToken requires a live bot identity before
	// PrepareInstall assembles the config). Degrade to an empty invite URL
	// rather than failing the whole list for one malformed row.
	inviteURL, _ := discord.BuildInviteURL(info.BotID)
	return DiscordInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		AppID:           info.BotID,
		BotUsername:     info.BotUsername,
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
		InviteURL:       inviteURL,
	}
}

// ListDiscordInstallations (GET /api/workspaces/{id}/discord/installations)
// is member-visible so the Integrations tab renders for non-admins.
// configured=false when MULTICA_DISCORD_SECRET_KEY is not set, so the UI
// renders a disabled state instead of erroring — mirrors Telegram.
func (h *Handler) ListDiscordInstallations(w http.ResponseWriter, r *http.Request) {
	if h.DiscordInstall == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations": []DiscordInstallationResponse{},
			"configured":    false,
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListChannelInstallationsByWorkspace(r.Context(), db.ListChannelInstallationsByWorkspaceParams{
		WorkspaceID: wsUUID,
		ChannelType: string(discord.TypeDiscord),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list discord installations")
		return
	}
	out := make([]DiscordInstallationResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, discordInstallationToResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations": out,
		"configured":    true,
	})
}

// RegisterDiscordRequest is the body for a bot install: the token pasted
// from the Discord developer portal's Bot page, and the agent it is
// installed for.
type RegisterDiscordRequest struct {
	BotToken string `json:"bot_token"`
	AgentID  string `json:"agent_id"`
}

// discordInstallErrorResponse is the pure error->(status, message) mapping
// discord.Register can fail with. Split out from RegisterDiscordBot so the
// full mapping is testable without a database — see
// TestDiscordInstallErrorResponse in discord_test.go. Precedent for every
// row is telegram.go's RegisterTelegramBot switch; Discord has no webhook
// concept, so telegram.ErrWebhookConfigured has no counterpart here.
func discordInstallErrorResponse(err error) (status int, message string) {
	switch {
	case errors.Is(err, discord.ErrInvalidBotToken):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, discord.ErrCredentialsRejected):
		return http.StatusBadRequest, "Discord rejected this bot token — generate a current token in the Discord developer portal and try again"
	case errors.Is(err, discord.ErrCredentialsUnverifiable):
		return http.StatusServiceUnavailable, "could not reach Discord to verify this bot — check the server network or proxy and try again; the token was not saved"
	case errors.Is(err, discord.ErrBotOwnedBySameWorkspace):
		return http.StatusConflict, "this Discord bot is already connected to another agent in this workspace — disconnect it there first, then connect it here"
	case errors.Is(err, discord.ErrBotOwnedByArchivedAgent):
		return http.StatusConflict, "this Discord bot is connected to an archived agent in this workspace — restore that agent, or disconnect its bot, before connecting it here"
	case errors.Is(err, discord.ErrBotOwnedByAnotherWorkspace):
		return http.StatusConflict, "this Discord bot is already connected to a different Multica workspace — disconnect it there before connecting it here"
	default:
		return http.StatusInternalServerError, "could not save this Discord bot — something went wrong on the server; the token was not saved"
	}
}

// RegisterDiscordBot (POST /api/workspaces/{id}/discord/install) installs a
// user-supplied Discord bot for an agent. Admin-only at the router. Mirrors
// RegisterTelegramBot, except agent_id travels in the body (there is no
// OAuth redirect step to thread it through a query param for).
func (h *Handler) RegisterDiscordBot(w http.ResponseWriter, r *http.Request) {
	if h.DiscordInstall == nil {
		writeError(w, http.StatusServiceUnavailable, "discord integration not enabled")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	var body RegisterDiscordRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, body.AgentID, "agent_id")
	if !ok {
		return
	}
	// Ownership pre-check at the boundary so a wrong agent_id is a clear 404.
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	row, err := discord.Register(r.Context(), h.DiscordInstall, discord.NewPersistQueries(h.Queries), h.TxStarter, discord.RegisterParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
		InstallerID: initiatorUUID,
		BotToken:    body.BotToken,
	})
	if err != nil {
		status, message := discordInstallErrorResponse(err)
		writeError(w, status, message)
		return
	}
	// Broadcast so every open client invalidates its installations query and
	// shows the new bot — matching the Telegram install semantics.
	h.publish(protocol.EventDiscordInstallationCreated, uuidToString(row.WorkspaceID), "user", userID, map[string]any{
		"id": uuidToString(row.ID),
	})
	writeJSON(w, http.StatusOK, discordInstallationToResponse(row))
}

// RevokeDiscordInstallation
// (DELETE /api/workspaces/{id}/discord/installations/{installationId})
// flips status to 'revoked'. Admin-only at the router. The row is preserved
// for audit; a re-install (re-pasting the bot's token) flips status back to
// 'active'. Mirrors RevokeTelegramInstallation.
func (h *Handler) RevokeDiscordInstallation(w http.ResponseWriter, r *http.Request) {
	if h.DiscordInstall == nil {
		writeError(w, http.StatusServiceUnavailable, "discord integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	// installationId is a pure UUID from the request boundary (CLAUDE.md's
	// Backend UUID Rules): parseUUIDOrBadRequest, never the panicking
	// parseUUID.
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	// Workspace-scoped lookup so one workspace cannot revoke another's
	// installation by guessing the UUID.
	if _, err := h.Queries.GetChannelInstallationInWorkspace(r.Context(), db.GetChannelInstallationInWorkspaceParams{
		ID:          instUUID,
		WorkspaceID: wsUUID,
		ChannelType: string(discord.TypeDiscord),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "discord installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	if err := h.Queries.SetChannelInstallationStatus(r.Context(), db.SetChannelInstallationStatusParams{
		ID:     instUUID,
		Status: "revoked",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	h.publish(protocol.EventDiscordInstallationRevoked, uuidToString(wsUUID), "user", userID, map[string]any{
		"id": uuidToString(instUUID),
	})
	w.WriteHeader(http.StatusNoContent)
}
