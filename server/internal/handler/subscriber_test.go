package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSubscriberAPI(t *testing.T) {
	ctx := context.Background()

	// Helper: create an issue for subscriber tests
	createIssue := func(t *testing.T) string {
		t.Helper()

		req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title": "Subscriber test issue",
		})
		w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
		var issue IssueResponse
		json.NewDecoder(w.Body).Decode(&issue)
		return issue.ID
	}

	// Helper: delete an issue
	deleteIssue := func(t *testing.T, issueID string) {
		t.Helper()

		req := newRequest("DELETE", "/api/issues/"+issueID, nil)
		req = withURLParam(req, "id", issueID)
		testutil.Call(t, testHandler.DeleteIssue, req)
	}

	t.Run("Subscribe", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		req := newRequest("POST", "/api/issues/"+issueID+"/subscribe", nil)
		req = withURLParam(req, "id", issueID)
		w := testutil.Call(t, testHandler.SubscribeToIssue, req).Want(http.StatusOK)

		var resp map[string]bool
		json.NewDecoder(w.Body).Decode(&resp)
		if !resp["subscribed"] {
			t.Fatal("SubscribeToIssue: expected subscribed=true")
		}

		// Verify in DB
		subscribed, err := testHandler.Queries.IsIssueSubscriber(ctx, db.IsIssueSubscriberParams{
			IssueID:  parseUUID(issueID),
			UserType: "member",
			UserID:   parseUUID(testUserID),
		})
		if err != nil {
			t.Fatalf("IsIssueSubscriber: %v", err)
		}
		if !subscribed {
			t.Fatal("expected user to be subscribed in DB")
		}
	})

	t.Run("SubscribeIdempotent", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		// Subscribe first time
		w := httptest.NewRecorder()
		req := newRequest("POST", "/api/issues/"+issueID+"/subscribe", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.SubscribeToIssue(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		// Subscribe second time — should also succeed
		w = httptest.NewRecorder()
		req = newRequest("POST", "/api/issues/"+issueID+"/subscribe", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.SubscribeToIssue(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
	})

	t.Run("ListSubscribers", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		// Subscribe first
		w := httptest.NewRecorder()
		req := newRequest("POST", "/api/issues/"+issueID+"/subscribe", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.SubscribeToIssue(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		// List
		w = httptest.NewRecorder()
		req = newRequest("GET", "/api/issues/"+issueID+"/subscribers", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.ListIssueSubscribers(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		var subscribers []SubscriberResponse
		json.NewDecoder(w.Body).Decode(&subscribers)
		if len(subscribers) == 0 {
			t.Fatal("ListIssueSubscribers: expected at least 1 subscriber")
		}
		found := false
		for _, s := range subscribers {
			if s.UserID == testUserID && s.UserType == "member" && s.Reason == "manual" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ListIssueSubscribers: expected to find test user subscriber, got %+v", subscribers)
		}
	})

	t.Run("Unsubscribe", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		// Subscribe first
		w := httptest.NewRecorder()
		req := newRequest("POST", "/api/issues/"+issueID+"/subscribe", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.SubscribeToIssue(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		// Unsubscribe
		w = httptest.NewRecorder()
		req = newRequest("POST", "/api/issues/"+issueID+"/unsubscribe", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.UnsubscribeFromIssue(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		var resp map[string]bool
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["subscribed"] {
			t.Fatal("UnsubscribeFromIssue: expected subscribed=false")
		}

		// Verify in DB
		subscribed, err := testHandler.Queries.IsIssueSubscriber(ctx, db.IsIssueSubscriberParams{
			IssueID:  parseUUID(issueID),
			UserType: "member",
			UserID:   parseUUID(testUserID),
		})
		if err != nil {
			t.Fatalf("IsIssueSubscriber: %v", err)
		}
		if subscribed {
			t.Fatal("expected user to NOT be subscribed in DB")
		}
	})

	t.Run("SubscribeCrossWorkspaceUser", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		foreignUserID := "00000000-0000-0000-0000-000000000099"

		req := newRequest("POST", "/api/issues/"+issueID+"/subscribe", map[string]any{
			"user_id":   foreignUserID,
			"user_type": "member",
		})
		req = withURLParam(req, "id", issueID)
		testutil.Call(t, testHandler.SubscribeToIssue, req).Want(http.StatusForbidden)

		subscribed, err := testHandler.Queries.IsIssueSubscriber(ctx, db.IsIssueSubscriberParams{
			IssueID:  parseUUID(issueID),
			UserType: "member",
			UserID:   parseUUID(foreignUserID),
		})
		if err != nil {
			t.Fatalf("IsIssueSubscriber: %v", err)
		}
		if subscribed {
			t.Fatal("cross-workspace user should NOT be subscribed in DB")
		}
	})

	t.Run("UnsubscribeCrossWorkspaceUser", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		foreignUserID := "00000000-0000-0000-0000-000000000099"

		req := newRequest("POST", "/api/issues/"+issueID+"/unsubscribe", map[string]any{
			"user_id":   foreignUserID,
			"user_type": "member",
		})
		req = withURLParam(req, "id", issueID)
		testutil.Call(t, testHandler.UnsubscribeFromIssue, req).Want(http.StatusForbidden)
	})

	t.Run("AgentCallerSubscribesItself", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		// Look up the agent created by the handler test fixture.
		var agentID string
		err := testPool.QueryRow(ctx,
			`SELECT id FROM agent WHERE workspace_id = $1 AND name = $2`,
			testWorkspaceID, "Handler Test Agent",
		).Scan(&agentID)
		if err != nil {
			t.Fatalf("failed to find test agent: %v", err)
		}

		// Subscribe with X-Agent-ID set — no body, so the handler must default
		// to subscribing the agent itself (not the member behind X-User-ID).
		// resolveActor requires X-Task-ID alongside X-Agent-ID to grant the
		// "agent" identity (defense against header forgery), so seed a task.
		agentTask := createHandlerTestTaskForAgent(t, agentID)
		w := httptest.NewRecorder()
		req := newRequest("POST", "/api/issues/"+issueID+"/subscribe", nil)
		req = withURLParam(req, "id", issueID)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", agentTask)
		testHandler.SubscribeToIssue(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		agentSubscribed, err := testHandler.Queries.IsIssueSubscriber(ctx, db.IsIssueSubscriberParams{
			IssueID:  parseUUID(issueID),
			UserType: "agent",
			UserID:   parseUUID(agentID),
		})
		if err != nil {
			t.Fatalf("IsIssueSubscriber (agent): %v", err)
		}
		if !agentSubscribed {
			t.Fatal("expected agent to be subscribed in DB when X-Agent-ID is set")
		}

		memberSubscribed, err := testHandler.Queries.IsIssueSubscriber(ctx, db.IsIssueSubscriberParams{
			IssueID:  parseUUID(issueID),
			UserType: "member",
			UserID:   parseUUID(testUserID),
		})
		if err != nil {
			t.Fatalf("IsIssueSubscriber (member): %v", err)
		}
		if memberSubscribed {
			t.Fatal("member must not be auto-subscribed when caller is an agent")
		}

		// Unsubscribe with X-Agent-ID set — same default-to-caller expectation.
		// Re-use the same task as the subscribe call; resolveActor only
		// validates that the task belongs to the agent, not which task.
		w = httptest.NewRecorder()
		req = newRequest("POST", "/api/issues/"+issueID+"/unsubscribe", nil)
		req = withURLParam(req, "id", issueID)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", agentTask)
		testHandler.UnsubscribeFromIssue(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		agentSubscribed, err = testHandler.Queries.IsIssueSubscriber(ctx, db.IsIssueSubscriberParams{
			IssueID:  parseUUID(issueID),
			UserType: "agent",
			UserID:   parseUUID(agentID),
		})
		if err != nil {
			t.Fatalf("IsIssueSubscriber (agent, after unsubscribe): %v", err)
		}
		if agentSubscribed {
			t.Fatal("expected agent to be unsubscribed in DB when X-Agent-ID is set")
		}
	})

	t.Run("ListAfterUnsubscribe", func(t *testing.T) {
		issueID := createIssue(t)
		defer deleteIssue(t, issueID)

		// Subscribe
		w := httptest.NewRecorder()
		req := newRequest("POST", "/api/issues/"+issueID+"/subscribe", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.SubscribeToIssue(w, req)

		// Unsubscribe
		w = httptest.NewRecorder()
		req = newRequest("POST", "/api/issues/"+issueID+"/unsubscribe", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.UnsubscribeFromIssue(w, req)

		// List should be empty
		w = httptest.NewRecorder()
		req = newRequest("GET", "/api/issues/"+issueID+"/subscribers", nil)
		req = withURLParam(req, "id", issueID)
		testHandler.ListIssueSubscribers(w, req)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")

		var subscribers []SubscriberResponse
		json.NewDecoder(w.Body).Decode(&subscribers)
		if len(subscribers) != 0 {
			t.Fatalf("ListIssueSubscribers: expected 0 subscribers after unsubscribe, got %d", len(subscribers))
		}
	})
}
