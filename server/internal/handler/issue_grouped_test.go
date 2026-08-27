package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func TestListGroupedIssuesAssigneePaginatesPerGroup(t *testing.T) {
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	var assigneeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ($1, $2)
		RETURNING id
	`, "Grouped Issues Test User", fmt.Sprintf("grouped-%d@multica.ai", suffix)).Scan(&assigneeID); err != nil {
		t.Fatalf("create assignee user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, assigneeID)
	})

	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'member')
	`, testWorkspaceID, assigneeID); err != nil {
		t.Fatalf("create assignee member: %v", err)
	}

	var agentID string
	agentID = dbfx.Insert(t, "agent", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Grouped Issues Test Agent", "description": testutil.Raw("''"), "runtime_mode": testutil.Raw("'cloud'"), "runtime_config": testutil.Raw("'{}'::jsonb"), "runtime_id": testRuntimeID, "visibility": testutil.Raw("'workspace'"), "max_concurrent_tasks": testutil.Raw("1"), "owner_id": testUserID})

	createIssue := func(title, assigneeType, assigneeID string, position float64, startDate *time.Time, stage *int32) string {
		t.Helper()
		var number int32
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(
				issue_counter,
				(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
			) + 1
			WHERE id = $1
			RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}

		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (
				workspace_id, title, description, status, priority,
				assignee_type, assignee_id, creator_type, creator_id,
				position, number, start_date, stage
			)
			VALUES ($1, $2, NULL, 'todo', 'none', $3, $4, 'member', $5, $6, $7, $8, $9)
			RETURNING id
		`, testWorkspaceID, title, assigneeType, assigneeID, testUserID, position, number, startDate, stage).Scan(&id); err != nil {
			t.Fatalf("create issue %q: %v", title, err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
		})
		return id
	}

	stageTwo := int32(2)
	startDate := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	createIssue("Grouped member one", "member", assigneeID, 1, &startDate, &stageTwo)
	createIssue("Grouped member two", "member", assigneeID, 2, nil, nil)
	createIssue("Grouped member three", "member", assigneeID, 3, nil, nil)
	createIssue("Grouped agent one", "agent", agentID, 1, nil, nil)

	path := fmt.Sprintf(
		"/api/issues/grouped?workspace_id=%s&group_by=assignee&statuses=todo&limit=2&assignee_filters=member:%s,agent:%s",
		testWorkspaceID,
		assigneeID,
		agentID,
	)
	w := testutil.Call(t, testHandler.ListGroupedIssues, newRequest("GET", path, nil)).Want(http.StatusOK)

	var resp GroupedIssuesResponse
	w.Decode(&resp)

	memberGroupID := "assignee:member:" + assigneeID
	agentGroupID := "assignee:agent:" + agentID
	groups := map[string]IssueAssigneeGroupResponse{}
	for _, group := range resp.Groups {
		groups[group.ID] = group
	}

	memberGroup, ok := groups[memberGroupID]
	if !ok {
		t.Fatalf("missing member group %s in %#v", memberGroupID, resp.Groups)
	}
	if memberGroup.Total != 3 || len(memberGroup.Issues) != 2 {
		t.Fatalf("member group total/page mismatch: total=%d len=%d", memberGroup.Total, len(memberGroup.Issues))
	}
	if memberGroup.Issues[0].Title != "Grouped member one" || memberGroup.Issues[1].Title != "Grouped member two" {
		t.Fatalf("member group order mismatch: %#v", memberGroup.Issues)
	}
	if memberGroup.Issues[0].Stage == nil || *memberGroup.Issues[0].Stage != stageTwo {
		t.Fatalf("member group first issue stage = %#v, want %d", memberGroup.Issues[0].Stage, stageTwo)
	}
	if memberGroup.Issues[0].StartDate == nil || *memberGroup.Issues[0].StartDate != "2026-03-01" {
		t.Fatalf("member group first issue start_date = %#v, want 2026-03-01", memberGroup.Issues[0].StartDate)
	}

	agentGroup, ok := groups[agentGroupID]
	if !ok {
		t.Fatalf("missing agent group %s in %#v", agentGroupID, resp.Groups)
	}
	if agentGroup.Total != 1 || len(agentGroup.Issues) != 1 {
		t.Fatalf("agent group total/page mismatch: total=%d len=%d", agentGroup.Total, len(agentGroup.Issues))
	}

	nextPath := fmt.Sprintf(
		"/api/issues/grouped?workspace_id=%s&group_by=assignee&statuses=todo&limit=2&offset=2&group_assignee_type=member&group_assignee_id=%s",
		testWorkspaceID,
		assigneeID,
	)
	next := testutil.Call(t, testHandler.ListGroupedIssues, newRequest("GET", nextPath, nil)).Want(http.StatusOK)

	var nextResp GroupedIssuesResponse
	next.Decode(&nextResp)
	if len(nextResp.Groups) != 1 {
		t.Fatalf("expected one next-page group, got %#v", nextResp.Groups)
	}
	if nextResp.Groups[0].ID != memberGroupID || nextResp.Groups[0].Total != 3 || len(nextResp.Groups[0].Issues) != 1 {
		t.Fatalf("unexpected next-page group: %#v", nextResp.Groups[0])
	}
	if nextResp.Groups[0].Issues[0].Title != "Grouped member three" {
		t.Fatalf("unexpected next-page issue: %#v", nextResp.Groups[0].Issues[0])
	}
}
