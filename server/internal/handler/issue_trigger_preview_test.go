package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// seededReadyAgentID returns a workspace agent that has a runtime bound (the
// fixture's first agent), so WillEnqueueRun treats it as ready.
func seededReadyAgentID(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		SELECT id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL
		ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&id); err != nil {
		t.Fatalf("load ready agent: %v", err)
	}
	return id
}

func previewIssueTrigger(t *testing.T, body map[string]any) IssueTriggerPreviewResponse {
	t.Helper()

	req := newRequest("POST", "/api/issues/preview-trigger?workspace_id="+testWorkspaceID, body)
	w := testutil.Call(t, testHandler.PreviewIssueTrigger, req).Want(http.StatusOK)
	var resp IssueTriggerPreviewResponse
	w.Decode(&resp)
	return resp
}

func createIssueForTest(t *testing.T, body map[string]any) IssueResponse {
	t.Helper()

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
	var created IssueResponse
	w.Decode(&created)
	t.Cleanup(func() {
		r := withURLParam(newRequest("DELETE", "/api/issues/"+created.ID, nil), "id", created.ID)
		testHandler.DeleteIssue(httptest.NewRecorder(), r)
	})
	return created
}

func taskCountFor(t *testing.T, issueID, agentID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2
	`, issueID, agentID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

// TestPreviewIssueTrigger_CreateAgentVsBacklog covers the create entry point:
// an active status with an agent assignee previews one run; the same assignee
// parked in backlog previews none.
func TestPreviewIssueTrigger_CreateAgentVsBacklog(t *testing.T) {
	agentID := seededReadyAgentID(t)

	active := previewIssueTrigger(t, map[string]any{
		"is_create":     true,
		"assignee_type": "agent",
		"assignee_id":   agentID,
		"status":        "todo",
	})
	if active.TotalCount != 1 || len(active.Triggers) != 1 {
		t.Fatalf("active create: expected 1 trigger, got %+v", active)
	}
	if active.Triggers[0].AgentID != agentID || active.Triggers[0].Source != "assign" {
		t.Fatalf("active create: wrong trigger %+v", active.Triggers[0])
	}

	backlog := previewIssueTrigger(t, map[string]any{
		"is_create":     true,
		"assignee_type": "agent",
		"assignee_id":   agentID,
		"status":        "backlog",
	})
	if backlog.TotalCount != 0 {
		t.Fatalf("backlog create: expected 0 triggers, got %+v", backlog)
	}
}

// TestPreviewIssueTrigger_MemberNoTrigger verifies a member assignee never
// previews a run.
func TestPreviewIssueTrigger_MemberNoTrigger(t *testing.T) {
	resp := previewIssueTrigger(t, map[string]any{
		"is_create":     true,
		"assignee_type": "member",
		"assignee_id":   testUserID,
		"status":        "todo",
	})
	if resp.TotalCount != 0 {
		t.Fatalf("member assignee: expected 0 triggers, got %+v", resp)
	}
}

// TestPreviewIssueTrigger_BatchAggregates verifies the batch shape: two
// agent-assigned issues moving out of backlog preview two distinct runs.
func TestPreviewIssueTrigger_BatchAggregates(t *testing.T) {
	agentID := seededReadyAgentID(t)
	i1 := createIssueForTest(t, map[string]any{"title": "batch preview 1", "status": "backlog", "assignee_type": "agent", "assignee_id": agentID})
	i2 := createIssueForTest(t, map[string]any{"title": "batch preview 2", "status": "backlog", "assignee_type": "agent", "assignee_id": agentID})

	resp := previewIssueTrigger(t, map[string]any{
		"issue_ids": []string{i1.ID, i2.ID},
		"status":    "todo",
	})
	if resp.TotalCount != 2 {
		t.Fatalf("batch promote: expected total_count 2, got %+v", resp)
	}
	seen := map[string]bool{}
	for _, tr := range resp.Triggers {
		if tr.Source != "status" {
			t.Fatalf("batch promote: expected source=status, got %q", tr.Source)
		}
		seen[tr.IssueID] = true
	}
	if !seen[i1.ID] || !seen[i2.ID] {
		t.Fatalf("batch promote: missing an issue in %+v", resp.Triggers)
	}
}

// TestPreviewIssueTrigger_MatchesWritePath is the core invariant: when preview
// says a run will start, the real write path enqueues it; when preview says it
// won't, the write path enqueues nothing.
func TestPreviewIssueTrigger_MatchesWritePath(t *testing.T) {
	agentID := seededReadyAgentID(t)

	// Case 1: preview says assign will start → write path enqueues.
	issue := createIssueForTest(t, map[string]any{"title": "match write 1", "status": "todo"})
	pv := previewIssueTrigger(t, map[string]any{
		"issue_ids":     []string{issue.ID},
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	if pv.TotalCount != 1 {
		t.Fatalf("preview assign: expected 1, got %+v", pv)
	}

	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"assignee_type": "agent", "assignee_id": agentID}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := taskCountFor(t, issue.ID, agentID); got == 0 {
		t.Fatalf("preview promised a run but write path enqueued none")
	}

	// Case 2: preview says backlog assign will NOT start → write enqueues none.
	issue2 := createIssueForTest(t, map[string]any{"title": "match write 2", "status": "backlog"})
	pv2 := previewIssueTrigger(t, map[string]any{
		"issue_ids":     []string{issue2.ID},
		"assignee_type": "agent",
		"assignee_id":   agentID,
		"status":        "backlog",
	})
	if pv2.TotalCount != 0 {
		t.Fatalf("preview backlog assign: expected 0, got %+v", pv2)
	}

	req2 := withURLParam(newRequest("PUT", "/api/issues/"+issue2.ID, map[string]any{"assignee_type": "agent", "assignee_id": agentID}), "id", issue2.ID)
	testutil.Call(t, testHandler.UpdateIssue, req2).Want(http.StatusOK)
	if got := taskCountFor(t, issue2.ID, agentID); got != 0 {
		t.Fatalf("preview said no run for backlog assign but write path enqueued %d", got)
	}
}

// TestUpdateIssueSuppressRunSkipsEnqueue verifies suppress_run applies the
// assignee change but starts no run, while the same write without it does.
func TestUpdateIssueSuppressRunSkipsEnqueue(t *testing.T) {
	agentID := seededReadyAgentID(t)

	// Suppressed assign: assignee set, no task.
	suppressed := createIssueForTest(t, map[string]any{"title": "suppress on", "status": "todo"})

	req := withURLParam(newRequest("PUT", "/api/issues/"+suppressed.ID, map[string]any{
		"assignee_type": "agent", "assignee_id": agentID, "suppress_run": true,
	}), "id", suppressed.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := taskCountFor(t, suppressed.ID, agentID); got != 0 {
		t.Fatalf("suppress_run=true should not enqueue, got %d tasks", got)
	}

	// Control: same write without suppress_run enqueues.
	control := createIssueForTest(t, map[string]any{"title": "suppress off", "status": "todo"})

	req2 := withURLParam(newRequest("PUT", "/api/issues/"+control.ID, map[string]any{
		"assignee_type": "agent", "assignee_id": agentID,
	}), "id", control.ID)
	testutil.Call(t, testHandler.UpdateIssue, req2).Want(http.StatusOK)
	if got := taskCountFor(t, control.ID, agentID); got == 0 {
		t.Fatalf("control (no suppress_run) should enqueue, got 0 tasks")
	}
}

// TestUpdateIssueHandoffNotePersistsOnTask verifies an assign carrying a
// handoff_note writes that note onto the enqueued task (the daemon then renders
// it), while a suppressed assign with a note enqueues nothing at all.
func TestUpdateIssueHandoffNotePersistsOnTask(t *testing.T) {
	agentID := seededReadyAgentID(t)
	note := "Only touch the login flow."

	issue := createIssueForTest(t, map[string]any{"title": "handoff persist", "status": "todo"})

	req := withURLParam(newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{
		"assignee_type": "agent", "assignee_id": agentID, "handoff_note": note,
	}), "id", issue.ID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)

	var stored string
	if err := testPool.QueryRow(context.Background(), `
		SELECT COALESCE(handoff_note, '') FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 ORDER BY created_at DESC LIMIT 1
	`, issue.ID, agentID).Scan(&stored); err != nil {
		t.Fatalf("read task handoff_note: %v", err)
	}
	if stored != note {
		t.Fatalf("expected task handoff_note %q, got %q", note, stored)
	}

	// Suppressed assign with a note: no task at all (no run to inject into).
	suppressed := createIssueForTest(t, map[string]any{"title": "handoff suppressed", "status": "todo"})

	req2 := withURLParam(newRequest("PUT", "/api/issues/"+suppressed.ID, map[string]any{
		"assignee_type": "agent", "assignee_id": agentID, "handoff_note": note, "suppress_run": true,
	}), "id", suppressed.ID)
	testutil.Call(t, testHandler.UpdateIssue, req2).Want(http.StatusOK)
	if got := taskCountFor(t, suppressed.ID, agentID); got != 0 {
		t.Fatalf("suppressed handoff should enqueue no task, got %d", got)
	}
}

// TestPreviewIssueTrigger_MalformedBody verifies the endpoint rejects a
// malformed body with 400 rather than a 500 or a silent empty result.
func TestPreviewIssueTrigger_MalformedBody(t *testing.T) {

	req := httptest.NewRequest("POST", "/api/issues/preview-trigger?workspace_id="+testWorkspaceID, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	testutil.Call(t, testHandler.PreviewIssueTrigger, req).Want(http.StatusBadRequest)
}
