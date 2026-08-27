package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

// failingUpdateStore returns a chosen error from Create. The embedded interface
// supplies the rest of the method set; InitiateUpdate only reaches Create, so
// anything else calling through would panic loudly rather than pass silently.
type failingUpdateStore struct {
	UpdateStore
	createErr error
}

func (s *failingUpdateStore) Create(context.Context, string, string, string) (*UpdateRequest, error) {
	return nil, s.createErr
}

// TestInitiateUpdate_InfrastructureErrorIsNotAConflict pins the classification
// split this PR adds alongside GH #6264. InitiateUpdate used to answer every
// UpdateStore.Create failure with a 409 carrying err.Error(). That was survivable
// while the CLI hid conflict bodies; now that they print by default, a Redis
// outage would show its dial address to the user and read as a conflict they
// could fix by retrying.
func TestInitiateUpdate_InfrastructureErrorIsNotAConflict(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	// Shape mirrors RedisUpdateStore.Create wrapping a dial failure.
	infraErr := errors.New("reserve active update: dial tcp 10.1.2.3:6379: connect: connection refused")

	original := testHandler.UpdateStore
	testHandler.UpdateStore = &failingUpdateStore{createErr: infraErr}
	t.Cleanup(func() { testHandler.UpdateStore = original })

	r := newRequest("POST", "/api/runtimes/"+testRuntimeID+"/update", map[string]any{"target_version": "v1.2.3"})
	r = withURLParams(r, "runtimeId", testRuntimeID)

	w := testutil.Call(t, testHandler.InitiateUpdate, r).Want(http.StatusInternalServerError)
	body := w.Body.String()
	for _, leak := range []string{"10.1.2.3:6379", "connection refused", "reserve active update"} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaked internal detail %q: %s", leak, body)
		}
	}
}

// TestInitiateUpdate_InProgressStillConflicts is the positive half: the one
// Create failure a caller can actually act on keeps its 409 and its actionable
// wording, which is exactly what the CLI now surfaces by default.
func TestInitiateUpdate_InProgressStillConflicts(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	original := testHandler.UpdateStore
	testHandler.UpdateStore = &failingUpdateStore{createErr: errUpdateInProgress}
	t.Cleanup(func() { testHandler.UpdateStore = original })

	r := newRequest("POST", "/api/runtimes/"+testRuntimeID+"/update", map[string]any{"target_version": "v1.2.3"})
	r = withURLParams(r, "runtimeId", testRuntimeID)

	w := testutil.Call(t, testHandler.InitiateUpdate, r).Want(http.StatusConflict)
	if !strings.Contains(w.Body.String(), "already in progress") {
		t.Fatalf("409 should keep its actionable message, got %s", w.Body.String())
	}
}
