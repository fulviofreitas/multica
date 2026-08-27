package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

type batchClaimReceiptResponse struct {
	Tasks []struct {
		ID                  string   `json:"id"`
		RuntimeID           string   `json:"runtime_id"`
		AuthToken           string   `json:"auth_token"`
		DeliveredCommentIDs []string `json:"delivered_comment_ids"`
	} `json:"tasks"`
}

// TestClaimTasksByRuntime_MaxTasksZeroClaimsNothing pins the MUL-4257 review
// fix: max_tasks=0 is a valid "no free slots" poll that must claim nothing —
// it must NOT be coerced to 1 (which would dispatch a task the daemon can't run
// and strand it until stale reclaim).
func TestClaimTasksByRuntime_MaxTasksZeroClaimsNothing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rt := createClaimReclaimRuntime(t, ctx, "Batch max0 rt")
	a, i := createClaimReclaimAgentAndIssue(t, ctx, rt, "Batch max0 agent")
	taskID := seedQueuedIssueTask(t, ctx, a, rt, i)

	w := postBatchClaim(t, testWorkspaceID, []string{rt}, 0)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var resp batchClaimReceiptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 0 {
		t.Fatalf("max_tasks=0 claimed %d tasks, want 0", len(resp.Tasks))
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Fatalf("task status = %s, want still queued", status)
	}
}

// TestClaimTasksByRuntime_MaxTasksNegativeIsBadRequest pins that a negative
// max_tasks is rejected rather than silently coerced.
func TestClaimTasksByRuntime_MaxTasksNegativeIsBadRequest(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	rt := createClaimReclaimRuntime(t, context.Background(), "Batch neg rt")
	w := postBatchClaim(t, testWorkspaceID, []string{rt}, -1)
	testutil.Equal(t, w.Code, http.StatusBadRequest, "HTTP status")
}

// TestClaimTasksByRuntime_SkipsInvalidRuntimeID pins the MUL-4257 review fix:
// a malformed runtime_id must be skipped (non-panicking parse), not turned into
// a 500 — and a valid runtime in the same request is still claimed.
func TestClaimTasksByRuntime_SkipsInvalidRuntimeID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rt := createClaimReclaimRuntime(t, ctx, "Batch badid rt")
	a, i := createClaimReclaimAgentAndIssue(t, ctx, rt, "Batch badid agent")
	seedQueuedIssueTask(t, ctx, a, rt, i)

	w := postBatchClaim(t, testWorkspaceID, []string{"not-a-uuid", rt}, 5)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var resp batchClaimReceiptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 1 || resp.Tasks[0].RuntimeID != rt {
		t.Fatalf("expected the valid runtime's task to be claimed despite the invalid id, got %+v", resp.Tasks)
	}
}

// seedCommentBackedQueuedTask inserts a queued task triggered by a real comment
// on its issue, returning (taskID, commentID).
func seedCommentBackedQueuedTask(t *testing.T, ctx context.Context, agentID, runtimeID, issueID string) (string, string) {
	t.Helper()
	var commentID string
	commentID = dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "author_type": testutil.Raw("'member'"), "author_id": testUserID, "content": testutil.Raw("'please handle this'")})

	var taskID string
	taskID = dbfx.Insert(t, "agent_task_queue", testutil.Cols{"agent_id": agentID, "runtime_id": runtimeID, "issue_id": issueID, "trigger_comment_id": commentID, "status": testutil.Raw("'queued'"), "priority": testutil.Raw("0")})
	return taskID, commentID
}

func assertCommentDelivered(t *testing.T, ctx context.Context, taskID, commentID string) {
	t.Helper()
	var member bool
	if err := testPool.QueryRow(ctx, `
		SELECT $1 = ANY(delivered_comment_ids) FROM agent_task_queue WHERE id = $2
	`, commentID, taskID).Scan(&member); err != nil {
		t.Fatalf("read delivered_comment_ids: %v", err)
	}
	if !member {
		t.Fatalf("trigger comment %s not persisted in task %s delivered_comment_ids", commentID, taskID)
	}
}

// TestClaimTasksByRuntime_PersistsCommentDeliveryReceipt pins the MUL-4257
// must-fix: the batch path routes through FinalizeTaskClaim, so a comment-backed
// task claimed via batch persists the delivered_comment_ids receipt AND returns
// it in the response.
func TestClaimTasksByRuntime_PersistsCommentDeliveryReceipt(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rt := createClaimReclaimRuntime(t, ctx, "Batch receipt rt")
	a, i := createClaimReclaimAgentAndIssue(t, ctx, rt, "Batch receipt agent")
	taskID, commentID := seedCommentBackedQueuedTask(t, ctx, a, rt, i)

	w := postBatchClaim(t, testWorkspaceID, []string{rt}, 5)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var resp batchClaimReceiptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 1 {
		t.Fatalf("claimed %d tasks, want 1: %s", len(resp.Tasks), w.Body.String())
	}
	found := false
	for _, id := range resp.Tasks[0].DeliveredCommentIDs {
		if id == commentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("response delivered_comment_ids %v missing trigger comment %s", resp.Tasks[0].DeliveredCommentIDs, commentID)
	}
	assertCommentDelivered(t, ctx, taskID, commentID)
}

