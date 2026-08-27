package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func TestListIssuesSortsByStatusAndUpdatedAt(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var projectID string
	projectID = dbfx.Insert(t, "project", testutil.Cols{"workspace_id": testWorkspaceID, "title": fmt.Sprintf("Issue table sort %d", suffix)})

	type fixture struct {
		title      string
		status     string
		updatedAt  time.Time
		activityAt time.Time
	}
	fixtures := []fixture{
		{"sort-done", "done", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{"sort-backlog", "backlog", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)},
		{"sort-progress", "in_progress", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)},
	}
	for index, item := range fixtures {
		var number int
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
			WHERE id = $1 RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}
		if _, err := testPool.Exec(ctx, `
			INSERT INTO issue (
				workspace_id, title, status, priority, creator_type, creator_id,
				position, number, project_id, created_at, updated_at, last_activity_at
			)
			VALUES ($1, $2, $3, 'none', 'member', $4, $5, $6, $7, $8, $8, $9)
		`, testWorkspaceID, item.title, item.status, testUserID, index, number, projectID, item.updatedAt, item.activityAt); err != nil {
			t.Fatalf("create issue %q: %v", item.title, err)
		}
	}

	listTitles := func(sort, direction string) []string {
		t.Helper()
		path := fmt.Sprintf(
			"/api/issues?workspace_id=%s&project_id=%s&limit=50&sort=%s",
			testWorkspaceID,
			projectID,
			sort,
		)
		if direction != "" {
			path += "&direction=" + direction
		}
		w := testutil.Call(t, testHandler.ListIssues, newRequest("GET", path, nil)).Want(http.StatusOK)
		var response struct {
			Issues []IssueResponse `json:"issues"`
		}
		w.Decode(&response)
		titles := make([]string, 0, len(response.Issues))
		for _, issue := range response.Issues {
			titles = append(titles, issue.Title)
		}
		return titles
	}
	groupedTitles := func(sort, direction string) []string {
		t.Helper()
		path := fmt.Sprintf(
			"/api/issues/grouped?workspace_id=%s&project_id=%s&limit=50&sort=%s",
			testWorkspaceID,
			projectID,
			sort,
		)
		if direction != "" {
			path += "&direction=" + direction
		}
		w := testutil.Call(t, testHandler.ListGroupedIssues, newRequest("GET", path, nil)).Want(http.StatusOK)
		var response GroupedIssuesResponse
		w.Decode(&response)
		if len(response.Groups) != 1 {
			t.Fatalf("ListGroupedIssues groups = %d, want 1", len(response.Groups))
		}
		titles := make([]string, 0, len(response.Groups[0].Issues))
		for _, issue := range response.Groups[0].Issues {
			titles = append(titles, issue.Title)
		}
		return titles
	}

	assertTitles := func(got, want []string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	assertTitles(listTitles("status", "asc"), []string{
		"sort-backlog",
		"sort-progress",
		"sort-done",
	})
	assertTitles(listTitles("status", "desc"), []string{
		"sort-done",
		"sort-progress",
		"sort-backlog",
	})
	assertTitles(listTitles("updated_at", "asc"), []string{
		"sort-backlog",
		"sort-progress",
		"sort-done",
	})
	assertTitles(listTitles("updated_at", "desc"), []string{
		"sort-done",
		"sort-progress",
		"sort-backlog",
	})
	assertTitles(listTitles("last_activity", "asc"), []string{
		"sort-done",
		"sort-progress",
		"sort-backlog",
	})
	assertTitles(listTitles("last_activity", "desc"), []string{
		"sort-backlog",
		"sort-progress",
		"sort-done",
	})
	assertTitles(listTitles("last_activity", ""), []string{
		"sort-backlog",
		"sort-progress",
		"sort-done",
	})
	assertTitles(groupedTitles("last_activity", "asc"), []string{
		"sort-done",
		"sort-progress",
		"sort-backlog",
	})
	assertTitles(groupedTitles("last_activity", ""), []string{
		"sort-backlog",
		"sort-progress",
		"sort-done",
	})
}
