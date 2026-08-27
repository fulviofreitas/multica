package handler

import (
	"net/http"
	"strings"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func TestCreateIssueInvalidStatusReturns400(t *testing.T) {

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "invalid status issue",
		"status": "active",
	})
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusBadRequest)
	if body := w.Body.String(); !strings.Contains(body, "backlog") {
		t.Errorf("expected error to list valid statuses, got: %s", body)
	}
}

func TestCreateIssueInvalidPriorityReturns400(t *testing.T) {

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":    "invalid priority issue",
		"priority": "P1",
	})
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusBadRequest)
	if body := w.Body.String(); !strings.Contains(body, "urgent") {
		t.Errorf("expected error to list valid priorities, got: %s", body)
	}
}

func TestUpdateIssueInvalidStatusReturns400(t *testing.T) {
	issueID := createTestIssue(t, "update invalid status issue", "todo", "none")
	t.Cleanup(func() { deleteTestIssue(t, issueID) })

	req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{"status": "active"})
	req = withURLParam(req, "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusBadRequest)
}

func TestUpdateIssueInvalidPriorityReturns400(t *testing.T) {
	issueID := createTestIssue(t, "update invalid priority issue", "todo", "none")
	t.Cleanup(func() { deleteTestIssue(t, issueID) })

	req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{"priority": "P1"})
	req = withURLParam(req, "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusBadRequest)
}

func TestBatchUpdateIssuesInvalidStatusReturns400(t *testing.T) {

	req := newRequest("POST", "/api/issues/batch-update", map[string]any{
		"issue_ids": []string{"not-needed"},
		"updates": map[string]any{
			"status": "active",
		},
	})
	testutil.Call(t, testHandler.BatchUpdateIssues, req).Want(http.StatusBadRequest)
}

func TestBatchUpdateIssuesInvalidPriorityReturns400(t *testing.T) {

	req := newRequest("POST", "/api/issues/batch-update", map[string]any{
		"issue_ids": []string{"not-needed"},
		"updates": map[string]any{
			"priority": "P1",
		},
	})
	testutil.Call(t, testHandler.BatchUpdateIssues, req).Want(http.StatusBadRequest)
}