// TestClaimTasksByRuntime_StaleReclaimRecordsDeliveryReceipt pins that a
// comment-backed task recovered via the batch reclaim path (dispatched, never
// started, past the recovery window) is re-finalized so its delivery receipt is
// recorded on the replacement claim.
func TestClaimTasksByRuntime_StaleReclaimRecordsDeliveryReceipt(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rt := createClaimReclaimRuntime(t, ctx, "Batch stale-receipt rt")
	a, i := createClaimReclaimAgentAndIssue(t, ctx, rt, "Batch stale-receipt agent")

	var commentID string
	commentID = dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": i, "author_type": testutil.Raw("'member'"), "author_id": testUserID, "content": testutil.Raw("'stale reclaim comment'")})

	// Stale dispatched, comment-backed, never started, past the 90s window.
	var taskID string
	taskID = dbfx.Insert(t, "agent_task_queue", testutil.Cols{"agent_id": a, "runtime_id": rt, "issue_id": i, "trigger_comment_id": commentID, "status": testutil.Raw("'dispatched'"), "priority": testutil.Raw("0"), "dispatched_at": testutil.Raw("now() - interval '120 seconds'"), "started_at": testutil.Raw("NULL")})

	w := postBatchClaim(t, testWorkspaceID, []string{rt}, 5)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var resp batchClaimReceiptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 1 || resp.Tasks[0].ID != taskID {
		t.Fatalf("expected the stale task %s reclaimed, got %+v", taskID, resp.Tasks)
	}
	assertCommentDelivered(t, ctx, taskID, commentID)
}

// TestClaimTasksByRuntime_SkipsRuntimeOwnedByAnotherDaemon pins the MUL-4257
// review must-fix: a daemon must not batch-claim a task routed to a runtime
// bound to a DIFFERENT daemon, even in the same workspace. The runtime is
// skipped and its task stays queued for the owning machine.
func TestClaimTasksByRuntime_SkipsRuntimeOwnedByAnotherDaemon(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	// Runtime bound to a different daemon in the same (handler-test) workspace.
	var rt string
	rt = dbfx.Insert(t, "agent_runtime", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": testutil.Raw("'other-daemon-machine'"), "name": testutil.Raw("'Other daemon RT'"), "runtime_mode": testutil.Raw("'cloud'"), "provider": testutil.Raw("'handler_test_runtime'"), "status": testutil.Raw("'online'"), "device_info": testutil.Raw("'x'"), "metadata": testutil.Raw("'{}'::jsonb"), "last_seen_at": testutil.Raw("now()"), "visibility": testutil.Raw("'private'"), "owner_id": testUserID})

	a, i := createClaimReclaimAgentAndIssue(t, ctx, rt, "Other daemon agent")
	taskID := seedQueuedIssueTask(t, ctx, a, rt, i)

	// postBatchClaim sends daemon_id = batchClaimTestDaemonID ("batch-claim-review"),
	// which differs from the runtime's "other-daemon-machine".
	w := postBatchClaim(t, testWorkspaceID, []string{rt}, 5)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var resp batchClaimReceiptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Tasks) != 0 {
		t.Fatalf("daemon-A claimed %d tasks from a runtime owned by daemon-B, want 0", len(resp.Tasks))
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Fatalf("task status = %s, want still queued for the owning daemon", status)
	}
}

// TestClaimTasksByRuntime_RequiresDaemonID pins that the batch claim rejects a
// request with no daemon_id rather than falling back to workspace-only scoping.
func TestClaimTasksByRuntime_RequiresDaemonID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/claim",
		map[string]any{"runtime_ids": []string{}, "max_tasks": 5},
		testWorkspaceID, batchClaimTestDaemonID)
	testutil.Call(t, testHandler.ClaimTasksByRuntime, req).Want(http.StatusBadRequest)
}

// TestClaimTasksByRuntime_RepairsStaleCommentPlan pins the MUL-4257 review
// must-fix: when a claimed task's trigger comment was deleted (only coalesced
// survivors remain), the batch path must NOT finalize+dispatch it (which would
// silently drop the surviving comment). Instead it cancels the stale task,
// omits it from the batch, and replays the surviving comment as a fresh plan.
func TestClaimTasksByRuntime_RepairsStaleCommentPlan(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	rt := createClaimReclaimRuntime(t, ctx, "Stale plan rt")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, rt, "Stale plan agent")
	// Assign the issue to the agent so the surviving comment re-routes to it.
	if _, err := testPool.Exec(ctx, `UPDATE issue SET assignee_type='agent', assignee_id=$1 WHERE id=$2`, agentID, issueID); err != nil {
		t.Fatalf("assign issue: %v", err)
	}

	// A surviving member comment on the issue.
	var survivorID string
	survivorID = dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "author_type": testutil.Raw("'member'"), "author_id": testUserID, "content": testutil.Raw("'please still handle this'")})

	// Stale plan: trigger_comment_id NULL, only coalesced survivor remains.
	var staleID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, coalesced_comment_ids)
		VALUES ($1, $2, $3, 'queued', 0, ARRAY[$4]::uuid[])
		RETURNING id
	`, agentID, rt, issueID, survivorID).Scan(&staleID); err != nil {
		t.Fatalf("seed stale task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, staleID) })
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })

	w := postBatchClaim(t, testWorkspaceID, []string{rt}, 5)
	testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	var resp batchClaimReceiptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// (1) The stale task must not be returned.
	for _, task := range resp.Tasks {
		if task.ID == staleID {
			t.Fatalf("stale-plan task %s was dispatched by the batch path; want it repaired/omitted", staleID)
		}
	}
	// (2) The stale task must be cancelled.
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, staleID).Scan(&status); err != nil {
		t.Fatalf("read stale status: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("stale task status = %s, want cancelled", status)
	}
	// (3) The surviving comment was rebuilt into a fresh plan (a new task with
	// the survivor as its trigger).
	var rebuilt int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND trigger_comment_id = $2 AND id <> $3
	`, issueID, survivorID, staleID).Scan(&rebuilt); err != nil {
		t.Fatalf("count rebuilt: %v", err)
	}
	if rebuilt < 1 {
		t.Fatalf("expected the surviving comment rebuilt into a new trigger task, found %d", rebuilt)
	}
}
