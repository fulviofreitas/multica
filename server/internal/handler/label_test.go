package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// TestLabelCRUD exercises label create/list/get/update/delete.
func TestLabelCRUD(t *testing.T) {
	// Create
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/labels", map[string]any{
		"name":  "bug",
		"color": "#ef4444",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var created LabelResponse
	json.NewDecoder(w.Body).Decode(&created)
	if created.Name != "bug" || created.Color != "#ef4444" {
		t.Fatalf("CreateLabel: unexpected payload: %+v", created)
	}
	labelID := created.ID

	t.Cleanup(func() {

		req := newRequest("DELETE", "/api/labels/"+labelID, nil)
		req = withURLParam(req, "id", labelID)
		testutil.Call(t, testHandler.DeleteLabel, req)
	})

	// Duplicate name → 409
	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/labels", map[string]any{
		"name":  "BUG", // case-insensitive unique
		"color": "#000000",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusConflict, "HTTP status")

	// Invalid color → 400
	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/labels", map[string]any{
		"name":  "enhancement",
		"color": "nope",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusBadRequest, "HTTP status")

	// List
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/labels", nil)
	testHandler.ListLabels(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var listResp struct {
		Labels []LabelResponse `json:"labels"`
		Total  int             `json:"total"`
	}
	json.NewDecoder(w.Body).Decode(&listResp)
	if listResp.Total < 1 {
		t.Fatalf("ListLabels: expected >= 1 label, got %d", listResp.Total)
	}

	// Get
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/labels/"+labelID, nil)
	req = withURLParam(req, "id", labelID)
	testHandler.GetLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

	// Update
	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/labels/"+labelID, map[string]any{
		"name":  "Bug (P0)",
		"color": "#b91c1c",
	})
	req = withURLParam(req, "id", labelID)
	testHandler.UpdateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var updated LabelResponse
	json.NewDecoder(w.Body).Decode(&updated)
	if updated.Name != "Bug (P0)" || updated.Color != "#b91c1c" {
		t.Fatalf("UpdateLabel: unexpected payload: %+v", updated)
	}
}

// TestIssueLabelAttachDetach exercises attach/detach + the issue-scoped endpoints.
func TestIssueLabelAttachDetach(t *testing.T) {
	// Create issue
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":    "Issue for label attach test",
		"status":   "todo",
		"priority": "medium",
	})
	testHandler.CreateIssue(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var issue IssueResponse
	json.NewDecoder(w.Body).Decode(&issue)
	issueID := issue.ID

	// Create label
	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/labels", map[string]any{
		"name":  "feature",
		"color": "#3b82f6",
	})
	testHandler.CreateLabel(w, req)
	var label LabelResponse
	json.NewDecoder(w.Body).Decode(&label)
	labelID := label.ID

	t.Cleanup(func() {

		req := newRequest("DELETE", "/api/labels/"+labelID, nil)
		req = withURLParam(req, "id", labelID)
		testutil.Call(t, testHandler.DeleteLabel, req)
	})

	// Attach
	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/issues/"+issueID+"/labels", map[string]any{
		"label_id": labelID,
	})
	req = withURLParam(req, "id", issueID)
	testHandler.AttachLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

	// Attach again (idempotent — ON CONFLICT DO NOTHING)
	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/issues/"+issueID+"/labels", map[string]any{
		"label_id": labelID,
	})
	req = withURLParam(req, "id", issueID)
	testHandler.AttachLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

	// List labels for issue
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/issues/"+issueID+"/labels", nil)
	req = withURLParam(req, "id", issueID)
	testHandler.ListLabelsForIssue(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var issueLabels struct {
		Labels []LabelResponse `json:"labels"`
	}
	json.NewDecoder(w.Body).Decode(&issueLabels)
	if len(issueLabels.Labels) != 1 {
		t.Fatalf("ListLabelsForIssue: expected 1 label, got %d", len(issueLabels.Labels))
	}
	if issueLabels.Labels[0].ID != labelID {
		t.Fatalf("ListLabelsForIssue: wrong label returned: %+v", issueLabels.Labels[0])
	}

	// Detach
	w = httptest.NewRecorder()
	req = newRequest("DELETE", "/api/issues/"+issueID+"/labels/"+labelID, nil)
	req = withURLParams(req, "id", issueID, "labelId", labelID)
	testHandler.DetachLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

	// Confirm detached
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/issues/"+issueID+"/labels", nil)
	req = withURLParam(req, "id", issueID)
	testHandler.ListLabelsForIssue(w, req)
	json.NewDecoder(w.Body).Decode(&issueLabels)
	if len(issueLabels.Labels) != 0 {
		t.Fatalf("after Detach: expected 0 labels, got %d", len(issueLabels.Labels))
	}
}

