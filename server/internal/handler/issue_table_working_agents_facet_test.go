package handler

import (
	"context"
	"net/http"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// workingAgentsFacetCounts posts one facets request and returns the
// working-agents facet as agent id -> running task count.
func workingAgentsFacetCounts(
	t *testing.T,
	request *http.Request,
) map[string]int64 {
	t.Helper()

	recorder := testutil.Call(t, testHandler.ListIssueTableFacets, request).Want(http.StatusOK)
	var response issueTableFacetsResponse
	recorder.Decode(&response)
	counts := map[string]int64{}
	for _, facet := range response.Facets {
		if facet.Kind != "working_agents" {
			continue
		}
		for _, value := range facet.Values {
			counts[value.Key] = value.Count
		}
	}
	return counts
}

func workingAgentsFacetRequest(scope, filters map[string]any) *http.Request {
	if filters == nil {
		filters = map[string]any{}
	}
	return newRequest(http.MethodPost, "/api/issues/table/facets", map[string]any{
		"query": map[string]any{
			"scope":   scope,
			"filters": filters,
			"sort":    map[string]any{"field": "position", "direction": "asc"},
		},
		"facets":        []map[string]any{{"kind": "working_agents"}},
		"include_total": false,
	})
}

// MUL-5525. The header chip's count used to come from a workspace-wide
// projection while the list came from the surface's own compiled query, so a
// project page could advertise "2 agents working" and then open an empty list.
// The `working_agents` facet answers the same question against the same scope
// and filters the rows come from.
func TestIssueTableWorkingAgentsFacetFollowsSurfaceScopeAndFilters(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	insideAgentID := createHandlerTestAgent(t, "facet-working-inside", []byte(`{}`))
	outsideAgentID := createHandlerTestAgent(t, "facet-working-outside", []byte(`{}`))
	idleAgentID := createHandlerTestAgent(t, "facet-working-idle", []byte(`{}`))

	var projectID string
	projectID = dbfx.Insert(t, "project", testutil.Cols{"workspace_id": testWorkspaceID, "title": testutil.Raw("'Working Agents Facet Project'")})

	var finalNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 4
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&finalNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}

	insertIssue := func(title, status string, number int, project any, parent any) string {
		t.Helper()
		var issueID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (
				workspace_id, title, status, priority, creator_type, creator_id,
				project_id, parent_issue_id, position, number
			)
			VALUES ($1, $2, $3, 'none', 'member', $4, $5, $6, $7, $8)
			RETURNING id
		`,
			testWorkspaceID, title, status, testUserID, project, parent, number, number,
		).Scan(&issueID); err != nil {
			t.Fatalf("insert issue %q: %v", title, err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
		})
		return issueID
	}

	inProjectTodoID := insertIssue("in project, todo", "todo", finalNumber-3, projectID, nil)
	inProjectDoneID := insertIssue("in project, done", "done", finalNumber-2, projectID, nil)
	outsideProjectID := insertIssue("outside the project", "todo", finalNumber-1, nil, nil)
	subIssueID := insertIssue("in project, sub-issue", "todo", finalNumber, projectID, inProjectTodoID)

	// insideAgent holds two running tasks inside the project (one on each
	// status) plus one on the sub-issue; outsideAgent works only outside it;
	// idleAgent has no running task at all.
	createHandlerTestTaskForAgentOnIssue(t, insideAgentID, inProjectTodoID)
	createHandlerTestTaskForAgentOnIssue(t, insideAgentID, inProjectDoneID)
	createHandlerTestTaskForAgentOnIssue(t, insideAgentID, subIssueID)
	createHandlerTestTaskForAgentOnIssue(t, outsideAgentID, outsideProjectID)

	projectScope := map[string]any{"kind": "project", "project_id": projectID}

	t.Run("project scope excludes agents working elsewhere", func(t *testing.T) {
		counts := workingAgentsFacetCounts(t, workingAgentsFacetRequest(projectScope, nil))
		if counts[insideAgentID] != 3 {
			t.Errorf("inside agent count = %d, want 3", counts[insideAgentID])
		}
		if _, present := counts[outsideAgentID]; present {
			t.Errorf("agent working outside the project was counted: %s", outsideAgentID)
		}
		if _, present := counts[idleAgentID]; present {
			t.Errorf("agent with no running task was counted: %s", idleAgentID)
		}
	})

	t.Run("a status filter narrows the count like it narrows the rows", func(t *testing.T) {
		counts := workingAgentsFacetCounts(t, workingAgentsFacetRequest(projectScope, map[string]any{
			"statuses": []string{"done"},
		}))
		if counts[insideAgentID] != 1 {
			t.Errorf("inside agent count under status=done = %d, want 1", counts[insideAgentID])
		}
	})

	t.Run("hiding sub-issues drops their tasks from the count", func(t *testing.T) {
		counts := workingAgentsFacetCounts(t, workingAgentsFacetRequest(projectScope, map[string]any{
			"include_sub_issues": false,
		}))
		if counts[insideAgentID] != 2 {
			t.Errorf("inside agent count without sub-issues = %d, want 2", counts[insideAgentID])
		}
	})

	t.Run("a filter that matches nothing reports a real zero", func(t *testing.T) {
		counts := workingAgentsFacetCounts(t, workingAgentsFacetRequest(projectScope, map[string]any{
			"statuses": []string{"cancelled"},
		}))
		if len(counts) != 0 {
			t.Errorf("counts = %v, want empty", counts)
		}
	})

	// The facet drops only its OWN dimension, so the answer is identical whether
	// the agents-working filter is on or off. That is what lets the chip's number
	// stay put when you click it.
	t.Run("the working filter itself does not change the answer", func(t *testing.T) {
		off := workingAgentsFacetCounts(t, workingAgentsFacetRequest(projectScope, nil))
		on := workingAgentsFacetCounts(t, workingAgentsFacetRequest(projectScope, map[string]any{
			"working_issue_ids": []string{inProjectTodoID},
		}))
		if off[insideAgentID] != on[insideAgentID] {
			t.Errorf("count moved with the filter: off=%d on=%d", off[insideAgentID], on[insideAgentID])
		}
		matchNone := workingAgentsFacetCounts(t, workingAgentsFacetRequest(projectScope, map[string]any{
			"working_issue_ids": []string{},
		}))
		if matchNone[insideAgentID] != off[insideAgentID] {
			t.Errorf(
				"an explicit match-none working filter changed the count: %d vs %d",
				matchNone[insideAgentID], off[insideAgentID],
			)
		}
	})

	t.Run("workspace scope still sees both agents", func(t *testing.T) {
		counts := workingAgentsFacetCounts(t, workingAgentsFacetRequest(
			map[string]any{"kind": "workspace"}, nil,
		))
		if counts[insideAgentID] != 3 || counts[outsideAgentID] != 1 {
			t.Errorf(
				"workspace counts inside=%d outside=%d, want 3 and 1",
				counts[insideAgentID], counts[outsideAgentID],
			)
		}
	})
}

// The facet keys are agent ids, so it discloses agent identity and has to pass
// the same visibility gate as /api/working-agents: a plain member must not learn
// that someone else's private agent exists, not even as a count.
func TestIssueTableWorkingAgentsFacetHidesInaccessibleAgents(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	privateAgentID, ownerID, plainMemberID := privateAgentTestFixture(t)

	var finalNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 1
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&finalNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}

	var issueID string
	issueID = dbfx.Insert(t, "issue", testutil.Cols{"workspace_id": testWorkspaceID, "title": testutil.Raw("'worked on by a private agent'"), "status": testutil.Raw("'todo'"), "priority": testutil.Raw("'none'"), "creator_type": testutil.Raw("'member'"), "creator_id": testUserID, "position": finalNumber, "number": finalNumber})
	createHandlerTestTaskForAgentOnIssue(t, privateAgentID, issueID)

	facetRequest := func(userID string) *http.Request {
		request := workingAgentsFacetRequest(map[string]any{"kind": "workspace"}, nil)
		request.Header.Set("X-User-ID", userID)
		return request
	}

	if counts := workingAgentsFacetCounts(t, facetRequest(plainMemberID)); len(counts) != 0 {
		t.Errorf("plain member saw inaccessible agents: %v", counts)
	}
	if counts := workingAgentsFacetCounts(t, facetRequest(ownerID)); counts[privateAgentID] != 1 {
		t.Errorf("agent owner count = %d, want 1", counts[privateAgentID])
	}
}
