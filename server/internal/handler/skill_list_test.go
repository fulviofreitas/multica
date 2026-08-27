package handler

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// TestListSkills_OmitsContent guards the fix for GH multica-ai/multica#2174:
// the workspace skill list endpoint must not ship the SKILL.md `content`
// blob, which used to bloat the payload past CLI timeouts on workspaces with
// many large skills. The detail endpoint still returns content (covered by
// TestGetSkill_IncludesContent below).
func TestListSkills_OmitsContent(t *testing.T) {
	skillID := insertHandlerTestSkill(t, "list-omits-content", strings.Repeat("a", 4096))

	req := newRequest("GET", "/api/skills?workspace_id="+testWorkspaceID, nil)
	w := testutil.Call(t, testHandler.ListSkills, req).Want(200)

	// Decode into a generic shape so we can prove the wire format has no
	// `content` field at all — not "content present but empty", which would
	// still leave the bytes on the wire.
	var rows []map[string]any
	w.JSON(&rows)

	var found bool
	for _, row := range rows {
		if row["id"] != skillID {
			continue
		}
		found = true
		if _, ok := row["content"]; ok {
			t.Fatalf("ListSkills: response must not include `content` field, got: %v", row)
		}
		// Other expected list fields should still be present.
		for _, key := range []string{"id", "name", "description", "config", "created_at", "updated_at", "workspace_id"} {
			if _, ok := row[key]; !ok {
				t.Fatalf("ListSkills: missing expected field %q in response: %v", key, row)
			}
		}
	}
	if !found {
		t.Fatalf("ListSkills: inserted skill %s not in response", skillID)
	}
}

// TestGetSkill_IncludesContent confirms the detail endpoint still ships the
// full SKILL.md body — the list-summary change must not regress single-skill
// reads.
func TestGetSkill_IncludesContent(t *testing.T) {
	body := "# detail body\nstill served on /api/skills/{id}"
	skillID := insertHandlerTestSkill(t, "detail-includes-content", body)

	req := newRequest("GET", "/api/skills/"+skillID, nil)
	req = withURLParam(req, "id", skillID)
	w := testutil.Call(t, testHandler.GetSkill, req).Want(200)

	var resp map[string]any
	w.JSON(&resp)
	if got, _ := resp["content"].(string); got != body {
		t.Fatalf("GetSkill: expected content %q, got %q", body, got)
	}
}

