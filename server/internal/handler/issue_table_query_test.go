package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

type issueTableEnrichmentFailTxStarter struct {
	inner           txStarter
	labelCalls      *int
	tableQueryCalls *int
	facetQueryCalls *int
	rowQuerySQL     *string
	groupQuerySQL   *string
}

func (s issueTableEnrichmentFailTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &issueTableEnrichmentFailTx{
		Tx:              tx,
		labelCalls:      s.labelCalls,
		tableQueryCalls: s.tableQueryCalls,
		facetQueryCalls: s.facetQueryCalls,
		rowQuerySQL:     s.rowQuerySQL,
		groupQuerySQL:   s.groupQuerySQL,
	}, nil
}

type issueTableEnrichmentFailTx struct {
	pgx.Tx
	labelCalls      *int
	tableQueryCalls *int
	facetQueryCalls *int
	rowQuerySQL     *string
	groupQuerySQL   *string
}

func (tx *issueTableEnrichmentFailTx) recordTableQuery(sql string) {
	if tx.tableQueryCalls != nil {
		if strings.Contains(sql, "page AS MATERIALIZED (") ||
			strings.Contains(sql, "SELECT COUNT(*)::bigint FROM issue i WHERE") {
			*tx.tableQueryCalls = *tx.tableQueryCalls + 1
		}
	}
	if tx.facetQueryCalls != nil && strings.Contains(sql, "GROUP BY GROUPING SETS") {
		*tx.facetQueryCalls = *tx.facetQueryCalls + 1
	}
	if tx.rowQuerySQL != nil && strings.Contains(sql, "page AS MATERIALIZED (") {
		*tx.rowQuerySQL = sql
	}
	if tx.groupQuerySQL != nil && strings.Contains(sql, "WITH grouped AS (") {
		*tx.groupQuerySQL = sql
	}
}

func (tx *issueTableEnrichmentFailTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.recordTableQuery(sql)
	if strings.Contains(sql, "ListLabelsForIssues") {
		*tx.labelCalls = *tx.labelCalls + 1
		// A real PostgreSQL statement error poisons the transaction until
		// rollback. Before enrichment moved after Commit, this turned the
		// otherwise successful row window into a 500.
		_, err := tx.Tx.Exec(ctx, "SELECT * FROM issue_table_missing_enrichment_relation")
		return nil, err
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *issueTableEnrichmentFailTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.recordTableQuery(sql)
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func TestCanonicalIssueTableFingerprintNormalizesSetLikeArrays(t *testing.T) {
	left := issueTableQuerySpec{
		Scope: issueTableScope{Kind: "workspace", AssigneeTypes: []string{"agent", "member", "agent"}},
		Filters: issueTableFiltersRequest{
			Statuses:   []string{"todo", "backlog", "todo"},
			ProjectIDs: []string{"b", "a"},
		},
		Sort: issueTableSortRequest{Field: "title", Direction: "asc"},
	}
	right := issueTableQuerySpec{
		Scope: issueTableScope{Kind: "workspace", AssigneeTypes: []string{"member", "agent"}},
		Filters: issueTableFiltersRequest{
			Statuses:   []string{"backlog", "todo"},
			ProjectIDs: []string{"a", "b"},
		},
		Sort: issueTableSortRequest{Field: "title", Direction: "asc"},
	}
	leftFingerprint, err := canonicalIssueTableFingerprint("workspace-1", left)
	if err != nil {
		t.Fatal(err)
	}
	rightFingerprint, err := canonicalIssueTableFingerprint("workspace-1", right)
	if err != nil {
		t.Fatal(err)
	}
	if leftFingerprint != rightFingerprint {
		t.Fatalf("equivalent table queries produced different fingerprints: %s != %s", leftFingerprint, rightFingerprint)
	}
}

func TestCanonicalIssueTableFingerprintBindsWorkspace(t *testing.T) {
	spec := issueTableQuerySpec{
		Scope: issueTableScope{Kind: "workspace"},
		Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
	}
	left, err := canonicalIssueTableFingerprint("workspace-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalIssueTableFingerprint("workspace-2", spec)
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("equivalent queries in different workspaces produced the same fingerprint")
	}
}

func TestIssueTableExplicitEmptyAssigneesMatchesNone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	base := issueTableQuerySpec{
		Scope: issueTableScope{Kind: "workspace"},
		Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
	}
	withEmpty := base
	withEmpty.Filters.Assignees = []issueTableActorRef{}

	unfilteredFingerprint, err := canonicalIssueTableFingerprint(testWorkspaceID, base)
	if err != nil {
		t.Fatal(err)
	}
	emptyFingerprint, err := canonicalIssueTableFingerprint(testWorkspaceID, withEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if unfilteredFingerprint == emptyFingerprint {
		t.Fatal("explicit empty assignees must not share the unfiltered cursor fingerprint")
	}

	w := httptest.NewRecorder()
	compiled, ok := testHandler.compileIssueTableQuery(
		w,
		newRequest(http.MethodPost, "/api/issues/table/rows", nil),
		withEmpty,
	)
	if !ok {
		t.Fatalf("compile failed: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(compiled.where, "FALSE") {
		t.Fatalf("explicit empty assignees predicate = %q, want FALSE", compiled.where)
	}
}

func TestIssueTableProjectScopeAssigneeTypes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	spec := issueTableQuerySpec{
		Scope: issueTableScope{
			Kind:          "project",
			ProjectID:     "00000000-0000-0000-0000-000000000001",
			AssigneeTypes: []string{"agent", "squad"},
		},
		Sort: issueTableSortRequest{Field: "position", Direction: "asc"},
	}

	w := httptest.NewRecorder()
	compiled, ok := testHandler.compileIssueTableQuery(
		w,
		newRequest(http.MethodPost, "/api/issues/table/rows", nil),
		spec,
	)
	if !ok {
		t.Fatalf("compile failed: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(compiled.where, "i.project_id") {
		t.Fatalf("project predicate missing: %q", compiled.where)
	}
	if !strings.Contains(compiled.where, "i.assignee_type = ANY") {
		t.Fatalf("assignee-type narrowing missing on project scope: %q", compiled.where)
	}

	bad := spec
	bad.Scope.AssigneeTypes = []string{"martian"}
	w = httptest.NewRecorder()
	if _, ok := testHandler.compileIssueTableQuery(
		w,
		newRequest(http.MethodPost, "/api/issues/table/rows", nil),
		bad,
	); ok {
		t.Fatal("invalid assignee_types must be rejected on project scope")
	}
}

func TestIssueTableWorkingIssueIDsAreExplicitAndAssigneeIndependent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	base := issueTableQuerySpec{
		Scope: issueTableScope{Kind: "workspace"},
		Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
	}
	withEmpty := base
	withEmpty.Filters.WorkingIssueIDs = []string{}

	unfilteredFingerprint, err := canonicalIssueTableFingerprint(testWorkspaceID, base)
	if err != nil {
		t.Fatal(err)
	}
	emptyFingerprint, err := canonicalIssueTableFingerprint(testWorkspaceID, withEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if unfilteredFingerprint == emptyFingerprint {
		t.Fatal("explicit empty working_issue_ids must not share the unfiltered cursor fingerprint")
	}

	w := httptest.NewRecorder()
	compiled, ok := testHandler.compileIssueTableQuery(
		w,
		newRequest(http.MethodPost, "/api/issues/table/rows", nil),
		withEmpty,
	)
	if !ok {
		t.Fatalf("compile empty filter: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(compiled.where, "FALSE") {
		t.Fatalf("explicit empty working_issue_ids predicate = %q, want FALSE", compiled.where)
	}

	withIssue := base
	withIssue.Filters.WorkingIssueIDs = []string{
		"00000000-0000-4000-8000-000000000001",
	}
	w = httptest.NewRecorder()
	compiled, ok = testHandler.compileIssueTableQuery(
		w,
		newRequest(http.MethodPost, "/api/issues/table/rows", nil),
		withIssue,
	)
	if !ok {
		t.Fatalf("compile issue filter: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(compiled.where, "i.id = ANY(") {
		t.Errorf("working-issue predicate = %q, want issue-id membership", compiled.where)
	}
	if strings.Contains(compiled.where, "i.assignee_id") {
		t.Fatalf("working-issue predicate must not filter issue assignees: %q", compiled.where)
	}
}

func TestIssueTableWorkingIssueProjectionMatchesTaskIssuesNotAssignees(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	workingAgentID := createHandlerTestAgent(t, "table-working-task-agent", []byte(`{}`))
	otherAgentID := createHandlerTestAgent(t, "table-other-working-agent", []byte(`{}`))

	var finalNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 3
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&finalNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}

	insertIssue := func(title string, number int, assigneeType string, assigneeID any) string {
		t.Helper()
		var issueID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (
				workspace_id, title, status, priority, creator_type, creator_id,
				assignee_type, assignee_id, position, number
			)
			VALUES ($1, $2, 'todo', 'none', 'member', $3, $4, $5, $6, $7)
			RETURNING id
		`,
			testWorkspaceID,
			title,
			testUserID,
			assigneeType,
			assigneeID,
			number,
			number,
		).Scan(&issueID); err != nil {
			t.Fatalf("insert issue %q: %v", title, err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
		})
		return issueID
	}

	assignedOnlyIssueID := insertIssue(
		"assigned to working agent but not being edited",
		finalNumber-2,
		"agent",
		workingAgentID,
	)
	editedIssueID := insertIssue(
		"being edited by working agent but assigned to member",
		finalNumber-1,
		"member",
		testUserID,
	)
	otherAgentIssueID := insertIssue(
		"being edited by another agent",
		finalNumber,
		"agent",
		workingAgentID,
	)
	createHandlerTestTaskForAgentOnIssue(t, workingAgentID, editedIssueID)
	createHandlerTestTaskForAgentOnIssue(t, otherAgentID, otherAgentIssueID)

	workingRecorder := testutil.Call(t, testHandler.ListWorkspaceWorkingAgents, newRequest(http.MethodGet, "/api/working-agents?type=issue", nil)).Want(http.StatusOK)
	var workingAgents []WorkspaceWorkingAgent
	workingRecorder.Decode(&workingAgents)
	workingIssueIDs := make([]string, 0, 2)
	for _, agent := range workingAgents {
		if agent.ID == workingAgentID || agent.ID == otherAgentID {
			workingIssueIDs = append(workingIssueIDs, agent.IssueIDs...)
		}
	}

	listRows := func(issueIDs []string) issueTableRowsResponse {
		t.Helper()
		recorder := testutil.Call(t, testHandler.ListIssueTableRows, newRequest(http.MethodPost, "/api/issues/table/rows", map[string]any{
			"query": map[string]any{
				"scope": map[string]any{"kind": "workspace"},
				"filters": map[string]any{
					// A map intentionally preserves [] in JSON. Marshaling
					// issueTableFiltersRequest directly would apply its
					// omitempty tag and turn the match-none case into no filter.
					"working_issue_ids": issueIDs,
				},
				"sort": map[string]any{
					"field":     "position",
					"direction": "asc",
				},
			},
			"group":     map[string]any{"kind": "none"},
			"hierarchy": map[string]any{"enabled": false},
			"page":      map[string]any{"limit": 10},
		})).Want(http.StatusOK)
		var response issueTableRowsResponse
		recorder.Decode(&response)
		return response
	}

	response := listRows(workingIssueIDs)
	if response.Total != 2 || len(response.Rows) != 2 {
		t.Fatalf("working rows total=%d rows=%d, want two", response.Total, len(response.Rows))
	}
	gotIssueIDs := make(map[string]struct{}, len(response.Rows))
	for _, row := range response.Rows {
		gotIssueIDs[row.Issue.ID] = struct{}{}
	}
	if _, ok := gotIssueIDs[editedIssueID]; !ok {
		t.Errorf("missing issue edited by selected working agent: %s", editedIssueID)
	}
	if _, ok := gotIssueIDs[otherAgentIssueID]; !ok {
		t.Errorf("missing issue edited by other working agent: %s", otherAgentIssueID)
	}
	if _, ok := gotIssueIDs[assignedOnlyIssueID]; ok {
		t.Errorf("included assigned-only issue without a running task: %s", assignedOnlyIssueID)
	}

	empty := listRows([]string{})
	if empty.Total != 0 || len(empty.Rows) != 0 {
		t.Fatalf("explicit empty working-issue filter returned total=%d rows=%d", empty.Total, len(empty.Rows))
	}
}

func TestIssueTableCursorRejectsAnotherQuery(t *testing.T) {
	groupKey := "status:todo"
	cursor := issueTableCursor{
		Version:          1,
		QueryFingerprint: "sha256:old",
		GroupKey:         &groupKey,
	}
	w := httptest.NewRecorder()
	if issueTableCursorMatches(w, &cursor, "sha256:new", &groupKey, nil) {
		t.Fatal("cursor from another query unexpectedly matched")
	}
	testutil.Equal(t, w.Code, http.StatusConflict, "HTTP status")
}

func TestIssueTablePositionCursorIncludesIndexableLowerBound(t *testing.T) {
	cursorValue := "90000"
	cursor := issueTableCursor{
		SortValue:    &cursorValue,
		RowCreatedAt: "2026-01-01T00:00:00Z",
		RowID:        "00000000-0000-4000-8000-000000000001",
	}
	args := make([]any, 0, 3)
	predicate, ok := (resolvedIssueTableSort{
		expression: "i.position",
		direction:  "asc",
		castType:   "double precision",
	}).cursorPredicate(httptest.NewRecorder(), &cursor, func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	})
	if !ok {
		t.Fatal("valid position cursor was rejected")
	}
	if !strings.Contains(predicate, "i.position >= $3::double precision") {
		t.Fatalf("position cursor is missing its indexable lower bound: %s", predicate)
	}
}

func TestIssueTableLastActivityDefaultsToIndexedOrder(t *testing.T) {
	w := httptest.NewRecorder()
	sort, ok := testHandler.issueTableOrderBy(
		w,
		newRequest(http.MethodPost, "/api/issues/table/rows", nil),
		testWorkspaceID,
		issueTableSortRequest{Field: "last_activity"},
	)
	if !ok {
		t.Fatalf("last_activity sort rejected: status=%d body=%s", w.Code, w.Body.String())
	}
	if got, want := sort.orderBy(), "i.last_activity_at DESC NULLS LAST, i.id DESC"; got != want {
		t.Fatalf("orderBy = %q, want %q", got, want)
	}

	sortValue := "2026-08-19T05:04:03.123456Z"
	cursor := issueTableCursor{
		SortValue: &sortValue,
		RowID:     "00000000-0000-4000-8000-000000000001",
	}
	args := make([]any, 0, 2)
	predicate, ok := sort.cursorPredicate(w, &cursor, func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	})
	if !ok {
		t.Fatalf("valid last_activity cursor rejected: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(predicate, "created_at") {
		t.Fatalf("last_activity cursor unexpectedly uses created_at tie-break: %s", predicate)
	}
	if !strings.Contains(predicate, "i.id < $1::uuid") {
		t.Fatalf("last_activity cursor is missing id tie-break: %s", predicate)
	}
}

func TestIssueTableGroupIdentityBindsIncludeEmpty(t *testing.T) {
	withoutEmpty := issueTableGroupIdentity(issueTableGroupSpec{
		Kind:       "property",
		PropertyID: "00000000-0000-4000-8000-000000000001",
	})
	withEmpty := issueTableGroupIdentity(issueTableGroupSpec{
		Kind:         "property",
		PropertyID:   "00000000-0000-4000-8000-000000000001",
		IncludeEmpty: true,
	})
	if withoutEmpty == withEmpty {
		t.Fatalf("include-empty property cursors share an identity: %q", withEmpty)
	}
}

func TestIssueTableCompoundCellKeyResolvesPrimaryAndStatus(t *testing.T) {
	primary := resolvedIssueTableGroup{kind: "parent"}
	compound := resolvedIssueTableGroup{kind: "compound", primary: &primary}
	key := compoundCellGroupKey(
		"parent:00000000-0000-4000-8000-000000000001",
		"todo",
		false,
	)
	args := make([]any, 0, 2)
	predicate, ok := compound.predicate(
		httptest.NewRecorder(),
		key,
		func(value any) string {
			args = append(args, value)
			return fmt.Sprintf("$%d", len(args))
		},
	)
	if !ok {
		t.Fatal("valid compound cell key was rejected")
	}
	if !strings.Contains(predicate, "i.parent_issue_id = $1::uuid") ||
		!strings.Contains(predicate, "i.status = $2::text") ||
		len(args) != 2 || args[1] != "todo" {
		t.Fatalf("compound cell predicate lost a dimension: %s args=%#v", predicate, args)
	}
}

func TestIssueTableRowsCommitsBeforeBestEffortEnrichment(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, $2)
		RETURNING id
	`, testWorkspaceID, fmt.Sprintf("Table enrichment %d", suffix)).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	var issueNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 1
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&issueNumber); err != nil {
		t.Fatalf("reserve issue number: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			position, number, project_id
		)
		VALUES ($1, 'table-enrichment', 'todo', 'none', 'member', $2, 1, $3, $4)
	`, testWorkspaceID, testUserID, issueNumber, projectID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	labelCalls := 0
	tableQueryCalls := 0
	rowQuerySQL := ""
	handler := *testHandler
	handler.TxStarter = issueTableEnrichmentFailTxStarter{
		inner:           testHandler.TxStarter,
		labelCalls:      &labelCalls,
		tableQueryCalls: &tableQueryCalls,
		rowQuerySQL:     &rowQuerySQL,
	}
	recorder := testutil.Call(t, handler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
		Query: issueTableQuerySpec{
			Scope: issueTableScope{Kind: "project", ProjectID: projectID},
			Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
		},
		Group:     issueTableGroupSpec{Kind: "none"},
		Hierarchy: issueTableHierarchyRequest{Enabled: false},
		Page:      issueTablePageRequest{Limit: 10},
	})).Want(http.StatusOK)
	if labelCalls != 0 {
		t.Fatalf("best-effort labels ran inside snapshot transaction %d times", labelCalls)
	}
	var response issueTableRowsResponse
	recorder.Decode(&response)
	if len(response.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(response.Rows))
	}
	if response.Total != 1 || response.BranchTotal != 1 {
		t.Fatalf("unexpected root counts: total=%d branch_total=%d", response.Total, response.BranchTotal)
	}
	if tableQueryCalls != 2 {
		t.Fatalf("ungrouped root head executed %d table queries, want 2", tableQueryCalls)
	}
	if !strings.Contains(rowQuerySQL, "WITH page AS MATERIALIZED") ||
		strings.Contains(rowQuerySQL, "membership AS") ||
		strings.Contains(rowQuerySQL, "FROM membership child") {
		t.Fatalf("flat rows query must page directly without hierarchy work:\n%s", rowQuerySQL)
	}
}

func TestIssueTableStatusGroupingOverOneThousandRows(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, $2)
		RETURNING id
	`, testWorkspaceID, fmt.Sprintf("Server table grouping %d", suffix)).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	var finalNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 1001
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&finalNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}
	firstNumber := finalNumber - 1000
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			position, number, project_id
		)
		SELECT $1, 'server-table-' || n::text,
		       CASE WHEN n <= 501 THEN 'todo' ELSE 'done' END,
		       'none', 'member', $2, n::double precision,
		       $3 + n - 1, $4
		FROM generate_series(1, 1001) AS n
	`, testWorkspaceID, testUserID, firstNumber, projectID); err != nil {
		t.Fatalf("seed issues: %v", err)
	}

	query := issueTableQuerySpec{
		Scope:   issueTableScope{Kind: "project", ProjectID: projectID},
		Filters: issueTableFiltersRequest{},
		Sort:    issueTableSortRequest{Field: "title", Direction: "asc"},
	}
	w := testutil.Call(t, testHandler.ListIssueTableGroups, newRequest("POST", "/api/issues/table/groups", issueTableGroupsRequest{
		Query: query,
		Group: issueTableGroupSpec{Kind: "status"},
		Page:  issueTablePageRequest{Limit: 100},
	})).Want(http.StatusOK)
	var groups issueTableGroupsResponse
	w.Decode(&groups)
	if groups.Total != 1001 {
		t.Fatalf("total = %d, want 1001", groups.Total)
	}
	counts := map[string]int64{}
	for _, group := range groups.Groups {
		counts[group.Key] = group.Count
	}
	if counts["status:todo"] != 501 || counts["status:done"] != 500 {
		t.Fatalf("unexpected group counts: %#v", counts)
	}
	firstGroupPageRecorder := testutil.Call(t, testHandler.ListIssueTableGroups, newRequest("POST", "/api/issues/table/groups", issueTableGroupsRequest{
		Query: query,
		Group: issueTableGroupSpec{Kind: "status"},
		Page:  issueTablePageRequest{Limit: 1},
	}))
	var firstGroupPage issueTableGroupsResponse
	testutil.Equal(t, firstGroupPageRecorder.Code, http.StatusOK, "HTTP status")
	firstGroupPageRecorder.Decode(&firstGroupPage)
	if len(firstGroupPage.Groups) != 1 || firstGroupPage.NextCursor == nil {
		t.Fatalf("unexpected first group page: %#v", firstGroupPage)
	}
	secondGroupPageRecorder := testutil.Call(t, testHandler.ListIssueTableGroups, newRequest("POST", "/api/issues/table/groups", issueTableGroupsRequest{
		Query: query,
		Group: issueTableGroupSpec{Kind: "status"},
		Page:  issueTablePageRequest{Limit: 1, Cursor: firstGroupPage.NextCursor},
	}))
	var secondGroupPage issueTableGroupsResponse
	testutil.Equal(t, secondGroupPageRecorder.Code, http.StatusOK, "HTTP status")
	secondGroupPageRecorder.Decode(&secondGroupPage)
	if len(secondGroupPage.Groups) != 1 || secondGroupPage.Groups[0].Key == firstGroupPage.Groups[0].Key || secondGroupPage.Total != 1001 {
		t.Fatalf("group keyset pagination mismatch: first=%#v second=%#v", firstGroupPage, secondGroupPage)
	}

	groupKey := "status:todo"
	labelCalls := 0
	tableQueryCalls := 0
	rowsHandler := *testHandler
	rowsHandler.TxStarter = issueTableEnrichmentFailTxStarter{
		inner:           testHandler.TxStarter,
		labelCalls:      &labelCalls,
		tableQueryCalls: &tableQueryCalls,
	}
	rowsRecorder := testutil.Call(t, rowsHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
		Query:     query,
		Group:     issueTableGroupSpec{Kind: "status"},
		GroupKey:  &groupKey,
		Hierarchy: issueTableHierarchyRequest{Enabled: false},
		Page:      issueTablePageRequest{Limit: 50},
	})).Want(http.StatusOK)
	var rows issueTableRowsResponse
	rowsRecorder.Decode(&rows)
	if rows.Total != 0 || rows.BranchTotal != 50 || len(rows.Rows) != 50 || rows.NextCursor == nil {
		t.Fatalf("unexpected rows page: total=%d branch_total=%d rows=%d cursor=%v", rows.Total, rows.BranchTotal, len(rows.Rows), rows.NextCursor)
	}
	if tableQueryCalls != 1 {
		t.Fatalf("grouped root head executed %d table queries, want 1", tableQueryCalls)
	}
	firstPageIDs := make(map[string]struct{}, len(rows.Rows))
	for _, row := range rows.Rows {
		firstPageIDs[row.Issue.ID] = struct{}{}
	}
	secondRowsRecorder := testutil.Call(t, rowsHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
		Query:     query,
		Group:     issueTableGroupSpec{Kind: "status"},
		GroupKey:  &groupKey,
		Hierarchy: issueTableHierarchyRequest{Enabled: false},
		Page:      issueTablePageRequest{Limit: 50, Cursor: rows.NextCursor},
	})).Want(http.StatusOK)
	var secondRows issueTableRowsResponse
	secondRowsRecorder.Decode(&secondRows)
	if secondRows.Total != 0 || secondRows.BranchTotal != 50 || len(secondRows.Rows) != 50 {
		t.Fatalf("unexpected grouped continuation: total=%d branch_total=%d rows=%d", secondRows.Total, secondRows.BranchTotal, len(secondRows.Rows))
	}
	if tableQueryCalls != 2 {
		t.Fatalf("grouped continuation executed %d cumulative table queries, want 2", tableQueryCalls)
	}
	for _, row := range secondRows.Rows {
		if _, duplicate := firstPageIDs[row.Issue.ID]; duplicate {
			t.Fatalf("keyset cursor repeated issue %s across pages", row.Issue.ID)
		}
	}

	ungroupedRecorder := testutil.Call(t, rowsHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
		Query:     query,
		Group:     issueTableGroupSpec{Kind: "none"},
		Hierarchy: issueTableHierarchyRequest{Enabled: false},
		Page:      issueTablePageRequest{Limit: 50},
	})).Want(http.StatusOK)
	var ungroupedRows issueTableRowsResponse
	ungroupedRecorder.Decode(&ungroupedRows)
	if ungroupedRows.Total != 1001 || ungroupedRows.BranchTotal != 50 || len(ungroupedRows.Rows) != 50 || ungroupedRows.NextCursor == nil {
		t.Fatalf("unexpected ungrouped root head: total=%d branch_total=%d rows=%d cursor=%v", ungroupedRows.Total, ungroupedRows.BranchTotal, len(ungroupedRows.Rows), ungroupedRows.NextCursor)
	}
	if tableQueryCalls != 4 {
		t.Fatalf("ungrouped root head executed %d cumulative table queries, want 4", tableQueryCalls)
	}

	ungroupedNextRecorder := testutil.Call(t, rowsHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
		Query:     query,
		Group:     issueTableGroupSpec{Kind: "none"},
		Hierarchy: issueTableHierarchyRequest{Enabled: false},
		Page:      issueTablePageRequest{Limit: 50, Cursor: ungroupedRows.NextCursor},
	})).Want(http.StatusOK)
	var ungroupedNext issueTableRowsResponse
	ungroupedNextRecorder.Decode(&ungroupedNext)
	if ungroupedNext.Total != 0 || ungroupedNext.BranchTotal != 50 || len(ungroupedNext.Rows) != 50 {
		t.Fatalf("unexpected ungrouped continuation: total=%d branch_total=%d rows=%d", ungroupedNext.Total, ungroupedNext.BranchTotal, len(ungroupedNext.Rows))
	}
	if tableQueryCalls != 5 {
		t.Fatalf("ungrouped continuation executed %d cumulative table queries, want 5", tableQueryCalls)
	}

	for _, sortCase := range []issueTableSortRequest{
		{Field: "status", Direction: "desc"},
		{Field: "created_at", Direction: "desc"},
		{Field: "due_date", Direction: "asc"},
	} {
		sortQuery := query
		sortQuery.Sort = sortCase
		fetchPage := func(cursor *string) issueTableRowsResponse {
			t.Helper()
			recorder := testutil.Call(t, testHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
				Query:     sortQuery,
				Group:     issueTableGroupSpec{Kind: "status"},
				GroupKey:  &groupKey,
				Hierarchy: issueTableHierarchyRequest{Enabled: false},
				Page:      issueTablePageRequest{Limit: 10, Cursor: cursor},
			}))
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s cursor page status = %d: %s", sortCase.Field, recorder.Code, recorder.Body.String())
			}
			var response issueTableRowsResponse
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode %s cursor page: %v", sortCase.Field, err)
			}
			return response
		}
		first := fetchPage(nil)
		if len(first.Rows) != 10 || first.NextCursor == nil {
			t.Fatalf("%s first cursor page is incomplete", sortCase.Field)
		}
		seen := make(map[string]struct{}, len(first.Rows))
		for _, row := range first.Rows {
			seen[row.Issue.ID] = struct{}{}
		}
		second := fetchPage(first.NextCursor)
		if len(second.Rows) != 10 {
			t.Fatalf("%s second cursor page length = %d", sortCase.Field, len(second.Rows))
		}
		for _, row := range second.Rows {
			if _, duplicate := seen[row.Issue.ID]; duplicate {
				t.Fatalf("%s keyset cursor repeated issue %s", sortCase.Field, row.Issue.ID)
			}
		}
	}

	filteredQuery := query
	filteredQuery.Filters.Statuses = []string{"todo"}
	facetsRecorder := testutil.Call(t, testHandler.ListIssueTableFacets, newRequest("POST", "/api/issues/table/facets", issueTableFacetsRequest{
		Query:  filteredQuery,
		Facets: []issueTableFacetSpec{{Kind: "status"}},
	})).Want(http.StatusOK)
	var facets issueTableFacetsResponse
	facetsRecorder.Decode(&facets)
	if facets.Total != 501 || len(facets.Facets) != 1 {
		t.Fatalf("unexpected filtered facet response: total=%d facets=%d", facets.Total, len(facets.Facets))
	}
	facetCounts := map[string]int64{}
	for _, value := range facets.Facets[0].Values {
		facetCounts[value.Key] = value.Count
	}
	if facetCounts["todo"] != 501 || facetCounts["done"] != 500 {
		t.Fatalf("status facet must ignore its own active filter: %#v", facetCounts)
	}

	includeTotal := false
	facetCountQueries := 0
	facetsHandler := *testHandler
	facetsHandler.TxStarter = issueTableEnrichmentFailTxStarter{
		inner:           testHandler.TxStarter,
		tableQueryCalls: &facetCountQueries,
	}
	noTotalRecorder := testutil.Call(t, facetsHandler.ListIssueTableFacets, newRequest("POST", "/api/issues/table/facets", issueTableFacetsRequest{
		Query:        filteredQuery,
		Facets:       []issueTableFacetSpec{{Kind: "status"}},
		IncludeTotal: &includeTotal,
	})).Want(http.StatusOK)
	var noTotal issueTableFacetsResponse
	noTotalRecorder.Decode(&noTotal)
	if noTotal.Total != 0 || len(noTotal.Facets) != 1 {
		t.Fatalf("unexpected facets without total: %#v", noTotal)
	}
	if facetCountQueries != 0 {
		t.Fatalf("include_total=false executed %d total count queries, want 0", facetCountQueries)
	}

	batchFacetQueries := 0
	batchTotalQueries := 0
	batchHandler := *testHandler
	batchHandler.TxStarter = issueTableEnrichmentFailTxStarter{
		inner:           testHandler.TxStarter,
		facetQueryCalls: &batchFacetQueries,
		tableQueryCalls: &batchTotalQueries,
	}
	batchRecorder := testutil.Call(t, batchHandler.ListIssueTableFacets, newRequest("POST", "/api/issues/table/facets", issueTableFacetsRequest{
		Query: query,
		Facets: []issueTableFacetSpec{
			{Kind: "status"},
			{Kind: "priority"},
			{Kind: "assignee"},
			{Kind: "creator"},
			{Kind: "project"},
		},
	})).Want(http.StatusOK)
	var batchResponse issueTableFacetsResponse
	batchRecorder.Decode(&batchResponse)
	if batchResponse.Total != 1001 || len(batchResponse.Facets) != 5 {
		t.Fatalf("unexpected batch facets response: total=%d facets=%d", batchResponse.Total, len(batchResponse.Facets))
	}
	if batchFacetQueries != 1 || batchTotalQueries != 0 {
		t.Fatalf("batch facets executed grouping queries=%d total queries=%d, want 1 and 0", batchFacetQueries, batchTotalQueries)
	}
	batchCounts := make(map[string]map[string]int64, len(batchResponse.Facets))
	for _, facet := range batchResponse.Facets {
		counts := make(map[string]int64, len(facet.Values))
		for _, value := range facet.Values {
			counts[value.Key] = value.Count
		}
		batchCounts[facet.Kind] = counts
	}
	if batchCounts["status"]["todo"] != 501 || batchCounts["status"]["done"] != 500 ||
		batchCounts["priority"]["none"] != 1001 ||
		batchCounts["assignee"]["__none__"] != 1001 ||
		batchCounts["creator"]["member:"+testUserID] != 1001 ||
		batchCounts["project"][projectID] != 1001 {
		t.Fatalf("unexpected batched facet counts: %#v", batchCounts)
	}

	mixedRecorder := testutil.Call(t, testHandler.ListIssueTableFacets, newRequest("POST", "/api/issues/table/facets", issueTableFacetsRequest{
		Query:  filteredQuery,
		Facets: []issueTableFacetSpec{{Kind: "status"}, {Kind: "priority"}},
	})).Want(http.StatusOK)
	var mixedResponse issueTableFacetsResponse
	mixedRecorder.Decode(&mixedResponse)
	if mixedResponse.Total != 501 || len(mixedResponse.Facets) != 2 ||
		mixedResponse.Facets[0].Kind != "status" || mixedResponse.Facets[1].Kind != "priority" {
		t.Fatalf("unexpected mixed facets response: %#v", mixedResponse)
	}
	mixedCounts := make(map[string]map[string]int64, len(mixedResponse.Facets))
	for _, facet := range mixedResponse.Facets {
		counts := make(map[string]int64, len(facet.Values))
		for _, value := range facet.Values {
			counts[value.Key] = value.Count
		}
		mixedCounts[facet.Kind] = counts
	}
	if mixedCounts["status"]["todo"] != 501 || mixedCounts["status"]["done"] != 500 ||
		mixedCounts["priority"]["none"] != 501 {
		t.Fatalf("mixed facets lost disjunctive semantics: %#v", mixedCounts)
	}
}

func TestIssueTableAssigneeNamesResolveAfterGrouping(t *testing.T) {
	ctx := context.Background()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, 'Server table assignee grouping')
		RETURNING id
	`, testWorkspaceID).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	var finalNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 2
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&finalNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, assignee_type, assignee_id,
			creator_type, creator_id, position, number, project_id
		)
		VALUES
			($1, 'Assigned row', 'todo', 'none', 'member', $2, 'member', $2, 1, $3, $4),
			($1, 'Unassigned row', 'todo', 'none', NULL, NULL, 'member', $2, 2, $3 + 1, $4)
	`, testWorkspaceID, testUserID, finalNumber-1, projectID); err != nil {
		t.Fatalf("seed issues: %v", err)
	}

	groupQuerySQL := ""
	handler := *testHandler
	handler.TxStarter = issueTableEnrichmentFailTxStarter{
		inner:         testHandler.TxStarter,
		groupQuerySQL: &groupQuerySQL,
	}
	recorder := testutil.Call(t, handler.ListIssueTableGroups, newRequest("POST", "/api/issues/table/groups", issueTableGroupsRequest{
		Query: issueTableQuerySpec{
			Scope: issueTableScope{Kind: "project", ProjectID: projectID},
			Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
		},
		Group: issueTableGroupSpec{Kind: "assignee"},
		Page:  issueTablePageRequest{Limit: 10},
	})).Want(http.StatusOK)
	var response issueTableGroupsResponse
	recorder.Decode(&response)
	counts := make(map[string]int64, len(response.Groups))
	for _, group := range response.Groups {
		counts[group.Key] = group.Count
	}
	if counts["assignee:member:"+testUserID] != 1 || counts["assignee:unassigned"] != 1 {
		t.Fatalf("unexpected assignee groups: %#v", counts)
	}
	if response.Groups[0].Key != "assignee:member:"+testUserID ||
		response.Groups[len(response.Groups)-1].Key != "assignee:unassigned" {
		t.Fatalf("assignee groups are not actor-priority ordered: %#v", response.Groups)
	}
	sortedAt := strings.Index(groupQuerySQL, "), sorted AS (")
	nameLookupAt := strings.Index(groupQuerySQL, `SELECT u.name FROM "user" u`)
	if sortedAt < 0 || nameLookupAt < sortedAt {
		t.Fatalf("assignee names must resolve after actor aggregation:\n%s", groupQuerySQL)
	}
}

func TestIssueTableCompoundParentGroupsReturnExactStatusCells(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, $2)
		RETURNING id
	`, testWorkspaceID, fmt.Sprintf("Compound parent grouping %d", suffix)).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

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
	var parentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			position, number, project_id
		)
		VALUES ($1, 'Parent context', 'done', 'none', 'member', $2, 1, $3, $4)
		RETURNING id
	`, testWorkspaceID, testUserID, finalNumber-3, projectID).Scan(&parentID); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			parent_issue_id, position, number, project_id
		)
		VALUES
			($1, 'Child todo', 'todo', 'none', 'member', $2, $3, 2, $4, $5),
			($1, 'Child review', 'in_review', 'none', 'member', $2, $3, 3, $4 + 1, $5),
			($1, 'No parent', 'todo', 'none', 'member', $2, NULL, 4, $4 + 2, $5)
	`, testWorkspaceID, testUserID, parentID, finalNumber-2, projectID); err != nil {
		t.Fatalf("seed grouped issues: %v", err)
	}

	recorder := testutil.Call(t, testHandler.ListIssueTableGroups, newRequest("POST", "/api/issues/table/groups", issueTableGroupsRequest{
		Query: issueTableQuerySpec{
			Scope: issueTableScope{Kind: "project", ProjectID: projectID},
			Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
		},
		Group: issueTableGroupSpec{Kind: "compound", Primary: "parent", Secondary: "status"},
		Page:  issueTablePageRequest{Limit: 10},
	})).Want(http.StatusOK)
	var response issueTableGroupsResponse
	recorder.Decode(&response)
	if response.Total != 4 || len(response.Groups) != 2 {
		t.Fatalf("unexpected compound group totals: %#v", response)
	}
	byKey := make(map[string]issueTableGroupDescriptorResponse, len(response.Groups))
	for _, group := range response.Groups {
		byKey[group.Key] = group
	}
	parent := byKey["parent:"+parentID]
	if parent.Count != 2 || parent.Value.Parent == nil ||
		parent.Value.Parent.Title != "Parent context" ||
		parent.Value.ValueState != "value" {
		t.Fatalf("parent descriptor lost context: %#v", parent)
	}
	parentCells := make(map[string]int64, len(parent.SecondaryGroups))
	for _, cell := range parent.SecondaryGroups {
		parentCells[cell.Value.Status] = cell.Count
		if !strings.HasPrefix(cell.Key, "compound:") {
			t.Fatalf("cell key is not opaque compound key: %q", cell.Key)
		}
	}
	if parentCells["todo"] != 1 || parentCells["in_review"] != 1 {
		t.Fatalf("unexpected parent status cells: %#v", parentCells)
	}
	noParent := byKey["parent:none"]
	if noParent.Count != 2 || noParent.Value.ValueState != "unset" {
		t.Fatalf("unexpected no-parent lane: %#v", noParent)
	}

	// A second parent has only a hidden-status child. Under a visible-status
	// compound query it must stay a No-parent card instead of creating an
	// empty lane. The first parent does have a visible child, so it is promoted
	// to a lane header and must disappear from the No-parent rows and counts.
	var hiddenParentNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = issue_counter + 2
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&hiddenParentNumber); err != nil {
		t.Fatalf("reserve hidden-parent issue numbers: %v", err)
	}
	var hiddenParentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			position, number, project_id
		)
		VALUES ($1, 'Visible parent card', 'todo', 'none', 'member', $2, 5, $3, $4)
		RETURNING id
	`, testWorkspaceID, testUserID, hiddenParentNumber-1, projectID).Scan(&hiddenParentID); err != nil {
		t.Fatalf("create hidden-only parent: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			parent_issue_id, position, number, project_id
		)
		VALUES ($1, 'Hidden child', 'done', 'none', 'member', $2, $3, 6, $4, $5)
	`, testWorkspaceID, testUserID, hiddenParentID, hiddenParentNumber, projectID); err != nil {
		t.Fatalf("create hidden child: %v", err)
	}

	filteredGroup := issueTableGroupSpec{
		Kind:            "compound",
		Primary:         "parent",
		Secondary:       "status",
		SecondaryValues: []string{"todo"},
	}
	filteredRecorder := testutil.Call(t, testHandler.ListIssueTableGroups, newRequest("POST", "/api/issues/table/groups", issueTableGroupsRequest{
		Query: issueTableQuerySpec{
			Scope: issueTableScope{Kind: "project", ProjectID: projectID},
			Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
		},
		Group: filteredGroup,
		Page:  issueTablePageRequest{Limit: 10},
	})).Want(http.StatusOK)
	var filtered issueTableGroupsResponse
	filteredRecorder.Decode(&filtered)
	if filtered.Total != 3 || len(filtered.Groups) != 2 {
		t.Fatalf("unexpected visible compound groups: %#v", filtered)
	}
	filteredByKey := make(map[string]issueTableGroupDescriptorResponse, len(filtered.Groups))
	for _, descriptor := range filtered.Groups {
		filteredByKey[descriptor.Key] = descriptor
	}
	if _, exists := filteredByKey["parent:"+hiddenParentID]; exists {
		t.Fatalf("hidden-only parent lane was returned: %#v", filtered.Groups)
	}
	filteredParent := filteredByKey["parent:"+parentID]
	filteredNoParent := filteredByKey["parent:none"]
	if filteredParent.Count != 2 || filteredNoParent.Count != 2 {
		t.Fatalf("parent header was not removed from aligned counts: parent=%#v no-parent=%#v", filteredParent, filteredNoParent)
	}

	cellKey := func(descriptor issueTableGroupDescriptorResponse, status string) string {
		t.Helper()
		for _, cell := range descriptor.SecondaryGroups {
			if cell.Value.Status == status {
				return cell.Key
			}
		}
		t.Fatalf("missing %s cell in %#v", status, descriptor)
		return ""
	}
	listCell := func(key string) issueTableRowsResponse {
		t.Helper()
		rowsRecorder := testutil.Call(t, testHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
			Query: issueTableQuerySpec{
				Scope: issueTableScope{Kind: "project", ProjectID: projectID},
				Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
			},
			Group:     filteredGroup,
			GroupKey:  &key,
			Hierarchy: issueTableHierarchyRequest{Enabled: false},
			Page:      issueTablePageRequest{Limit: 10},
		})).Want(http.StatusOK)
		var result issueTableRowsResponse
		rowsRecorder.Decode(&result)
		return result
	}
	noParentTodo := listCell(cellKey(filteredNoParent, "todo"))
	if len(noParentTodo.Rows) != 2 {
		t.Fatalf("unexpected visible no-parent rows: %#v", noParentTodo.Rows)
	}
	noParentIDs := map[string]bool{}
	for _, row := range noParentTodo.Rows {
		noParentIDs[row.Issue.ID] = true
	}
	if noParentIDs[parentID] || !noParentIDs[hiddenParentID] {
		t.Fatalf("parent header/card semantics diverged: %#v", noParentIDs)
	}
	noParentDone := listCell(cellKey(filteredNoParent, "done"))
	if len(noParentDone.Rows) != 0 {
		t.Fatalf("promoted parent remained in No-parent rows: %#v", noParentDone.Rows)
	}
}

