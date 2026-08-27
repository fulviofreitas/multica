package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	testutil "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Deleting a chat session cancels the session's active tasks, and every
// subscriber that shows something for a running task — the web queue, and the
// Slack and Lark processing indicators — learns about it from task:cancelled
// and from nothing else.
//
// It was never published. BroadcastCancelledTasks resolved each task's
// workspace by reading the task's chat_session, which the transaction it runs
// after has just deleted; the resolution returned "", and publishTaskEvent
// drops an event with no workspace before it reaches the bus. So the rows went
// to 'cancelled' in silence and the event reached no subscriber at all — not
// the web queue, not the realtime fanout, not a channel adapter — leaving the
// deleted conversation showing a run that no longer existed. The workspace now
// comes from the caller, which knows it.
func TestDeleteChatSession_BroadcastsTaskCancelled(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)

	var sessionID string
	sessionID = dbfx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": testutil.Raw("'delete cancels its tasks'"), "status": testutil.Raw("'active'")})

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0)
		RETURNING id
	`, agentID, runtimeID, sessionID).Scan(&taskID); err != nil {
		t.Fatalf("setup: create queued chat task: %v", err)
	}

	var mu sync.Mutex
	var got []events.Event
	testHandler.Bus.Subscribe(protocol.EventTaskCancelled, func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		if e.ChatSessionID == sessionID {
			got = append(got, e)
		}
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/chat/sessions/"+sessionID, nil)
	req.Header.Set("X-User-ID", testUserID)
	req = withURLParam(req, "sessionId", sessionID)
	req = withChatTestWorkspaceCtx(t, req)
	testutil.Call(t, testHandler.DeleteChatSession, req).Want(http.StatusNoContent)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("deleting the session cancelled its queued task and published %d "+
			"task:cancelled events — every channel indicator for that conversation "+
			"is still spinning for a run that no longer exists", len(got))
	}
	e := got[0]
	if e.WorkspaceID != testWorkspaceID {
		t.Errorf("workspace = %q, want %q: the realtime fanout routes on it", e.WorkspaceID, testWorkspaceID)
	}
	if e.TaskID != taskID {
		t.Errorf("task id = %q, want %q", e.TaskID, taskID)
	}
	// The channel adapters find their conversation through the session id, so a
	// payload without it reaches them as an issue task and is ignored. The row
	// still carries it because the cancel returned it before the delete; the
	// stored row's own chat_session_id is NULL by now (ON DELETE SET NULL).
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want the map a cancel is published with", e.Payload)
	}
	if payload["chat_session_id"] != sessionID {
		t.Errorf("payload chat_session_id = %v, want %q", payload["chat_session_id"], sessionID)
	}
	if payload["status"] != "cancelled" {
		t.Errorf("payload status = %v, want cancelled", payload["status"])
	}
}
