package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	testutil "github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func notificationPreferenceRequest(
	t *testing.T,
	method string,
	preferences map[string]string,
) *http.Request {
	t.Helper()

	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(
		context.Background(),
		db.GetMemberByUserAndWorkspaceParams{
			UserID:      parseUUID(testUserID),
			WorkspaceID: parseUUID(testWorkspaceID),
		},
	)
	if err != nil {
		t.Fatalf("load test member: %v", err)
	}

	req := newRequest(method, "/api/notification-preferences", map[string]any{
		"preferences": preferences,
	})
	return req.WithContext(
		middleware.SetMemberContext(req.Context(), testWorkspaceID, member),
	)
}

func TestPatchNotificationPreferencesMergesWithoutReplacing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `
		DELETE FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID); err != nil {
		t.Fatalf("reset notification preference: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	})

	testutil.Call(t, testHandler.UpdateNotificationPreferences, notificationPreferenceRequest(t, http.MethodPut, map[string]string{
		"status_changes": "muted",
	})).Want(http.StatusOK)

	patchRecorder := testutil.Call(t, testHandler.PatchNotificationPreferences, notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
		"comments": "muted",
	})).Want(http.StatusOK)

	var response struct {
		WorkspaceID string            `json:"workspace_id"`
		Preferences map[string]string `json:"preferences"`
	}
	patchRecorder.Decode(&response)
	if response.WorkspaceID != testWorkspaceID {
		t.Fatalf("workspace_id = %q, want %q", response.WorkspaceID, testWorkspaceID)
	}
	if response.Preferences["status_changes"] != "muted" {
		t.Fatalf("status_changes was replaced: %#v", response.Preferences)
	}
	if response.Preferences["comments"] != "muted" {
		t.Fatalf("comments patch missing: %#v", response.Preferences)
	}

	testutil.Call(t, testHandler.PatchNotificationPreferences, notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
		"status_changes": "all",
	})).Want(http.StatusOK)

	var persisted []byte
	if err := testPool.QueryRow(ctx, `
		SELECT preferences
		FROM notification_preference
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, testUserID).Scan(&persisted); err != nil {
		t.Fatalf("load persisted preference: %v", err)
	}
	var preferences map[string]string
	if err := json.Unmarshal(persisted, &preferences); err != nil {
		t.Fatalf("decode persisted preference: %v", err)
	}
	if preferences["status_changes"] != "all" || preferences["comments"] != "muted" {
		t.Fatalf("unexpected persisted preferences: %#v", preferences)
	}
}

// TestPatchNotificationPreferencesAcceptsMentions covers the group added by
// #6468. The whitelist rejects unknown keys, so a client shipped ahead of the
// server would 400 on this key — the reason the server must deploy first.
func TestPatchNotificationPreferencesAcceptsMentions(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM notification_preference
			WHERE workspace_id = $1 AND user_id = $2
		`, testWorkspaceID, testUserID)
	})

	recorder := testutil.Call(t, testHandler.PatchNotificationPreferences, notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
		"mentions": "muted",
	})).Want(http.StatusOK)

	var response struct {
		Preferences map[string]string `json:"preferences"`
	}
	recorder.Decode(&response)
	if response.Preferences["mentions"] != "muted" {
		t.Fatalf("mentions not persisted: %#v", response.Preferences)
	}
}

func TestPatchNotificationPreferencesRejectsUnknownGroups(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	testutil.Call(t, testHandler.PatchNotificationPreferences, notificationPreferenceRequest(t, http.MethodPatch, map[string]string{
		"unknown_group": "muted",
	})).Want(http.StatusBadRequest)
}