func TestIssueTableHierarchyDoesNotCrossGroups(t *testing.T) {
	ctx := context.Background()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, 'Server table cross-group hierarchy')
		RETURNING id
	`, testWorkspaceID).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	var finalNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 2
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&finalNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}
	var parentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			position, number, project_id
		)
		VALUES ($1, 'Todo parent', 'todo', 'none', 'member', $2, 1, $3, $4)
		RETURNING id
	`, testWorkspaceID, testUserID, finalNumber-1, projectID).Scan(&parentID); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	var childID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			parent_issue_id, position, number, project_id
		)
		VALUES ($1, 'Done child', 'done', 'none', 'member', $2, $3, 2, $4, $5)
		RETURNING id
	`, testWorkspaceID, testUserID, parentID, finalNumber, projectID).Scan(&childID); err != nil {
		t.Fatalf("create child: %v", err)
	}

	query := issueTableQuerySpec{
		Scope:   issueTableScope{Kind: "project", ProjectID: projectID},
		Filters: issueTableFiltersRequest{},
		Sort:    issueTableSortRequest{Field: "position", Direction: "asc"},
	}
	rowQuerySQL := ""
	rowsHandler := *testHandler
	rowsHandler.TxStarter = issueTableEnrichmentFailTxStarter{
		inner:       testHandler.TxStarter,
		rowQuerySQL: &rowQuerySQL,
	}
	listGroup := func(groupKey string) issueTableRowsResponse {
		t.Helper()
		w := testutil.Call(t, rowsHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
			Query:     query,
			Group:     issueTableGroupSpec{Kind: "status"},
			GroupKey:  &groupKey,
			Hierarchy: issueTableHierarchyRequest{Enabled: true},
			Page:      issueTablePageRequest{Limit: 50},
		}))
		if w.Code != http.StatusOK {
			t.Fatalf("list %s: status=%d body=%s", groupKey, w.Code, w.Body.String())
		}
		var response issueTableRowsResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("decode %s: %v", groupKey, err)
		}
		return response
	}

	doneRows := listGroup("status:done")
	if len(doneRows.Rows) != 1 || doneRows.Rows[0].Issue.ID != childID {
		t.Fatalf("done child must become a root in its own group: %#v", doneRows.Rows)
	}
	todoRows := listGroup("status:todo")
	if len(todoRows.Rows) != 1 || todoRows.Rows[0].Issue.ID != parentID {
		t.Fatalf("todo group root mismatch: %#v", todoRows.Rows)
	}
	if todoRows.Rows[0].DirectChildCount != 0 {
		t.Fatalf("cross-group child leaked into todo parent count: %d", todoRows.Rows[0].DirectChildCount)
	}
	if !strings.Contains(rowQuerySQL, "membership AS NOT MATERIALIZED") ||
		!strings.Contains(rowQuerySQL, "page AS MATERIALIZED") ||
		!strings.Contains(rowQuerySQL, "(SELECT parent.id FROM membership parent") ||
		strings.Contains(rowQuerySQL, "NOT EXISTS (SELECT 1 FROM membership parent") ||
		strings.Contains(rowQuerySQL, "child_counts AS") {
		t.Fatalf("hierarchy rows query must preserve ordered paging and page-local counts:\n%s", rowQuerySQL)
	}
}

func TestIssueTableHierarchyRootKeysetPagination(t *testing.T) {
	ctx := context.Background()
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, 'Server table hierarchy pagination')
		RETURNING id
	`, testWorkspaceID).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	var finalNumber int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(
			issue_counter,
			(SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)
		) + 3
		WHERE id = $1
		RETURNING issue_counter
	`, testWorkspaceID).Scan(&finalNumber); err != nil {
		t.Fatalf("reserve issue numbers: %v", err)
	}
	rows, err := testPool.Query(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			position, number, project_id
		)
		SELECT $1, 'Hierarchy root ' || n::text, 'todo', 'none', 'member', $2,
		       n::double precision, $3 + n - 1, $4
		FROM generate_series(1, 3) AS n
		ORDER BY n
		RETURNING id
	`, testWorkspaceID, testUserID, finalNumber-2, projectID)
	if err != nil {
		t.Fatalf("seed hierarchy roots: %v", err)
	}
	expectedIDs := make(map[string]struct{}, 3)
	for rows.Next() {
		var issueID string
		if err := rows.Scan(&issueID); err != nil {
			rows.Close()
			t.Fatalf("scan hierarchy root: %v", err)
		}
		expectedIDs[issueID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("seed hierarchy roots: %v", err)
	}
	rows.Close()

	query := issueTableQuerySpec{
		Scope: issueTableScope{Kind: "project", ProjectID: projectID},
		Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
	}
	fetchPage := func(cursor *string) issueTableRowsResponse {
		t.Helper()
		w := testutil.Call(t, testHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
			Query:     query,
			Group:     issueTableGroupSpec{Kind: "none"},
			Hierarchy: issueTableHierarchyRequest{Enabled: true},
			Page:      issueTablePageRequest{Limit: 2, Cursor: cursor},
		})).Want(http.StatusOK)
		var response issueTableRowsResponse
		w.Decode(&response)
		return response
	}

	first := fetchPage(nil)
	if first.Total != 3 || len(first.Rows) != 2 || first.NextCursor == nil {
		t.Fatalf("unexpected first hierarchy page: total=%d rows=%d cursor=%v", first.Total, len(first.Rows), first.NextCursor)
	}
	second := fetchPage(first.NextCursor)
	if second.Total != 0 || len(second.Rows) != 1 || second.NextCursor != nil {
		t.Fatalf("unexpected second hierarchy page: total=%d rows=%d cursor=%v", second.Total, len(second.Rows), second.NextCursor)
	}

	seenIDs := make(map[string]struct{}, 3)
	for _, page := range []issueTableRowsResponse{first, second} {
		for _, row := range page.Rows {
			if _, duplicate := seenIDs[row.Issue.ID]; duplicate {
				t.Fatalf("hierarchy root %s repeated across pages", row.Issue.ID)
			}
			seenIDs[row.Issue.ID] = struct{}{}
		}
	}
	if len(seenIDs) != len(expectedIDs) {
		t.Fatalf("reachable hierarchy roots = %d, want %d", len(seenIDs), len(expectedIDs))
	}
	for issueID := range expectedIDs {
		if _, ok := seenIDs[issueID]; !ok {
			t.Fatalf("hierarchy root %s was not reachable through pagination", issueID)
		}
	}
}
