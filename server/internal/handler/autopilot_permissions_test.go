package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// createPlainMember adds a fresh member-role user to the test workspace and
// returns its user id. Used to exercise the autopilot write gate from the
// perspective of a member who is neither the creator nor a workspace admin.
func createPlainMember(t *testing.T, email string) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO "user" (name, email) VALUES ('AP Perm Member', $1) RETURNING id`,
		email,
	).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, userID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})

	if _, err := testPool.Exec(ctx,
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`,
		testWorkspaceID, userID,
	); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return userID
}

// createAutopilotAs creates an autopilot via the API as the given user (empty
// userID = workspace owner) assigned to a fresh public agent, and returns its
// id. The caller-supplied title prefix keeps cleanup queries unambiguous.
func createAutopilotAs(t *testing.T, userID, title string) string {
	t.Helper()
	agentID := createHandlerTestAgent(t, title+"-agent", nil)

	body := map[string]any{
		"title":          title,
		"assignee_id":    agentID,
		"execution_mode": "create_issue",
	}

	path := "/api/autopilots?workspace_id=" + testWorkspaceID
	var r *http.Request
	if userID == "" {
		r = newRequest("POST", path, body)
	} else {
		r = newRequestAs(userID, "POST", path, body)
	}
	w := testutil.Call(t, testHandler.CreateAutopilot, r).Want(http.StatusCreated)
	var ap AutopilotResponse
	w.Decode(&ap)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM autopilot_run WHERE autopilot_id = $1`, ap.ID)
		testPool.Exec(context.Background(), `DELETE FROM autopilot_trigger WHERE autopilot_id = $1`, ap.ID)
		testPool.Exec(context.Background(), `DELETE FROM autopilot_collaborator WHERE autopilot_id = $1`, ap.ID)
		testPool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, ap.ID)
	})
	return ap.ID
}

// grantAutopilotAccess grants the target member write access via the API as the
// given caller (empty caller = workspace owner), asserting the expected status.
func grantAutopilotAccess(t *testing.T, caller, apID, targetUserID string, wantStatus int) {
	t.Helper()

	path := "/api/autopilots/" + apID + "/collaborators?workspace_id=" + testWorkspaceID
	body := map[string]any{"user_id": targetUserID}
	var r *http.Request
	if caller == "" {
		r = newRequest("POST", path, body)
	} else {
		r = newRequestAs(caller, "POST", path, body)
	}
	r = withURLParam(r, "id", apID)
	testutil.Call(t, testHandler.AddAutopilotCollaborator, r).Want(wantStatus)
}

// autopilotCanWrite fetches the detail as the given caller and returns the
// can_write flag the server stamped for them.
func autopilotCanWrite(t *testing.T, caller, apID string) bool {
	t.Helper()

	path := "/api/autopilots/" + apID + "?workspace_id=" + testWorkspaceID
	var r *http.Request
	if caller == "" {
		r = newRequest("GET", path, nil)
	} else {
		r = newRequestAs(caller, "GET", path, nil)
	}
	r = withURLParam(r, "id", apID)
	w := testutil.Call(t, testHandler.GetAutopilot, r).Want(http.StatusOK)
	var got struct {
		Autopilot AutopilotResponse `json:"autopilot"`
	}
	w.Decode(&got)
	if got.Autopilot.CanWrite == nil {
		t.Fatalf("expected can_write to be set on detail response")
	}
	return *got.Autopilot.CanWrite
}

// autopilotCanManageAccess fetches the detail as the given caller and returns
// the can_manage_access flag (narrower than can_write — collaborators lack it).
func autopilotCanManageAccess(t *testing.T, caller, apID string) bool {
	t.Helper()

	path := "/api/autopilots/" + apID + "?workspace_id=" + testWorkspaceID
	var r *http.Request
	if caller == "" {
		r = newRequest("GET", path, nil)
	} else {
		r = newRequestAs(caller, "GET", path, nil)
	}
	r = withURLParam(r, "id", apID)
	w := testutil.Call(t, testHandler.GetAutopilot, r).Want(http.StatusOK)
	var got struct {
		Autopilot AutopilotResponse `json:"autopilot"`
	}
	w.Decode(&got)
	if got.Autopilot.CanManageAccess == nil {
		t.Fatalf("expected can_manage_access to be set on detail response")
	}
	return *got.Autopilot.CanManageAccess
}

// TestAutopilotCollaborator_GrantedMemberCanWrite verifies the full delegation
// flow: a non-creator member is blocked, becomes a writer once granted, and is
// blocked again after the grant is revoked (MUL-3807).
func TestAutopilotCollaborator_GrantedMemberCanWrite(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	apID := createAutopilotAs(t, "", "ap-collab-grant")
	member := createPlainMember(t, "ap-collab-grantee@multica.test")

	updateAs := func(caller string) int {

		r := newRequestAs(caller, "PATCH", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, map[string]any{"title": "edited by " + caller})
		r = withURLParam(r, "id", apID)
		w := testutil.Call(t, testHandler.UpdateAutopilot, r)
		return w.Code
	}

	// Before grant: blocked, and can_write=false.
	if code := updateAs(member); code != http.StatusForbidden {
		t.Fatalf("pre-grant update: expected 403, got %d", code)
	}
	if autopilotCanWrite(t, member, apID) {
		t.Fatalf("pre-grant: expected can_write=false for member")
	}

	// Grant.
	grantAutopilotAccess(t, "", apID, member, http.StatusCreated)

	// After grant: allowed, and can_write=true.
	if !autopilotCanWrite(t, member, apID) {
		t.Fatalf("post-grant: expected can_write=true for collaborator")
	}
	if code := updateAs(member); code != http.StatusOK {
		t.Fatalf("post-grant update: expected 200, got %d", code)
	}

	// Revoke.

	r := newRequest("DELETE", "/api/autopilots/"+apID+"/collaborators/"+member+"?workspace_id="+testWorkspaceID, nil)
	r = withURLParams(r, "id", apID, "userId", member)
	testutil.Call(t, testHandler.RemoveAutopilotCollaborator, r).Want(http.StatusOK)

	// After revoke: blocked again.
	if code := updateAs(member); code != http.StatusForbidden {
		t.Fatalf("post-revoke update: expected 403, got %d", code)
	}
}

// TestAutopilotCollaborator_NonWriterCannotGrant verifies a member without write
// access cannot manage the access list, and granting a non-member is rejected.
func TestAutopilotCollaborator_NonWriterCannotGrant(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	apID := createAutopilotAs(t, "", "ap-collab-guard")
	stranger := createPlainMember(t, "ap-collab-stranger@multica.test")
	victim := createPlainMember(t, "ap-collab-victim@multica.test")

	// A non-writer cannot grant access to anyone.
	grantAutopilotAccess(t, stranger, apID, victim, http.StatusForbidden)

	// Owner granting a non-member (random UUID) is rejected as bad input.
	grantAutopilotAccess(t, "", apID, "00000000-0000-0000-0000-000000000000", http.StatusBadRequest)
}

// TestAutopilotCollaborator_CannotManageAccessList verifies the privilege-
// escalation boundary: a granted collaborator keeps write/execute access but
// CANNOT manage the access list — they cannot grant access to others or revoke
// peers. Only the creator / owner / admin may manage access (MUL-3807).
func TestAutopilotCollaborator_CannotManageAccessList(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	apID := createAutopilotAs(t, "", "ap-collab-noescalate")
	carol := createPlainMember(t, "ap-collab-carol@multica.test")
	dave := createPlainMember(t, "ap-collab-dave@multica.test")
	bob := createPlainMember(t, "ap-collab-bob2@multica.test")

	// Owner grants two collaborators.
	grantAutopilotAccess(t, "", apID, carol, http.StatusCreated)
	grantAutopilotAccess(t, "", apID, dave, http.StatusCreated)

	// Collaborator carol keeps write access (can still edit the autopilot).
	w := httptest.NewRecorder()
	r := newRequestAs(carol, "PATCH", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, map[string]any{"title": "carol edit"})
	r = withURLParam(r, "id", apID)
	testHandler.UpdateAutopilot(w, r)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

	// ...but cannot grant access to a new member.
	grantAutopilotAccess(t, carol, apID, bob, http.StatusForbidden)

	// ...and cannot revoke a peer collaborator.
	w = httptest.NewRecorder()
	r = newRequestAs(carol, "DELETE", "/api/autopilots/"+apID+"/collaborators/"+dave+"?workspace_id="+testWorkspaceID, nil)
	r = withURLParams(r, "id", apID, "userId", dave)
	testHandler.RemoveAutopilotCollaborator(w, r)
	testutil.Equal(t, w.Code, http.StatusForbidden, "HTTP status")

	// can_manage_access: false for the collaborator, true for the owner.
	if autopilotCanManageAccess(t, carol, apID) {
		t.Fatalf("carol can_manage_access: expected false")
	}
	if !autopilotCanManageAccess(t, "", apID) {
		t.Fatalf("owner can_manage_access: expected true")
	}
}

// TestAutopilotWrite_PlainMemberCannotMutateOthers verifies that a workspace
// member who is neither the creator nor an admin cannot edit, trigger, or
// delete an autopilot created by someone else (MUL-3807).
func TestAutopilotWrite_PlainMemberCannotMutateOthers(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	apID := createAutopilotAs(t, "", "ap-perm-owner-created")
	member := createPlainMember(t, "ap-perm-stranger@multica.test")

	// Update.
	w := httptest.NewRecorder()
	r := newRequestAs(member, "PATCH", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, map[string]any{"title": "hijacked"})
	r = withURLParam(r, "id", apID)
	testHandler.UpdateAutopilot(w, r)
	testutil.Equal(t, w.Code, http.StatusForbidden, "HTTP status")

	// Trigger.
	w = httptest.NewRecorder()
	r = newRequestAs(member, "POST", "/api/autopilots/"+apID+"/trigger?workspace_id="+testWorkspaceID, nil)
	r = withURLParam(r, "id", apID)
	testHandler.TriggerAutopilot(w, r)
	testutil.Equal(t, w.Code, http.StatusForbidden, "HTTP status")

	// Delete.
	w = httptest.NewRecorder()
	r = newRequestAs(member, "DELETE", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, nil)
	r = withURLParam(r, "id", apID)
	testHandler.DeleteAutopilot(w, r)
	testutil.Equal(t, w.Code, http.StatusForbidden, "HTTP status")
}

// TestAutopilotWrite_CreatorCanMutateOwn verifies that the member who created
// an autopilot retains write access to it even without an admin role.
func TestAutopilotWrite_CreatorCanMutateOwn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	member := createPlainMember(t, "ap-perm-creator@multica.test")
	apID := createAutopilotAs(t, member, "ap-perm-member-created")

	r := newRequestAs(member, "PATCH", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, map[string]any{"title": "creator edit"})
	r = withURLParam(r, "id", apID)
	testutil.Call(t, testHandler.UpdateAutopilot, r).Want(http.StatusOK)
}

// TestAutopilotWrite_AdminCanMutateMembersAutopilot verifies that a workspace
// owner/admin can manage an autopilot created by a plain member.
func TestAutopilotWrite_AdminCanMutateMembersAutopilot(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	member := createPlainMember(t, "ap-perm-admin-target@multica.test")
	apID := createAutopilotAs(t, member, "ap-perm-admin-target")

	// testUserID is the workspace owner.

	r := newRequest("PATCH", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, map[string]any{"title": "admin edit"})
	r = withURLParam(r, "id", apID)
	testutil.Call(t, testHandler.UpdateAutopilot, r).Want(http.StatusOK)
}

// TestAutopilotWrite_WebhookSecretRedactedForNonWriter verifies that the
// webhook token/path are returned to a writer (the owner) but stripped from
// the read response for a member who lacks write access — seeing the token is
// equivalent to being able to trigger the autopilot (MUL-3807).
func TestAutopilotWrite_WebhookSecretRedactedForNonWriter(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	apID := createAutopilotAs(t, "", "ap-perm-secret")
	stranger := createPlainMember(t, "ap-perm-secret-stranger@multica.test")

	// Owner adds a webhook trigger.
	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/autopilots/"+apID+"/triggers?workspace_id="+testWorkspaceID, map[string]any{"kind": "webhook"})
	r = withURLParam(r, "id", apID)
	testHandler.CreateAutopilotTrigger(w, r)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")

	type getResp struct {
		Triggers []AutopilotTriggerResponse `json:"triggers"`
	}

	// Owner (writer) sees the secret.
	w = httptest.NewRecorder()
	r = withURLParam(newRequest("GET", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, nil), "id", apID)
	testHandler.GetAutopilot(w, r)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var ownerView getResp
	if err := json.NewDecoder(w.Body).Decode(&ownerView); err != nil {
		t.Fatalf("decode owner view: %v", err)
	}
	if len(ownerView.Triggers) != 1 {
		t.Fatalf("owner view: expected 1 trigger, got %d", len(ownerView.Triggers))
	}
	if ownerView.Triggers[0].WebhookToken == nil || *ownerView.Triggers[0].WebhookToken == "" {
		t.Fatalf("owner view: expected webhook_token to be present")
	}
	if ownerView.Triggers[0].WebhookPath == nil {
		t.Fatalf("owner view: expected webhook_path to be present")
	}

	// Plain member (non-writer) sees the trigger but not the secret.
	w = httptest.NewRecorder()
	r = withURLParam(newRequestAs(stranger, "GET", "/api/autopilots/"+apID+"?workspace_id="+testWorkspaceID, nil), "id", apID)
	testHandler.GetAutopilot(w, r)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var strangerView getResp
	if err := json.NewDecoder(w.Body).Decode(&strangerView); err != nil {
		t.Fatalf("decode stranger view: %v", err)
	}
	if len(strangerView.Triggers) != 1 {
		t.Fatalf("stranger view: expected 1 trigger, got %d", len(strangerView.Triggers))
	}
	if strangerView.Triggers[0].Kind != "webhook" {
		t.Fatalf("stranger view: expected webhook trigger to remain visible, got kind %q", strangerView.Triggers[0].Kind)
	}
	if strangerView.Triggers[0].WebhookToken != nil {
		t.Fatalf("stranger view: webhook_token leaked: %v", *strangerView.Triggers[0].WebhookToken)
	}
	if strangerView.Triggers[0].WebhookPath != nil {
		t.Fatalf("stranger view: webhook_path leaked: %v", *strangerView.Triggers[0].WebhookPath)
	}
	if strangerView.Triggers[0].WebhookURL != nil {
		t.Fatalf("stranger view: webhook_url leaked: %v", *strangerView.Triggers[0].WebhookURL)
	}
}
