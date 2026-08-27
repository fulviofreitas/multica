package handler

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	testutil "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestSetAgentRuntimeSkillEnabledBroadcastsAgentStatus guards the realtime
// invalidation path for runtime-skill toggles (Howard review on MUL-5101):
// persisting a disabled_runtime_skills override must publish an "agent:status"
// event so every other open web/desktop/mobile client invalidates
// workspaceKeys.agents and drops its stale toggle state — mirroring the
// workspace-skill toggle in writeUpdatedAgentSkills. Before the fix the handler
// committed and returned 204 without broadcasting, so only the initiating tab
// refreshed while other clients kept showing the old state.
func TestSetAgentRuntimeSkillEnabledBroadcastsAgentStatus(t *testing.T) {
	runtimeID := createRuntimeLocalSkillTestRuntime(t, testUserID)
	var agentID string
	agentID = dbfx.Insert(t, "agent", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Runtime Skill Broadcast " + t.Name(), "description": testutil.Raw("''"), "runtime_mode": testutil.Raw("'local'"), "runtime_config": testutil.Raw("'{}'::jsonb"), "runtime_id": runtimeID, "visibility": testutil.Raw("'private'"), "permission_mode": testutil.Raw("'private'"), "max_concurrent_tasks": testutil.Raw("1"), "owner_id": testUserID})

	// The bus is synchronous, so counts observed right after each request are
	// deterministic; the mutex only guards against a future async listener.
	var mu sync.Mutex
	var broadcasts []string
	testHandler.Bus.Subscribe(protocol.EventAgentStatus, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		agent, ok := payload["agent"].(AgentResponse)
		if !ok {
			return
		}
		mu.Lock()
		broadcasts = append(broadcasts, agent.ID)
		mu.Unlock()
	})

	countFor := func(id string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, a := range broadcasts {
			if a == id {
				n++
			}
		}
		return n
	}

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
	if n := countFor(agentID); n != 1 {
		t.Fatalf("disabling a runtime skill must broadcast exactly one agent:status for %s, got %d", agentID, n)
	}

	if w := setEnabled(true); w.Code != 204 {
		t.Fatalf("enable inherited skill: expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if n := countFor(agentID); n != 2 {
		t.Fatalf("re-enabling a runtime skill must broadcast a second agent:status for %s, got %d", agentID, n)
	}
}