func TestIssueLabelRejectsNonIssueScope(t *testing.T) {
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":    "Issue rejects agent label",
		"status":   "todo",
		"priority": "medium",
	})
	testHandler.CreateIssue(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var issue IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&issue); err != nil {
		t.Fatalf("decode issue: %v", err)
	}

	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/labels", map[string]any{
		"resource_type": "agent",
		"name":          "agent-only-" + uuid.NewString()[:8],
		"color":         "#3b82f6",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var label LabelResponse
	if err := json.NewDecoder(w.Body).Decode(&label); err != nil {
		t.Fatalf("decode label: %v", err)
	}
	t.Cleanup(func() {

		req := newRequest("DELETE", "/api/labels/"+label.ID, nil)
		req = withURLParam(req, "id", label.ID)
		testutil.Call(t, testHandler.DeleteLabel, req)
	})

	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/issues/"+issue.ID+"/labels", map[string]any{
		"label_id": label.ID,
	})
	req = withURLParam(req, "id", issue.ID)
	testHandler.AttachLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusNotFound, "HTTP status")

	w = httptest.NewRecorder()
	req = newRequest("DELETE", "/api/issues/"+issue.ID+"/labels/"+label.ID, nil)
	req = withURLParams(req, "id", issue.ID, "labelId", label.ID)
	testHandler.DetachLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusNotFound, "HTTP status")
}

// TestLabelNotFoundAcrossWorkspaces ensures GET with a foreign workspace
// header returns 404 — the query's `WHERE workspace_id = $2` does the work.
func TestLabelNotFoundAcrossWorkspaces(t *testing.T) {
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/labels", map[string]any{
		"name":  "cross-ws-test",
		"color": "#a855f7",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var label LabelResponse
	json.NewDecoder(w.Body).Decode(&label)
	labelID := label.ID

	t.Cleanup(func() {

		req := newRequest("DELETE", "/api/labels/"+labelID, nil)
		req = withURLParam(req, "id", labelID)
		testutil.Call(t, testHandler.DeleteLabel, req)
	})

	// GET with a different workspace ID → 404
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/labels/"+labelID, nil)
	req.Header.Set("X-Workspace-ID", "00000000-0000-0000-0000-000000000000")
	req = withURLParam(req, "id", labelID)
	testHandler.GetLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusNotFound, "HTTP status")
}

// TestUpdateLabelCrossWorkspace — PUT with a foreign workspace header must not
// allow updating a label in another workspace (404 via pgx.ErrNoRows from the
// UPDATE ... WHERE id = $1 AND workspace_id = $2 clause).
func TestUpdateLabelCrossWorkspace(t *testing.T) {
	// Create in real workspace
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/labels", map[string]any{
		"name":  "cross-ws-update-test",
		"color": "#10b981",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var label LabelResponse
	json.NewDecoder(w.Body).Decode(&label)
	labelID := label.ID

	t.Cleanup(func() {

		req := newRequest("DELETE", "/api/labels/"+labelID, nil)
		req = withURLParam(req, "id", labelID)
		testutil.Call(t, testHandler.DeleteLabel, req)
	})

	// PUT with a foreign workspace ID → 404
	w = httptest.NewRecorder()
	req = newRequest("PUT", "/api/labels/"+labelID, map[string]any{"name": "hacked"})
	req.Header.Set("X-Workspace-ID", "00000000-0000-0000-0000-000000000000")
	req = withURLParam(req, "id", labelID)
	testHandler.UpdateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusNotFound, "HTTP status")

	// Sanity: the label wasn't renamed.
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/labels/"+labelID, nil)
	req = withURLParam(req, "id", labelID)
	testHandler.GetLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var after LabelResponse
	json.NewDecoder(w.Body).Decode(&after)
	if after.Name != "cross-ws-update-test" {
		t.Fatalf("label name changed despite cross-workspace PUT: got %q", after.Name)
	}
}

