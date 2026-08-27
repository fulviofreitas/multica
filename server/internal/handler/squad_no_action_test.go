package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type runningSquadLeaderTaskFixture struct {
	IssueID          string
	SquadID          string
	LeaderID         string
	TaskID           string
	TriggerCommentID string
}

func newRunningSquadLeaderTaskFixture(t *testing.T) runningSquadLeaderTaskFixture {
	t.Helper()

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)

	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, fx.LeaderID).Scan(&runtimeID)

	triggerCommentID := dbfx.Comment(t, issueID, "LGTM")

	// is_leader_task + squad_id are what RecordSquadLeaderEvaluation authorizes
	// against (MUL-6622); a leader task without them is not a leader turn.
	taskID := dbfx.Task(t, fx.LeaderID, testutil.Cols{
		"runtime_id":         runtimeID,
		"issue_id":           issueID,
		"trigger_comment_id": triggerCommentID,
		"status":             "running",
		"started_at":         testutil.Raw("now()"),
		"is_leader_task":     true,
		"squad_id":           fx.SquadID,
	})

	return runningSquadLeaderTaskFixture{
		IssueID:          issueID,
		SquadID:          fx.SquadID,
		LeaderID:         fx.LeaderID,
		TaskID:           taskID,
		TriggerCommentID: triggerCommentID,
	}
}

func recordSquadLeaderEvaluationForTask(t *testing.T, fx runningSquadLeaderTaskFixture, outcome string) {
	t.Helper()
	recordSquadLeaderEvaluationForTaskWithHeader(t, fx, outcome, fx.TaskID)
}

func recordSquadLeaderEvaluationForTaskWithHeader(t *testing.T, fx runningSquadLeaderTaskFixture, outcome, taskIDHeader string) {
	t.Helper()

	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/squad-evaluated", map[string]any{
		"outcome": outcome,
		"reason":  "test reason",
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", taskIDHeader)

	testutil.Call(t, testHandler.RecordSquadLeaderEvaluation, r).Want(http.StatusCreated)
}

func completeRunningTask(t *testing.T, fx runningSquadLeaderTaskFixture, output string) {
	t.Helper()

	r := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+fx.TaskID+"/complete",
		map[string]any{"output": output},
		testWorkspaceID, "legit-daemon")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", fx.TaskID)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	testutil.Call(t, testHandler.CompleteTask, r).Want(http.StatusOK)
}

func countAgentCommentsForIssue(t *testing.T, issueID, agentID string) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM comment
		WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2
	`, issueID, agentID).Scan(&count); err != nil {
		t.Fatalf("count agent comments: %v", err)
	}
	return count
}

func TestCompleteTask_SquadLeaderNoActionDoesNotSynthesizeComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTask(t, fx, "no_action")

	completeRunningTask(t, fx, "No action needed. Exiting silently.")

	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected no squad leader comment after no_action completion, got %d", got)
	}
}

func TestCompleteTask_SquadLeaderNoActionCanonicalizesTaskID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTaskWithHeader(t, fx, "no_action", strings.ToUpper(fx.TaskID))

	completeRunningTask(t, fx, "No action needed. Exiting silently.")

	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected no comment when no_action was recorded with uppercase task id header, got %d", got)
	}
}

func TestCompleteTask_SquadLeaderActionStillSynthesizesComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTask(t, fx, "action")

	completeRunningTask(t, fx, "Delegated the review.")

	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 1 {
		t.Fatalf("expected action completion to synthesize one comment, got %d", got)
	}
}

func TestCreateComment_SquadLeaderNoActionRejectsComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTask(t, fx, "no_action")

	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/comments", map[string]any{
		"content":   "No action needed.",
		"parent_id": fx.TriggerCommentID,
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", fx.TaskID)

	w := testutil.Call(t, testHandler.CreateComment, r).Want(http.StatusConflict)
	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected rejected no_action comment not to be stored, got %d", got)
	}

	var body map[string]any
	w.Decode(&body)
	if body["error"] == "" {
		t.Fatalf("expected error message in response, got %v", body)
	}
}
