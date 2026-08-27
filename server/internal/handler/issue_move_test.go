package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func TestIssueMovePositionUsesRelativeAnchors(t *testing.T) {
	before := 10.0
	after := 20.0
	position, err := issueMovePosition(3, &before, &after)
	if err != nil {
		t.Fatalf("issueMovePosition: %v", err)
	}
	if position != 15 {
		t.Fatalf("position = %v, want 15", position)
	}
}

func TestIssueMovePositionRejectsStaleAnchors(t *testing.T) {
	before := 20.0
	after := 10.0
	if _, err := issueMovePosition(3, &before, &after); err == nil {
		t.Fatal("out-of-order anchors were accepted")
	}
}

func TestIssueMovePositionRejectsExhaustedGap(t *testing.T) {
	before := 1.0
	after := math.Nextafter(before, math.Inf(1))
	if _, err := issueMovePosition(3, &before, &after); err == nil {
		t.Fatal("gap with no representable midpoint was accepted")
	}
}

func TestIssueMovePositionKeepsPositionWithoutAnchors(t *testing.T) {
	position, err := issueMovePosition(7, nil, nil)
	if err != nil {
		t.Fatalf("issueMovePosition: %v", err)
	}
	if position != 7 {
		t.Fatalf("position = %v, want 7", position)
	}
}

func TestMoveIssueRejectsUnsafeInputs(t *testing.T) {
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	var movedIssueID string
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
	`, testWorkspaceID, "Move boundary test "+suffix, testUserID).Scan(&movedIssueID); err != nil {
		t.Fatalf("insert moved issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, movedIssueID)
	})

	var foreignWorkspaceID string
	foreignWorkspaceID = dbfx.Insert(t, "workspace", testutil.Cols{"name": "Move boundary foreign workspace " + suffix, "slug": "move-boundary-" + suffix, "description": testutil.Raw("''"), "issue_prefix": testutil.Raw("'MOV'")})

	var foreignAnchorID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (
			workspace_id, title, status, priority, creator_type, creator_id,
			number, position
		)
		VALUES ($1, $2, 'todo', 'none', 'member', $3, 1, 200)
		RETURNING id
	`, foreignWorkspaceID, "Foreign move anchor "+suffix, testUserID).Scan(&foreignAnchorID); err != nil {
		t.Fatalf("insert foreign anchor: %v", err)
	}

	var foreignProjectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title)
		VALUES ($1, $2)
		RETURNING id
	`, foreignWorkspaceID, "Foreign move project "+suffix).Scan(&foreignProjectID); err != nil {
		t.Fatalf("insert foreign project: %v", err)
	}

	tests := []struct {
		name      string
		body      map[string]any
		wantError string
	}{
		{
			name: "cross-workspace anchor",
			body: map[string]any{
				"before_id": foreignAnchorID,
				"after_id":  nil,
			},
			wantError: "move anchor not found in this workspace",
		},
		{
			name: "cross-workspace project",
			body: map[string]any{
				"project_id": foreignProjectID,
				"before_id":  nil,
				"after_id":   nil,
			},
			wantError: "project not found in this workspace",
		},
		{
			name: "canonical position bypass",
			body: map[string]any{
				"position":  123,
				"before_id": nil,
				"after_id":  nil,
			},
			wantError: "unsupported move field: position",
		},
		{
			name: "self anchor",
			body: map[string]any{
				"before_id": movedIssueID,
				"after_id":  nil,
			},
			wantError: "before_id cannot be the moved issue",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {

			req := newRequest(
				http.MethodPost,
				"/api/issues/"+movedIssueID+"/move",
				tc.body,
			)
			req = withURLParam(req, "id", movedIssueID)
			w := testutil.Call(t, testHandler.MoveIssue, req).Want(http.StatusBadRequest)
			if !strings.Contains(w.Body.String(), tc.wantError) {
				t.Fatalf("MoveIssue: expected error %q, got %s", tc.wantError, w.Body.String())
			}
		})
	}

	var title string
	var position float64
	var projectID *string
	if err := testPool.QueryRow(ctx, `
		SELECT title, position, project_id::text
		FROM issue
		WHERE id = $1
	`, movedIssueID).Scan(&title, &position, &projectID); err != nil {
		t.Fatalf("reload moved issue: %v", err)
	}
	if title != "Move boundary test "+suffix || position != 100 || projectID != nil {
		t.Fatalf(
			"rejected moves changed issue: title=%q position=%v project_id=%v",
			title, position, projectID,
		)
	}
}
