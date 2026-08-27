package handler

import (
	"context"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func newTimezoneTestUser(t *testing.T, email string) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Timezone Test", email,
	).Scan(&userID); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}

func TestUpdateMeAcceptsTimezone(t *testing.T) {
	userID := newTimezoneTestUser(t, "tz-set@multica.ai")

	req := newPatchMeRequest(userID, `{"timezone":"Asia/Shanghai"}`)
	w := testutil.Call(t, testHandler.UpdateMe, req).Want(200)

	var stored *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT timezone FROM "user" WHERE id = $1`, userID,
	).Scan(&stored); err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	if stored == nil || *stored != "Asia/Shanghai" {
		t.Fatalf("expected timezone=Asia/Shanghai, got %v", stored)
	}

	var resp map[string]any
	w.JSON(&resp)
	if got, _ := resp["timezone"].(string); got != "Asia/Shanghai" {
		t.Fatalf("expected response timezone=Asia/Shanghai, got %v", resp["timezone"])
	}
}

func TestUpdateMeRejectsInvalidTimezone(t *testing.T) {
	userID := newTimezoneTestUser(t, "tz-reject@multica.ai")

	req := newPatchMeRequest(userID, `{"timezone":"Not/A/Real/Zone"}`)
	testutil.Call(t, testHandler.UpdateMe, req).Want(400)

	var stored *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT timezone FROM "user" WHERE id = $1`, userID,
	).Scan(&stored); err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	if stored != nil {
		t.Fatalf("expected timezone unchanged (NULL), got %v", *stored)
	}
}

// COALESCE semantics — omitting timezone must NOT clear an existing value.
func TestUpdateMePreservesTimezoneWhenNotProvided(t *testing.T) {
	userID := newTimezoneTestUser(t, "tz-preserve@multica.ai")

	if _, err := testPool.Exec(context.Background(),
		`UPDATE "user" SET timezone = 'America/Los_Angeles' WHERE id = $1`, userID,
	); err != nil {
		t.Fatalf("preset timezone: %v", err)
	}

	req := newPatchMeRequest(userID, `{"name":"Updated Name"}`)
	testutil.Call(t, testHandler.UpdateMe, req).Want(200)

	var stored *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT timezone FROM "user" WHERE id = $1`, userID,
	).Scan(&stored); err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	if stored == nil || *stored != "America/Los_Angeles" {
		t.Fatalf("expected timezone preserved, got %v", stored)
	}
}

// Explicit clear: `"timezone": ""` should NULL the column so the frontend
// falls back to the browser-detected tz again. Without the CASE branch in
// the UPDATE query this would either be a no-op (COALESCE) or a validation
// error.
func TestUpdateMeClearsTimezoneOnEmptyString(t *testing.T) {
	userID := newTimezoneTestUser(t, "tz-clear@multica.ai")

	if _, err := testPool.Exec(context.Background(),
		`UPDATE "user" SET timezone = 'Asia/Shanghai' WHERE id = $1`, userID,
	); err != nil {
		t.Fatalf("preset timezone: %v", err)
	}

	req := newPatchMeRequest(userID, `{"timezone":""}`)
	w := testutil.Call(t, testHandler.UpdateMe, req).Want(200)

	var stored *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT timezone FROM "user" WHERE id = $1`, userID,
	).Scan(&stored); err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	if stored != nil {
		t.Fatalf("expected timezone cleared to NULL, got %v", *stored)
	}

	var resp map[string]any
	w.JSON(&resp)
	// JSON null marshals from *string nil — confirm the response reflects
	// the cleared state, so the frontend can switch its picker back to
	// "(browser)" without a refetch.
	if resp["timezone"] != nil {
		t.Fatalf("expected response timezone=null, got %v", resp["timezone"])
	}
}