// TestListAgentSkills_OmitsContent: same constraint for the agent-scoped
// listing — gpt-boy review of the original fix flagged this as a sister case
// because `multica agent skills list` follows the same shape rules.
func TestListAgentSkills_OmitsContent(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Handler Skill Summary Test", nil)
	skillID := insertHandlerTestSkill(t, "agent-skill-omits-content", strings.Repeat("b", 1024))
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`,
		agentID, skillID,
	); err != nil {
		t.Fatalf("attach skill to agent: %v", err)
	}

	req := newRequest("GET", "/api/agents/"+agentID+"/skills", nil)
	req = withURLParam(req, "id", agentID)
	w := testutil.Call(t, testHandler.ListAgentSkills, req).Want(200)

	var rows []map[string]any
	w.JSON(&rows)
	if len(rows) == 0 {
		t.Fatalf("ListAgentSkills: expected at least 1 skill")
	}
	for _, row := range rows {
		if _, ok := row["content"]; ok {
			t.Fatalf("ListAgentSkills: response must not include `content` field, got: %v", row)
		}
	}
}

func TestSetAgentSkillEnabledControlsExecutionWithoutRemovingAssignment(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Handler Skill Toggle Test", nil)
	skillID := insertHandlerTestSkill(t, "agent-skill-toggle", "# Toggle me")
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`,
		agentID, skillID,
	); err != nil {
		t.Fatalf("attach skill to agent: %v", err)
	}

	setEnabled := func(enabled bool) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/agents/"+agentID+"/skills/"+skillID+"/enabled", map[string]any{
			"enabled": enabled,
		})
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", agentID)
		rctx.URLParams.Add("skillId", skillID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		testHandler.SetAgentSkillEnabled(w, req)
		return w
	}

	if w := setEnabled(false); w.Code != 200 {
		t.Fatalf("disable skill: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Disabled assignments remain visible to management surfaces.
	rows, err := testHandler.Queries.ListAgentSkillSummaries(context.Background(), parseUUID(agentID))
	if err != nil || len(rows) != 1 || rows[0].Enabled {
		t.Fatalf("disabled assignment not preserved: rows=%+v err=%v", rows, err)
	}
	// Execution only receives enabled skills.
	active, err := testHandler.Queries.ListAgentSkills(context.Background(), parseUUID(agentID))
	if err != nil || len(active) != 0 {
		t.Fatalf("disabled skill leaked into execution: skills=%+v err=%v", active, err)
	}

	if w := setEnabled(true); w.Code != 200 {
		t.Fatalf("enable skill: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	active, err = testHandler.Queries.ListAgentSkills(context.Background(), parseUUID(agentID))
	if err != nil || len(active) != 1 || active[0].Name == "" {
		t.Fatalf("re-enabled skill missing from execution: skills=%+v err=%v", active, err)
	}
}

func TestSetAgentRuntimeSkillEnabledPersistsScopedOverride(t *testing.T) {
	runtimeID := createRuntimeLocalSkillTestRuntime(t, testUserID)
	var agentID string
	agentID = dbfx.Insert(t, "agent", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Runtime Skill Toggle " + t.Name(), "description": testutil.Raw("''"), "runtime_mode": testutil.Raw("'local'"), "runtime_config": testutil.Raw("'{}'::jsonb"), "runtime_id": runtimeID, "visibility": testutil.Raw("'private'"), "permission_mode": testutil.Raw("'private'"), "max_concurrent_tasks": testutil.Raw("1"), "owner_id": testUserID})

	setEnabled := func(enabled bool) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/agents/"+agentID+"/runtime-skills/enabled", map[string]any{
			"runtime_id": runtimeID,
			"root":       "provider",
			"key":        "review",
			"name":       "Review Helper",
			"enabled":    enabled,
		})
		req = withURLParam(req, "id", agentID)
		testHandler.SetAgentRuntimeSkillEnabled(w, req)
		return w
	}

	if w := setEnabled(false); w.Code != 204 {
		t.Fatalf("disable inherited skill: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	row, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(agentID))
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	disabled := decodeDisabledRuntimeSkills(row.DisabledRuntimeSkills)
	if len(disabled) != 1 || disabled[0].RuntimeID != runtimeID ||
		disabled[0].Provider != "claude" || disabled[0].Root != "provider" || disabled[0].Key != "review" ||
		disabled[0].Name != "Review Helper" {
		t.Fatalf("unexpected disabled runtime skills: %+v", disabled)
	}
	if w := setEnabled(false); w.Code != 204 {
		t.Fatalf("repeat disable inherited skill: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	row, err = testHandler.Queries.GetAgent(context.Background(), parseUUID(agentID))
	if err != nil || len(decodeDisabledRuntimeSkills(row.DisabledRuntimeSkills)) != 1 {
		t.Fatalf("repeat disable was not idempotent: row=%+v err=%v", row, err)
	}

	if w := setEnabled(true); w.Code != 204 {
		t.Fatalf("enable inherited skill: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	row, err = testHandler.Queries.GetAgent(context.Background(), parseUUID(agentID))
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	if disabled = decodeDisabledRuntimeSkills(row.DisabledRuntimeSkills); len(disabled) != 0 {
		t.Fatalf("re-enabled skill still disabled: %+v", disabled)
	}
}

// TestGetSkill_MalformedUUIDReturns400 guards the handler UUID parsing
// convention (CLAUDE.md → "Backend Handler UUID Parsing Convention"): raw
// `id` URL params on the request boundary must be validated with
// parseUUIDOrBadRequest, not the panic-prone parseUUID. Before the fix
// the malformed input panicked in MustParseUUID and was rescued by the
// chi Recoverer middleware as a 500.
func TestGetSkill_MalformedUUIDReturns400(t *testing.T) {

	req := newRequest("GET", "/api/skills/not-a-uuid", nil)
	req = withURLParam(req, "id", "not-a-uuid")
	testutil.Call(t, testHandler.GetSkill, req).Want(400)
}

// insertHandlerTestSkill writes a skill row directly via SQL and registers a
// cleanup hook. We bypass the create handler to keep the test focused on the
// list/detail wire shape and to make it easy to inject a large body.
func insertHandlerTestSkill(t *testing.T, namePrefix, content string) string {
	t.Helper()
	name := namePrefix + "-" + t.Name()
	var id string
	id = dbfx.Insert(t, "skill", testutil.Cols{"workspace_id": testWorkspaceID, "name": name, "description": "fixture", "content": content, "config": testutil.Raw("'{}'::jsonb"), "created_by": testUserID})
	return id
}