// TestAttachLabelCrossWorkspaceLabel — an attach request whose label_id
// belongs to a different workspace must return 404, not silently no-op.
// Directly exercises the GetLabel workspace precheck and the SQL-layer
// defense-in-depth guard.
func TestAttachLabelCrossWorkspaceLabel(t *testing.T) {
	// Issue in the test workspace
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":    "cross-ws-attach-issue",
		"status":   "todo",
		"priority": "medium",
	})
	testHandler.CreateIssue(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var issue IssueResponse
	json.NewDecoder(w.Body).Decode(&issue)

	// Label in a second workspace — insert directly via the pool to avoid
	// the public API (which would require creating a full second workspace
	// fixture). The defense-in-depth is exactly that the handler refuses
	// even labels that exist *somewhere* but not in the current workspace.
	otherWorkspaceID := createOtherTestWorkspace(t)
	var otherLabelID string
	err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue_label (workspace_id, name, color)
		VALUES ($1, 'foreign-label', '#000000')
		RETURNING id
	`, otherWorkspaceID).Scan(&otherLabelID)
	if err != nil {
		t.Fatalf("insert foreign label: %v", err)
	}

	// Try to attach the foreign label to the test-workspace issue.
	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/issues/"+issue.ID+"/labels", map[string]any{
		"label_id": otherLabelID,
	})
	req = withURLParam(req, "id", issue.ID)
	testHandler.AttachLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusNotFound, "HTTP status")

	// Confirm nothing was attached.
	w = httptest.NewRecorder()
	req = newRequest("GET", "/api/issues/"+issue.ID+"/labels", nil)
	req = withURLParam(req, "id", issue.ID)
	testHandler.ListLabelsForIssue(w, req)
	var list struct {
		Labels []LabelResponse `json:"labels"`
	}
	json.NewDecoder(w.Body).Decode(&list)
	if len(list.Labels) != 0 {
		t.Fatalf("expected 0 labels on issue, got %d", len(list.Labels))
	}
}

// TestLabelNameTooLong — names longer than 32 chars must return 400.
func TestLabelNameTooLong(t *testing.T) {
	longName := strings.Repeat("a", 33)
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/labels", map[string]any{
		"name":  longName,
		"color": "#123456",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusBadRequest, "HTTP status")

	// Exactly 32 chars is fine.
	okName := strings.Repeat("b", 32)
	w = httptest.NewRecorder()
	req = newRequest("POST", "/api/labels", map[string]any{
		"name":  okName,
		"color": "#123456",
	})
	testHandler.CreateLabel(w, req)
	testutil.Equal(t, w.Code, http.StatusCreated, "HTTP status")
	var created LabelResponse
	json.NewDecoder(w.Body).Decode(&created)
	t.Cleanup(func() {

		req := newRequest("DELETE", "/api/labels/"+created.ID, nil)
		req = withURLParam(req, "id", created.ID)
		testutil.Call(t, testHandler.DeleteLabel, req)
	})
}

func TestLabelNameRejectsControlCharacters(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		call func(*httptest.ResponseRecorder, *http.Request)
	}{
		{
			name: "create newline",
			body: map[string]any{"name": "bug\nurgent", "color": "#123456"},
			call: func(w *httptest.ResponseRecorder, req *http.Request) {
				testHandler.CreateLabel(w, req)
			},
		},
		{
			name: "create tab",
			body: map[string]any{"name": "bug\turgent", "color": "#123456"},
			call: func(w *httptest.ResponseRecorder, req *http.Request) {
				testHandler.CreateLabel(w, req)
			},
		},
		{
			name: "update control",
			body: map[string]any{"name": "bug\u0000urgent"},
			call: func(w *httptest.ResponseRecorder, req *http.Request) {
				req = withURLParam(req, "id", "00000000-0000-0000-0000-000000000000")
				testHandler.UpdateLabel(w, req)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := newRequest("POST", "/api/labels", tc.body)
			tc.call(w, req)
			testutil.Equal(t, w.Code, http.StatusBadRequest, "HTTP status")
		})
	}
}

func TestLabelNameAllowsEmoji(t *testing.T) {

	req := newRequest("POST", "/api/labels", map[string]any{
		"name":  "🐛 bug",
		"color": "#123456",
	})
	w := testutil.Call(t, testHandler.CreateLabel, req).Want(http.StatusCreated)

	var created LabelResponse
	json.NewDecoder(w.Body).Decode(&created)
	if created.Name != "🐛 bug" {
		t.Fatalf("CreateLabel emoji name: got %q", created.Name)
	}
	t.Cleanup(func() {

		req := newRequest("DELETE", "/api/labels/"+created.ID, nil)
		req = withURLParam(req, "id", created.ID)
		testutil.Call(t, testHandler.DeleteLabel, req)
	})
}

func TestLabelResourceTypesHaveIndependentNamespaces(t *testing.T) {
	name := "shared-scope-label-" + uuid.NewString()[:8]
	for _, resourceType := range []string{"issue", "agent", "skill"} {

		req := newRequest("POST", "/api/labels", map[string]any{
			"resource_type": resourceType,
			"name":          name,
			"description":   resourceType + " description",
			"color":         "#3b82f6",
		})
		w := testutil.Call(t, testHandler.CreateLabel, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateLabel resource_type=%s: expected 201, got %d: %s", resourceType, w.Code, w.Body.String())
		}
		var created LabelResponse
		w.Decode(&created)
		if created.ResourceType != resourceType || created.Description != resourceType+" description" {
			t.Fatalf("unexpected scoped label: %+v", created)
		}
		t.Cleanup(func() {

			req := newRequest("DELETE", "/api/labels/"+created.ID, nil)
			req = withURLParam(req, "id", created.ID)
			testutil.Call(t, testHandler.DeleteLabel, req)
		})
	}
}

func TestDeleteResourceLabelCleansAssignments(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Resource Label Cleanup", nil)
	skillID := insertHandlerTestSkill(t, "resource-label-cleanup", "# cleanup")

	create := func(resourceType string) LabelResponse {
		t.Helper()
		w := testutil.Call(t, testHandler.CreateLabel, newRequest("POST", "/api/labels", map[string]any{
			"resource_type": resourceType,
			"name":          resourceType + "-cleanup-" + uuid.NewString()[:8],
			"color":         "#3b82f6",
		}))
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s label: expected 201, got %d: %s", resourceType, w.Code, w.Body.String())
		}
		var label LabelResponse
		if err := json.NewDecoder(w.Body).Decode(&label); err != nil {
			t.Fatalf("decode %s label: %v", resourceType, err)
		}
		return label
	}

	agentLabel := create("agent")
	skillLabel := create("skill")
	for _, link := range []struct {
		query string
		id    string
		label string
	}{
		{`INSERT INTO agent_to_label (agent_id, label_id) VALUES ($1, $2)`, agentID, agentLabel.ID},
		{`INSERT INTO skill_to_label (skill_id, label_id) VALUES ($1, $2)`, skillID, skillLabel.ID},
	} {
		if _, err := testPool.Exec(context.Background(), link.query, link.id, link.label); err != nil {
			t.Fatalf("seed resource label assignment: %v", err)
		}
	}

	for _, labelID := range []string{agentLabel.ID, skillLabel.ID} {

		req := withURLParam(newRequest("DELETE", "/api/labels/"+labelID, nil), "id", labelID)
		testutil.Call(t, testHandler.DeleteLabel, req).Want(http.StatusNoContent)
	}

	for _, check := range []struct {
		query string
		id    string
	}{
		{`SELECT COUNT(*) FROM agent_to_label WHERE agent_id = $1`, agentID},
		{`SELECT COUNT(*) FROM skill_to_label WHERE skill_id = $1`, skillID},
	} {
		var count int
		if err := testPool.QueryRow(context.Background(), check.query, check.id).Scan(&count); err != nil {
			t.Fatalf("count remaining resource assignments: %v", err)
		}
		if count != 0 {
			t.Fatalf("resource label assignments remain after label deletion: %d", count)
		}
	}
}

func TestLabelRejectsUnknownResourceType(t *testing.T) {

	req := newRequest("POST", "/api/labels", map[string]any{
		"resource_type": "project",
		"name":          "invalid-scope",
		"color":         "#3b82f6",
	})
	testutil.Call(t, testHandler.CreateLabel, req).Want(http.StatusBadRequest)
}

// TestColorCaseNormalization — input `#ABCDEF` must be stored as `#abcdef`
// so the case-insensitive uniqueness and downstream CSS rendering are
// consistent. Also accepts a bare `ABCDEF` (no leading #).
func TestColorCaseNormalization(t *testing.T) {
	cases := []struct {
		nameSuffix string
		input      string
		want       string
	}{
		{"upper", "#ABCDEF", "#abcdef"},
		{"mixed", "#AbCdEf", "#abcdef"},
		{"bare", "ABCDEF", "#abcdef"},
		{"lower", "#123abc", "#123abc"},
	}
	for _, tc := range cases {

		name := "color-norm-" + tc.nameSuffix // unique & case-independent
		req := newRequest("POST", "/api/labels", map[string]any{
			"name":  name,
			"color": tc.input,
		})
		w := testutil.Call(t, testHandler.CreateLabel, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateLabel %q: expected 201, got %d: %s", tc.input, w.Code, w.Body.String())
		}
		var got LabelResponse
		json.NewDecoder(w.Body).Decode(&got)
		if got.Color != tc.want {
			t.Errorf("color normalization %q: got %q, want %q", tc.input, got.Color, tc.want)
		}
		t.Cleanup(func() {

			req := newRequest("DELETE", "/api/labels/"+got.ID, nil)
			req = withURLParam(req, "id", got.ID)
			testutil.Call(t, testHandler.DeleteLabel, req)
		})
	}
}

// createOtherTestWorkspace inserts a second workspace + owner membership for
// cross-workspace tests. Returns the new workspace id; cleanup registered.
func createOtherTestWorkspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var wsID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "Other Handler Tests", handlerTestWorkspaceSlug+"-other", "temp second workspace", "OTH").Scan(&wsID)
	if err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
	`, wsID, testUserID); err != nil {
		t.Fatalf("add member to other workspace: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, wsID)
	})
	return wsID
}
