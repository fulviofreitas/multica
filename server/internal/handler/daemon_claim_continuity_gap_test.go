package handler

import (
	"context"
	"net/http"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// claimContinuityGapProbe decodes just the continuity-gap fields off a claim
// response so the two MUL-5305 disclosure paths can be asserted end-to-end.
type claimContinuityGapProbe struct {
	Task *struct {
		ID                            string `json:"id"`
		PriorSessionID                string `json:"prior_session_id"`
		PriorSessionResumeUnavailable bool   `json:"prior_session_resume_unavailable"`
	} `json:"task"`
}

func claimOneTaskForRuntime(t *testing.T, runtimeID, daemonID string) claimContinuityGapProbe {
	t.Helper()

	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, daemonID)
	req = withURLParam(req, "runtimeId", runtimeID)
	w := testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)
	var probe claimContinuityGapProbe
	w.Decode(&probe)
	if probe.Task == nil {
		t.Fatal("expected a claimed task in the response")
	}
	return probe
}

// TestClaimTaskByRuntime_ChatRolloutMissingDisclosesGap is the claim-response
// half of MUL-5305 Must-fix 2 for chat: when the most recent terminal task on a
// chat session withheld its Codex session (rollout missing), the next chat claim
// resumes the older pointer but MUST still set prior_session_resume_unavailable
// so the run discloses the gap instead of silently continuing.
func TestClaimTaskByRuntime_ChatRolloutMissingDisclosesGap(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "ChatGapClaimAgent", []byte("[]"))
	runtimeID := handlerTestRuntimeID(t)

	// Chat session that still carries a good resume pointer from an earlier turn.
	var sessionID string
	sessionID = dbfx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": testutil.Raw("'gap chat'"), "status": testutil.Raw("'active'"), "runtime_id": runtimeID, "session_id": testutil.Raw("'OLD-GOOD-CHAT-SESSION'"), "work_dir": testutil.Raw("'/tmp/chat'")})

	// Most recent terminal turn on this session withheld its Codex session.
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, started_at, completed_at, chat_session_id, session_rollout_missing)
		VALUES ($1, $2, 'completed', 0, now() - interval '1 minute', now() - interval '1 minute', $3, TRUE)
	`, agentID, runtimeID, sessionID); err != nil {
		t.Fatalf("insert withheld chat task: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content) VALUES ($1, 'user', 'next turn')
	`, sessionID); err != nil {
		t.Fatalf("insert chat message: %v", err)
	}

	dbfx.Insert(t, "agent_task_queue", testutil.Cols{"agent_id": agentID, "runtime_id": runtimeID, "status": testutil.Raw("'queued'"), "priority": testutil.Raw("1000"), "chat_session_id": sessionID})

	probe := claimOneTaskForRuntime(t, runtimeID, "chat-gap-claim-test")
	if !probe.Task.PriorSessionResumeUnavailable {
		t.Fatalf("expected chat claim to disclose the continuity gap; response task=%+v", *probe.Task)
	}
}

// TestClaimTaskByRuntime_RerunSourceRolloutMissingDisclosesGap is the
// claim-response half of MUL-5305 Must-fix 2 for manual rerun: when the exact
// source task withheld its Codex session, the rerun has nothing resumable from
// it and MUST disclose the gap rather than silently start fresh.
func TestClaimTaskByRuntime_RerunSourceRolloutMissingDisclosesGap(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "RerunGapClaimAgent", []byte("[]"))
	runtimeID := handlerTestRuntimeID(t)

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, assignee_type, assignee_id, number)
		VALUES ($1, 'rerun gap issue', 'todo', 'none', 'member', $2, 'agent', $3,
		        (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
		RETURNING id
	`, testWorkspaceID, testUserID, agentID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	// Source task whose Codex session was withheld (rollout missing).
	var srcID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at, session_rollout_missing)
		VALUES ($1, $2, $3, 'completed', 0, now() - interval '2 minutes', now() - interval '2 minutes', TRUE)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&srcID); err != nil {
		t.Fatalf("insert source task: %v", err)
	}

	// A manual rerun of that source task, queued to claim (rerun carries
	// force_fresh_session=true; the disclosure must still fire from the source).

	dbfx.Insert(t, "agent_task_queue", testutil.Cols{"agent_id": agentID, "runtime_id": runtimeID, "issue_id": issueID, "status": testutil.Raw("'queued'"), "priority": testutil.Raw("1000"), "rerun_of_task_id": srcID, "force_fresh_session": testutil.Raw("TRUE")})

	probe := claimOneTaskForRuntime(t, runtimeID, "rerun-gap-claim-test")
	if !probe.Task.PriorSessionResumeUnavailable {
		t.Fatalf("expected rerun claim to disclose the source task's continuity gap; response task=%+v", *probe.Task)
	}
}
