package handler

import (
	"context"
	"net/http"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func setRuntimeTestMemberRole(t *testing.T, userID, role string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		UPDATE member
		SET role = $3
		WHERE workspace_id = $1 AND user_id = $2
	`, testWorkspaceID, userID, role); err != nil {
		t.Fatalf("set runtime test member role to %s: %v", role, err)
	}
	if testHandler.MembershipCache != nil {
		testHandler.MembershipCache.Invalidate(context.Background(), userID, testWorkspaceID)
	}
}

func promoteRuntimeTestMemberToAdmin(t *testing.T, userID string) {
	t.Helper()
	setRuntimeTestMemberRole(t, userID, "admin")
}

func TestInitiateUpdateRequiresRuntimeManager(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	tests := []struct {
		name       string
		actor      string
		wantStatus int
	}{
		{name: "runtime owner", actor: "runtime_owner", wantStatus: http.StatusOK},
		{name: "workspace admin on another private runtime", actor: "workspace_admin", wantStatus: http.StatusNotFound},
		{name: "plain member on another private runtime", actor: "plain_member", wantStatus: http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runtimeID, runtimeOwnerID, plainMemberID := runtimeVisibilityFixture(t)
			actorID := runtimeOwnerID
			switch tc.actor {
			case "workspace_admin":
				promoteRuntimeTestMemberToAdmin(t, plainMemberID)
				actorID = plainMemberID
			case "plain_member":
				actorID = plainMemberID
			}

			req := withURLParam(
				newRequestAs(actorID, http.MethodPost, "/api/runtimes/"+runtimeID+"/update", map[string]any{
					"target_version": "v9.9.9",
				}),
				"runtimeId",
				runtimeID,
			)
			testutil.Call(t, testHandler.InitiateUpdate, req).Want(tc.wantStatus)

			hasPending, err := testHandler.UpdateStore.HasPending(context.Background(), runtimeID)
			if err != nil {
				t.Fatalf("check pending update request: %v", err)
			}
			wantPending := tc.wantStatus == http.StatusOK
			if hasPending != wantPending {
				t.Fatalf("pending update request = %v; want %v", hasPending, wantPending)
			}
		})
	}
}

func TestGetUpdateRequiresRuntimeManager(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	tests := []struct {
		name       string
		actor      string
		wantStatus int
	}{
		{name: "runtime owner", actor: "runtime_owner", wantStatus: http.StatusOK},
		{name: "workspace admin on another private runtime", actor: "workspace_admin", wantStatus: http.StatusNotFound},
		{name: "plain member on another private runtime", actor: "plain_member", wantStatus: http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runtimeID, runtimeOwnerID, plainMemberID := runtimeVisibilityFixture(t)
			actorID := runtimeOwnerID
			switch tc.actor {
			case "workspace_admin":
				promoteRuntimeTestMemberToAdmin(t, plainMemberID)
				actorID = plainMemberID
			case "plain_member":
				actorID = plainMemberID
			}

			update, err := testHandler.UpdateStore.Create(context.Background(), runtimeID, "v9.9.9", runtimeOwnerID)
			if err != nil {
				t.Fatalf("create update request: %v", err)
			}

			req := withURLParams(
				newRequestAs(actorID, http.MethodGet, "/api/runtimes/"+runtimeID+"/update/"+update.ID, nil),
				"runtimeId", runtimeID,
				"updateId", update.ID,
			)
			testutil.Call(t, testHandler.GetUpdate, req).Want(tc.wantStatus)
		})
	}
}

func TestGetUpdateAllowsInitiatorAfterAdminDemotionOnPublicRuntime(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	runtimeID, _, adminID := runtimeVisibilityFixture(t)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent_runtime
		SET visibility = 'public'
		WHERE id = $1
	`, runtimeID); err != nil {
		t.Fatalf("make runtime public: %v", err)
	}
	promoteRuntimeTestMemberToAdmin(t, adminID)

	initRequest := withURLParam(
		newRequestAs(adminID, http.MethodPost, "/api/runtimes/"+runtimeID+"/update", map[string]any{
			"target_version": "v9.9.9",
		}),
		"runtimeId",
		runtimeID,
	)
	initRecorder := testutil.Call(t, testHandler.InitiateUpdate, initRequest).Want(http.StatusOK)

	var update UpdateRequest
	initRecorder.JSON(&update)
	if update.InitiatorUserID != "" {
		t.Fatalf("initiator user ID leaked in API response: %q", update.InitiatorUserID)
	}
	setRuntimeTestMemberRole(t, adminID, "member")

	pollRequest := withURLParams(
		newRequestAs(adminID, http.MethodGet, "/api/runtimes/"+runtimeID+"/update/"+update.ID, nil),
		"runtimeId", runtimeID,
		"updateId", update.ID,
	)
	testutil.Call(t, testHandler.GetUpdate, pollRequest).Want(http.StatusOK)
}
