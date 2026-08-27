package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func TestDeriveSquadMemberStatus(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	online := pgtype.Text{String: "online", Valid: true}
	offline := pgtype.Text{String: "offline", Valid: true}
	missing := pgtype.Text{}

	tsAgo := func(d time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: now.Add(-d), Valid: true}
	}
	tsNone := pgtype.Timestamptz{}

	cases := []struct {
		name           string
		archived       bool
		runtimeStatus  pgtype.Text
		lastSeen       pgtype.Timestamptz
		hasWorkingTask bool
		want           string
	}{
		{"working wins over offline runtime", false, offline, tsAgo(time.Hour), true, "working"},
		{"working wins over missing runtime", false, missing, tsNone, true, "working"},
		{"online runtime, no task", false, online, tsAgo(2 * time.Second), false, "idle"},
		{"offline runtime, recent heartbeat", false, offline, tsAgo(2 * time.Minute), false, "unstable"},
		{"offline runtime, stale heartbeat", false, offline, tsAgo(2 * time.Hour), false, "offline"},
		{"offline runtime, no heartbeat", false, offline, tsNone, false, "offline"},
		{"no runtime row", false, missing, tsNone, false, "offline"},
		// Archived agents always report archived regardless of any leftover
		// runtime row or task — they should appear in the squad listing
		// but never look like they're still working or merely offline.
		{"archived agent with active task", true, online, tsAgo(time.Second), true, "archived"},
		{"archived agent with online runtime", true, online, tsAgo(time.Second), false, "archived"},
		{"archived agent already offline", true, offline, tsAgo(time.Hour), false, "archived"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveSquadMemberStatus(tc.archived, tc.runtimeStatus, tc.lastSeen, tc.hasWorkingTask, now)
			if got != tc.want {
				t.Fatalf("deriveSquadMemberStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListSquadMemberStatusRetainsWaiterIssueWithoutWorkingStatus(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "squad-status-local-directory-waiter", []byte(`{}`))
	squadID := createCommentTriggerPreviewSquad(t, "Squad Status Local Directory Waiter", agentID)
	issueID := createCommentTriggerPreviewIssue(t, "Directory waiter remains visible", "agent", agentID)

	dbfx.InsertNoID(t, "squad_member", testutil.Cols{"squad_id": squadID, "member_type": testutil.Raw("'agent'"), "member_id": agentID}, "squad_id = $1 AND member_id = $2", squadID, agentID)

	var taskID string
	taskID = dbfx.Insert(t, "agent_task_queue", testutil.Cols{"agent_id": agentID, "runtime_id": handlerTestRuntimeID(t), "issue_id": issueID, "status": testutil.Raw("'waiting_local_directory'"), "priority": testutil.Raw("0"), "dispatched_at": testutil.Raw("now()")})

	rows, err := testHandler.Queries.ListSquadMemberStatusRows(ctx, parseUUID(squadID))
	if err != nil {
		t.Fatalf("ListSquadMemberStatusRows with waiter: %v", err)
	}
	if len(rows) != 1 || !rows[0].TaskID.Valid || rows[0].TaskStatus.String != "waiting_local_directory" {
		t.Fatalf("waiter-only squad member rows = %+v, want the waiting task retained", rows)
	}

	assertSquadMember := func(wantWorking bool) {
		t.Helper()
		w := testutil.Call(t, testHandler.ListSquadMemberStatus, squadScopeReq(
			"",
			http.MethodGet,
			"/api/squads/"+squadID+"/members/status",
			nil,
			map[string]string{"id": squadID},
		)).Want(http.StatusOK)
		var resp SquadMemberStatusListResponse
		w.Decode(&resp)
		if len(resp.Members) != 1 {
			t.Fatalf("squad members = %+v, want one member", resp.Members)
		}
		member := resp.Members[0]
		if member.Status == nil {
			t.Fatal("agent member status is nil")
		}
		if gotWorking := *member.Status == "working"; gotWorking != wantWorking {
			t.Fatalf("member status = %q, want working=%t", *member.Status, wantWorking)
		}
		if len(member.ActiveIssues) != 1 || member.ActiveIssues[0].IssueID != issueID {
			t.Fatalf("active issues = %+v, want waiting issue %s", member.ActiveIssues, issueID)
		}
	}
	assertSquadMember(false)

	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'dispatched' WHERE id = $1
	`, taskID); err != nil {
		t.Fatalf("promote waiter to dispatched: %v", err)
	}
	rows, err = testHandler.Queries.ListSquadMemberStatusRows(ctx, parseUUID(squadID))
	if err != nil {
		t.Fatalf("ListSquadMemberStatusRows with dispatched task: %v", err)
	}
	if len(rows) != 1 || !rows[0].TaskID.Valid {
		t.Fatalf("dispatched squad member rows = %+v, want one working task row", rows)
	}
	assertSquadMember(true)
}
