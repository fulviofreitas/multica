package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/discord"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ---- unconditional (no database) ------------------------------------------

func TestListDiscordInstallationsNotConfiguredReturnsEmpty(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/x/discord/installations", nil)
	w := httptest.NewRecorder()

	h.ListDiscordInstallations(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Installations []any `json:"installations"`
		Configured    bool  `json:"configured"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Configured || len(resp.Installations) != 0 {
		t.Fatalf("unexpected unconfigured response: %+v", resp)
	}
}

func TestDiscordMutationHandlersRejectUnconfiguredDeployment(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		run    func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{
			name:   "register",
			method: http.MethodPost,
			path:   "/api/workspaces/x/discord/install",
			body:   `{"bot_token":"placeholder","agent_id":"placeholder"}`,
			run:    (*Handler).RegisterDiscordBot,
		},
		{
			name:   "revoke",
			method: http.MethodDelete,
			path:   "/api/workspaces/x/discord/installations/y",
			run:    (*Handler).RevokeDiscordInstallation,
		},
		{
			name:   "redeem binding",
			method: http.MethodPost,
			path:   "/api/discord/binding/redeem",
			body:   `{"token":"placeholder"}`,
			run:    (*Handler).RedeemDiscordBindingToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{}
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			w := httptest.NewRecorder()

			tt.run(h, w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDiscordInstallationResponseNeverExposesStoredCredential(t *testing.T) {
	now := time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)
	row := db.ChannelInstallation{
		ID:              parseUUID("11111111-1111-1111-1111-111111111111"),
		WorkspaceID:     parseUUID("22222222-2222-2222-2222-222222222222"),
		AgentID:         parseUUID("33333333-3333-3333-3333-333333333333"),
		InstallerUserID: parseUUID("44444444-4444-4444-4444-444444444444"),
		Status:          "active",
		Config: json.RawMessage(
			`{"app_id":"123456789","bot_username":"my_test_bot","bot_token_encrypted":"ciphertext-sentinel"}`,
		),
		InstalledAt: pgtype.Timestamptz{Time: now, Valid: true},
		CreatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	}

	got := discordInstallationToResponse(row)
	if got.AppID != "123456789" || got.BotUsername != "my_test_bot" {
		t.Fatalf("public bot identity = %+v", got)
	}
	if got.InviteURL == "" || !strings.Contains(got.InviteURL, "123456789") {
		t.Fatalf("invite url = %q, want it to carry the app id", got.InviteURL)
	}
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(payload), "ciphertext-sentinel") ||
		strings.Contains(string(payload), "bot_token") {
		t.Fatalf("management response exposed stored credential: %s", payload)
	}
}

// discordInstallationToResponse must never fail the whole list on a
// malformed/legacy row with an empty app id: BuildInviteURL degrades to an
// empty string instead of the response construction erroring out.
func TestDiscordInstallationResponseToleratesEmptyAppID(t *testing.T) {
	now := time.Now()
	row := db.ChannelInstallation{
		ID:          parseUUID("11111111-1111-1111-1111-111111111111"),
		WorkspaceID: parseUUID("22222222-2222-2222-2222-222222222222"),
		AgentID:     parseUUID("33333333-3333-3333-3333-333333333333"),
		Status:      "active",
		Config:      json.RawMessage(`{}`),
		InstalledAt: pgtype.Timestamptz{Time: now, Valid: true},
		CreatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	}
	got := discordInstallationToResponse(row)
	if got.InviteURL != "" {
		t.Fatalf("expected empty invite url for empty app id, got %q", got.InviteURL)
	}
}

func TestRegisterDiscordBotRejectsMalformedBody(t *testing.T) {
	h := &Handler{DiscordInstall: &discord.InstallService{}}
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/x/discord/install", strings.NewReader("{not json"))
	req = testutil.WithURLParams(req, "id", "11111111-1111-1111-1111-111111111111")
	req.Header.Set("X-User-ID", "44444444-4444-4444-4444-444444444444")
	w := httptest.NewRecorder()

	h.RegisterDiscordBot(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRegisterDiscordBotRejectsMalformedAgentID(t *testing.T) {
	h := &Handler{DiscordInstall: &discord.InstallService{}}
	req := testutil.JSONRequest(http.MethodPost, "/api/workspaces/x/discord/install", map[string]any{
		"bot_token": "placeholder",
		"agent_id":  "not-a-uuid",
	})
	req = testutil.WithURLParams(req, "id", "11111111-1111-1111-1111-111111111111")
	req.Header.Set("X-User-ID", "44444444-4444-4444-4444-444444444444")
	w := httptest.NewRecorder()

	h.RegisterDiscordBot(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRevokeDiscordInstallationRejectsMalformedInstallationID(t *testing.T) {
	h := &Handler{DiscordInstall: &discord.InstallService{}}
	req := httptest.NewRequest(http.MethodDelete, "/api/workspaces/x/discord/installations/not-a-uuid", nil)
	req = testutil.WithURLParams(req, "id", "11111111-1111-1111-1111-111111111111", "installationId", "not-a-uuid")
	req.Header.Set("X-User-ID", "44444444-4444-4444-4444-444444444444")
	w := httptest.NewRecorder()

	h.RevokeDiscordInstallation(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestListDiscordInstallationsRejectsMalformedWorkspaceID(t *testing.T) {
	h := &Handler{DiscordInstall: &discord.InstallService{}}
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/x/discord/installations", nil)
	req = testutil.WithURLParams(req, "id", "not-a-uuid")
	w := httptest.NewRecorder()

	h.ListDiscordInstallations(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestDiscordInstallErrorResponse exhaustively covers
// discordInstallErrorResponse — the pure error->(status, message) mapping
// RegisterDiscordBot uses — so every distinct failure class is proven to
// produce a distinct, actionable response without needing a database or a
// live Discord API call. Precedent for each row is telegram.go's
// RegisterTelegramBot switch (see that file); Discord has no webhook
// concept so there is no ErrWebhookConfigured row.
func TestDiscordInstallErrorResponse(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"invalid token shape", discord.ErrInvalidBotToken, http.StatusBadRequest},
		{"credentials rejected", discord.ErrCredentialsRejected, http.StatusBadRequest},
		{"credentials unverifiable", discord.ErrCredentialsUnverifiable, http.StatusServiceUnavailable},
		{"owned by same workspace", discord.ErrBotOwnedBySameWorkspace, http.StatusConflict},
		{"owned by archived agent", discord.ErrBotOwnedByArchivedAgent, http.StatusConflict},
		{"owned by another workspace", discord.ErrBotOwnedByAnotherWorkspace, http.StatusConflict},
		{"unknown error", errors.New("boom"), http.StatusInternalServerError},
	}
	seenMessages := map[string]string{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, message := discordInstallErrorResponse(tc.err)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if message == "" {
				t.Fatal("message must not be empty")
			}
			// Every distinct failure class must produce a distinct message —
			// a user who pasted a bad token and a user whose bot is owned by
			// another workspace must not see the same text.
			if prior, ok := seenMessages[message]; ok {
				t.Fatalf("message %q reused between %q and %q", message, prior, tc.name)
			}
			seenMessages[message] = tc.name
		})
	}
}

func TestDiscordInstallErrorResponseUnwrapsWrappedSentinels(t *testing.T) {
	wrapped := errors.New("discord users/@me: " + discord.ErrCredentialsRejected.Error())
	// A wrapped, non-errors.Is-compatible error must not be silently
	// mis-mapped: only fmt.Errorf("%w: ...", sentinel)-style wraps unwrap.
	status, _ := discordInstallErrorResponse(wrapped)
	if status != http.StatusInternalServerError {
		t.Fatalf("a same-text-but-not-wrapped error must fall through to the default mapping, got %d", status)
	}
}

// ---- DB-backed (skip without DATABASE_URL) --------------------------------

func wireDiscordInstallService(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	service, err := discord.NewInstallService(box, nil, nil)
	if err != nil {
		t.Fatalf("discord.NewInstallService: %v", err)
	}
	previous := testHandler.DiscordInstall
	testHandler.DiscordInstall = service
	t.Cleanup(func() { testHandler.DiscordInstall = previous })
}

func discordInstallationFixture(t *testing.T, agentID, appID string) string {
	t.Helper()
	return dbfx.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id":      testWorkspaceID,
		"agent_id":          agentID,
		"channel_type":      "discord",
		"config":            []byte(`{"app_id":"` + appID + `","bot_username":"handler-test-bot","bot_token_encrypted":"cipher"}`),
		"installer_user_id": testUserID,
		"status":            "active",
	})
}

func TestListDiscordInstallationsDB_Empty(t *testing.T) {
	wireDiscordInstallService(t)
	agentID := dbfx.Agent(t, "discord-list-empty-agent", "")
	_ = agentID // agent exists but no installation was created for it

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/x/discord/installations", nil)
	req = testutil.WithURLParams(req, "id", testWorkspaceID)
	resp := testutil.Call(t, testHandler.ListDiscordInstallations, req).Want(http.StatusOK)

	var out struct {
		Installations []DiscordInstallationResponse `json:"installations"`
		Configured    bool                          `json:"configured"`
	}
	resp.JSON(&out)
	if !out.Configured {
		t.Fatal("expected configured=true when DiscordInstall is wired")
	}
	for _, inst := range out.Installations {
		if inst.AgentID == agentID {
			t.Fatalf("unexpected installation for agent with none created: %+v", inst)
		}
	}
}

func TestListDiscordInstallationsDB_Populated(t *testing.T) {
	wireDiscordInstallService(t)
	agentID := dbfx.Agent(t, "discord-list-populated-agent", "")
	instID := discordInstallationFixture(t, agentID, "discord-list-populated-app")

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/x/discord/installations", nil)
	req = testutil.WithURLParams(req, "id", testWorkspaceID)
	resp := testutil.Call(t, testHandler.ListDiscordInstallations, req).Want(http.StatusOK)

	var out struct {
		Installations []DiscordInstallationResponse `json:"installations"`
		Configured    bool                          `json:"configured"`
	}
	resp.JSON(&out)
	if !out.Configured {
		t.Fatal("expected configured=true")
	}
	var found *DiscordInstallationResponse
	for i := range out.Installations {
		if out.Installations[i].ID == instID {
			found = &out.Installations[i]
		}
	}
	if found == nil {
		t.Fatalf("fixture installation %s not found in %+v", instID, out.Installations)
	}
	if found.AppID != "discord-list-populated-app" || found.BotUsername != "handler-test-bot" {
		t.Fatalf("unexpected fields: %+v", found)
	}
	if found.InviteURL == "" {
		t.Fatalf("expected a non-empty invite url: %+v", found)
	}
	body := resp.Text()
	if strings.Contains(body, "cipher") || strings.Contains(body, "bot_token") {
		t.Fatalf("list response leaked token material: %s", body)
	}
}

func TestRegisterDiscordBotDB_MalformedTokenShapeIsBadRequest(t *testing.T) {
	wireDiscordInstallService(t)
	agentID := dbfx.Agent(t, "discord-install-bad-token-agent", "")

	req := testutil.JSONRequest(http.MethodPost, "/api/workspaces/x/discord/install", map[string]any{
		"bot_token": "not-a-real-token",
		"agent_id":  agentID,
	})
	req = testutil.WithURLParams(req, "id", testWorkspaceID)
	req.Header.Set("X-User-ID", testUserID)

	testutil.Call(t, testHandler.RegisterDiscordBot, req).Want(http.StatusBadRequest)
}

func TestRegisterDiscordBotDB_AgentNotInWorkspaceIsNotFound(t *testing.T) {
	wireDiscordInstallService(t)

	req := testutil.JSONRequest(http.MethodPost, "/api/workspaces/x/discord/install", map[string]any{
		"bot_token": "placeholder",
		"agent_id":  "99999999-9999-9999-9999-999999999999",
	})
	req = testutil.WithURLParams(req, "id", testWorkspaceID)
	req.Header.Set("X-User-ID", testUserID)

	testutil.Call(t, testHandler.RegisterDiscordBot, req).Want(http.StatusNotFound)
}

func TestRevokeDiscordInstallationDB_Success(t *testing.T) {
	wireDiscordInstallService(t)
	agentID := dbfx.Agent(t, "discord-revoke-agent", "")
	instID := discordInstallationFixture(t, agentID, "discord-revoke-app")

	req := httptest.NewRequest(http.MethodDelete, "/api/workspaces/x/discord/installations/"+instID, nil)
	req = testutil.WithURLParams(req, "id", testWorkspaceID, "installationId", instID)
	req.Header.Set("X-User-ID", testUserID)

	testutil.Call(t, testHandler.RevokeDiscordInstallation, req).Want(http.StatusNoContent)

	var status string
	if err := testPool.QueryRow(req.Context(), `SELECT status FROM channel_installation WHERE id = $1`, instID).Scan(&status); err != nil {
		t.Fatalf("read back installation status: %v", err)
	}
	if status != "revoked" {
		t.Fatalf("status = %q, want revoked", status)
	}
}

func TestRevokeDiscordInstallationDB_NotFound(t *testing.T) {
	wireDiscordInstallService(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/workspaces/x/discord/installations/99999999-9999-9999-9999-999999999999", nil)
	req = testutil.WithURLParams(req, "id", testWorkspaceID, "installationId", "99999999-9999-9999-9999-999999999999")
	req.Header.Set("X-User-ID", testUserID)

	testutil.Call(t, testHandler.RevokeDiscordInstallation, req).Want(http.StatusNotFound)
}

func TestRedeemDiscordBindingTokenRejectsMissingToken(t *testing.T) {
	h := &Handler{DiscordBindingTokens: &discord.BindingTokenService{}}
	req := testutil.JSONRequest(http.MethodPost, "/api/discord/binding/redeem", map[string]any{"token": ""})
	req.Header.Set("X-User-ID", "44444444-4444-4444-4444-444444444444")
	w := httptest.NewRecorder()

	h.RedeemDiscordBindingToken(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRedeemDiscordBindingTokenRejectsMalformedBody(t *testing.T) {
	h := &Handler{DiscordBindingTokens: &discord.BindingTokenService{}}
	req := httptest.NewRequest(http.MethodPost, "/api/discord/binding/redeem", strings.NewReader("{not json"))
	req.Header.Set("X-User-ID", "44444444-4444-4444-4444-444444444444")
	w := httptest.NewRecorder()

	h.RedeemDiscordBindingToken(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// ---- redeem, DB-backed (skip without DATABASE_URL) ------------------------

func wireDiscordBindingTokens(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	previous := testHandler.DiscordBindingTokens
	testHandler.DiscordBindingTokens = discord.NewBindingTokenService(testHandler.Queries, testPool)
	t.Cleanup(func() { testHandler.DiscordBindingTokens = previous })
}

// TestRedeemDiscordBindingTokenDB_HappyPath exercises the full HTTP path:
// mint a token directly against the fixture workspace (mirroring what
// replier.go's sendBindingPrompt would trigger), then redeem it as the
// fixture user, who is a member of that workspace.
func TestRedeemDiscordBindingTokenDB_HappyPath(t *testing.T) {
	wireDiscordBindingTokens(t)
	agentID := dbfx.Agent(t, "discord-redeem-agent", "")
	installationID := discordInstallationFixture(t, agentID, "discord-redeem-app")
	token, err := testHandler.DiscordBindingTokens.Mint(t.Context(), parseUUID(testWorkspaceID), parseUUID(installationID), "discord-user-happy-path")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := testutil.JSONRequest(http.MethodPost, "/api/discord/binding/redeem", map[string]any{"token": token.Raw})
	req.Header.Set("X-User-ID", testUserID)
	resp := testutil.Call(t, testHandler.RedeemDiscordBindingToken, req).Want(http.StatusOK)

	var out RedeemDiscordBindingTokenResponse
	resp.JSON(&out)
	if out.WorkspaceID != testWorkspaceID || out.DiscordUserID != "discord-user-happy-path" {
		t.Fatalf("unexpected response: %+v", out)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(req.Context(), `DELETE FROM channel_user_binding WHERE channel_user_id = $1`, "discord-user-happy-path")
	})
}

func TestRedeemDiscordBindingTokenDB_UnknownTokenIsGone(t *testing.T) {
	wireDiscordBindingTokens(t)

	req := testutil.JSONRequest(http.MethodPost, "/api/discord/binding/redeem", map[string]any{"token": "no-such-token"})
	req.Header.Set("X-User-ID", testUserID)

	testutil.Call(t, testHandler.RedeemDiscordBindingToken, req).Want(http.StatusGone)
}
