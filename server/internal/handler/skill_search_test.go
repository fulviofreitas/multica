package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func TestSearchSkillsReturnsNormalizedClawHubCandidates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if got := r.URL.Query().Get("q"); got != "react" {
				t.Fatalf("expected q=react, got %q", got)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"results": []map[string]any{
					{
						"slug":        "react",
						"displayName": "React",
						"summary":     "React engineering skill",
						"ownerHandle": "ivangdavila",
					},
					{
						"slug":        "react-expert",
						"displayName": "React Expert",
						"summary":     "Advanced React review",
						"ownerHandle": "veeramanikandanr48",
					},
				},
			})
		case "/skills/react":
			writeJSON(w, http.StatusOK, map[string]any{
				"skill": map[string]any{
					"slug":        "react",
					"displayName": "React",
					"summary":     "React engineering skill",
					"stats": map[string]any{
						"installsAllTime": 62,
						"stars":           3,
					},
				},
			})
		case "/skills/react-expert":
			writeJSON(w, http.StatusOK, map[string]any{
				"skill": map[string]any{
					"slug":        "react-expert",
					"displayName": "React Expert",
					"summary":     "Advanced React review",
					"stats": map[string]any{
						"installsAllTime": 11,
						"stars":           7,
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	oldBase := clawHubAPIBase
	clawHubAPIBase = upstream.URL
	t.Cleanup(func() { clawHubAPIBase = oldBase })

	req := newRequest(http.MethodGet, "/api/skills/search?q=react", nil)
	w := testutil.Call(t, testHandler.SearchSkills, req).Want(http.StatusOK)

	var got []map[string]any
	w.JSON(&got)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %#v", len(got), got)
	}
	first := got[0]
	if first["name"] != "React" {
		t.Fatalf("expected normalized name, got %#v", first["name"])
	}
	if first["url"] != "https://clawhub.ai/ivangdavila/react" {
		t.Fatalf("expected importable ClawHub URL, got %#v", first["url"])
	}
	if first["source"] != "clawhub.ai" {
		t.Fatalf("expected source clawhub.ai, got %#v", first["source"])
	}
	if first["repo"] != nil {
		t.Fatalf("repo should be null when ClawHub has no GitHub repo field, got %#v", first["repo"])
	}
	if first["github_stars"] != nil {
		t.Fatalf("github_stars should not use ClawHub stars, got %#v", first["github_stars"])
	}
	if first["install_count"] != float64(62) {
		t.Fatalf("expected install_count from details stats, got %#v", first["install_count"])
	}
	if first["description"] != "React engineering skill" {
		t.Fatalf("expected description from summary, got %#v", first["description"])
	}
}

func TestSearchSkillsEmptyQueryReturns400(t *testing.T) {

	req := newRequest(http.MethodGet, "/api/skills/search?q=", nil)
	w := testutil.Call(t, testHandler.SearchSkills, req).Want(http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "query is required") {
		t.Fatalf("expected query is required error, got %s", w.Body.String())
	}
}

func TestSearchSkillsUpstreamUnavailableReturnsStructuredError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary outage", http.StatusBadGateway)
	}))
	defer upstream.Close()

	oldBase := clawHubAPIBase
	clawHubAPIBase = upstream.URL
	t.Cleanup(func() { clawHubAPIBase = oldBase })

	req := newRequest(http.MethodGet, "/api/skills/search?q=react", nil)
	w := testutil.Call(t, testHandler.SearchSkills, req).Want(http.StatusBadGateway)
	var got map[string]string
	w.JSON(&got)
	if got["code"] != "upstream_unavailable" {
		t.Fatalf("expected structured upstream_unavailable code, got %#v", got)
	}
}
