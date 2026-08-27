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

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// TestUpdateIssueProjectStaysInWorkspace mirrors
// TestCreateIssueRejectsCrossWorkspaceProject on the update path. UpdateIssue
// is the canonical issue write — the legacy PUT endpoint and MoveIssue both
// land on it — so the project boundary belongs here and not only on the entry
// points that happen to pre-check it. Accepting a foreign project would leave
// the issue pointing at a project from another workspace: it disappears from
// that workspace's board, which lists by workspace, and its own board cannot
// resolve the project.
func TestUpdateIssueProjectStaysInWorkspace(t *testing.T) {
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			number, position
		)
		VALUES (
			$1, $2, 'todo', 'none', 'member', $3,
			(SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1),
			100
		)
		RETURNING id
	`, testWorkspaceID, "Project boundary test "+suffix, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	var localProjectID string
	localProjectID = dbfx.Insert(t, "project", testutil.Cols{"workspace_id": testWorkspaceID, "title": "Local update project " + suffix})

	var foreignWorkspaceID string
	foreignWorkspaceID = dbfx.Insert(t, "workspace", testutil.Cols{"name": "Update boundary foreign workspace " + suffix, "slug": "update-boundary-" + suffix, "description": testutil.Raw("''"), "issue_prefix": testutil.Raw("'UPB'")})

	var foreignProjectID string
	foreignProjectID = dbfx.Insert(t, "project", testutil.Cols{"workspace_id": foreignWorkspaceID, "title": "Foreign update project " + suffix})

	t.Run("same-workspace project is accepted", func(t *testing.T) {

		req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{
			"project_id": localProjectID,
		})
		req = withURLParam(req, "id", issueID)
		w := testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
		var updated IssueResponse
		w.Decode(&updated)
		if updated.ProjectID == nil || *updated.ProjectID != localProjectID {
			t.Fatalf("UpdateIssue: expected project %s, got %v", localProjectID, updated.ProjectID)
		}
	})

	t.Run("cross-workspace project is rejected", func(t *testing.T) {

		req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{
			"project_id": foreignProjectID,
		})
		req = withURLParam(req, "id", issueID)
		w := testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusBadRequest)
		if !strings.Contains(w.Body.String(), "project not found in this workspace") {
			t.Fatalf("UpdateIssue: expected boundary error message, got %s", w.Body.String())
		}

		var storedProjectID string
		if err := testPool.QueryRow(context.Background(),
			`SELECT project_id FROM issue WHERE id = $1`, issueID,
		).Scan(&storedProjectID); err != nil {
			t.Fatalf("read stored project: %v", err)
		}
		if storedProjectID != localProjectID {
			t.Fatalf("rejected update still wrote project %s — workspace boundary crossed", storedProjectID)
		}
	})
}

// TestBatchUpdateIssuesProjectStaysInWorkspace covers the same boundary on
// POST /api/issues/batch-update, reachable with a multi-select drag on the
// board: a project from another workspace is rejected and no target row moves.
func TestBatchUpdateIssuesProjectStaysInWorkspace(t *testing.T) {

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	first := insertProjectScopeTestIssue(t, "Batch project boundary A "+suffix)
	second := insertProjectScopeTestIssue(t, "Batch project boundary B "+suffix)

	var localProjectID string
	localProjectID = dbfx.Insert(t, "project", testutil.Cols{"workspace_id": testWorkspaceID, "title": "Local batch project " + suffix})

	var foreignWorkspaceID string
	foreignWorkspaceID = dbfx.Insert(t, "workspace", testutil.Cols{"name": "Batch boundary foreign workspace " + suffix, "slug": "batch-boundary-" + suffix, "description": testutil.Raw("''"), "issue_prefix": testutil.Raw("'BPB'")})

	var foreignProjectID string
	foreignProjectID = dbfx.Insert(t, "project", testutil.Cols{"workspace_id": foreignWorkspaceID, "title": "Foreign batch project " + suffix})

	batchUpdateProject := func(projectID string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := newRequest("POST", "/api/issues/batch-update", map[string]any{
			"issue_ids": []string{first, second},
			"updates":   map[string]any{"project_id": projectID},
		})
		testHandler.BatchUpdateIssues(w, req)
		return w
	}

	assertStoredProjects := func(t *testing.T, want string) {
		t.Helper()
		for _, issueID := range []string{first, second} {
			var stored string
			if err := testPool.QueryRow(context.Background(),
				`SELECT project_id FROM issue WHERE id = $1`, issueID,
			).Scan(&stored); err != nil {
				t.Fatalf("read stored project for %s: %v", issueID, err)
			}
			if stored != want {
				t.Fatalf("issue %s: expected project %s, got %s", issueID, want, stored)
			}
		}
	}

	// A non-null baseline first, so the rejection below has to preserve a real
	// value rather than a NULL a skipped write would also leave.
	t.Run("same-workspace project is accepted", func(t *testing.T) {
		w := batchUpdateProject(localProjectID)
		testutil.Equal(t, w.Code, http.StatusOK, "HTTP status")
		var resp struct {
			Updated int `json:"updated"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Updated != 2 {
			t.Fatalf("BatchUpdateIssues: expected updated 2, got %d", resp.Updated)
		}
		assertStoredProjects(t, localProjectID)
	})

	t.Run("cross-workspace project changes no row", func(t *testing.T) {
		w := batchUpdateProject(foreignProjectID)
		testutil.Equal(t, w.Code, http.StatusBadRequest, "HTTP status")
		if !strings.Contains(w.Body.String(), "project not found in this workspace") {
			t.Fatalf("BatchUpdateIssues: expected boundary error message, got %s", w.Body.String())
		}
		assertStoredProjects(t, localProjectID)
	})

	// The batch shares one project_id, so an unparseable value is a request
	// error too, not a per-issue skip.
	t.Run("unparseable project is rejected", func(t *testing.T) {
		w := batchUpdateProject("not-a-uuid")
		testutil.Equal(t, w.Code, http.StatusBadRequest, "HTTP status")
		assertStoredProjects(t, localProjectID)
	})
}

// insertProjectScopeTestIssue writes the row directly, skipping the CreateIssue
// side effects the boundary tests do not need.
func insertProjectScopeTestIssue(t *testing.T, title string) string {
	t.Helper()
	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			number, position
		)
		VALUES (
			$1, $2, 'todo', 'none', 'member', $3,
			(SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1),
			100
		)
		RETURNING id
	`, testWorkspaceID, title, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("insert issue %q: %v", title, err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})
	return issueID
}
