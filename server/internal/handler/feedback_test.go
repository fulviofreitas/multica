package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func TestCreateFeedbackHappyPath(t *testing.T) {
	clearFeedbackForTestUser(t)

	req := newRequest("POST", "/api/feedback", CreateFeedbackRequest{
		Message: "Love the product, dark mode flashes on startup",
	})
	w := testutil.Call(t, testHandler.CreateFeedback, req).Want(http.StatusCreated)
	var resp FeedbackResponse
	w.Decode(&resp)
	if resp.ID == "" {
		t.Fatal("expected feedback id in response")
	}
}

func TestCreateFeedbackStoresStructuredContext(t *testing.T) {
	clearFeedbackForTestUser(t)

	req := newRequest("POST", "/api/feedback", CreateFeedbackRequest{
		Message: "Desktop route crashed",
		Context: &FeedbackContext{
			Kind:    "desktop_route_error",
			Trigger: "route-errorElement",
			Error: FeedbackErrorContext{
				Name:    "TypeError",
				Message: "Cannot read properties of undefined",
				Stack:   "TypeError: Cannot read properties of undefined",
			},
		},
	})
	w := testutil.Call(t, testHandler.CreateFeedback, req).Want(http.StatusCreated)
	var resp FeedbackResponse
	w.Decode(&resp)

	var metadata []byte
	if err := testPool.QueryRow(
		context.Background(),
		`SELECT metadata FROM feedback WHERE id = $1`,
		parseUUID(resp.ID),
	).Scan(&metadata); err != nil {
		t.Fatalf("load feedback metadata: %v", err)
	}
	var stored struct {
		Context *FeedbackContext `json:"context"`
	}
	if err := json.Unmarshal(metadata, &stored); err != nil {
		t.Fatalf("decode feedback metadata: %v", err)
	}
	if stored.Context == nil {
		t.Fatal("expected structured context in feedback metadata")
	}
	if stored.Context.Kind != "desktop_route_error" {
		t.Fatalf("context kind = %q, want desktop_route_error", stored.Context.Kind)
	}
	if stored.Context.Trigger != "route-errorElement" {
		t.Fatalf("context trigger = %q, want route-errorElement", stored.Context.Trigger)
	}
	if stored.Context.Error.Name != "TypeError" {
		t.Fatalf("error name = %q, want TypeError", stored.Context.Error.Name)
	}
	if stored.Context.Error.Message != "Cannot read properties of undefined" {
		t.Fatalf(
			"error message = %q, want Cannot read properties of undefined",
			stored.Context.Error.Message,
		)
	}
	if stored.Context.Error.Stack != "TypeError: Cannot read properties of undefined" {
		t.Fatalf(
			"error stack = %q, want TypeError: Cannot read properties of undefined",
			stored.Context.Error.Stack,
		)
	}
}

func TestCreateFeedbackRejectsMalformedContext(t *testing.T) {
	req := newRequest("POST", "/api/feedback", map[string]any{
		"message": "Desktop route crashed",
		"context": "not-an-object",
	})
	testutil.Call(t, testHandler.CreateFeedback, req).Want(http.StatusBadRequest)
}

func TestCreateFeedbackRejectsUnknownContextKind(t *testing.T) {
	req := newRequest("POST", "/api/feedback", CreateFeedbackRequest{
		Message: "Desktop route crashed",
		Context: &FeedbackContext{
			Kind:    "arbitrary_diagnostic",
			Trigger: "route-errorElement",
			Error: FeedbackErrorContext{
				Name:    "TypeError",
				Message: "Cannot read properties of undefined",
			},
		},
	})
	testutil.Call(t, testHandler.CreateFeedback, req).Want(http.StatusBadRequest)
}

func TestCreateFeedbackRejectsEmptyContextFields(t *testing.T) {
	tests := []struct {
		name    string
		context FeedbackContext
	}{
		{
			name:    "empty context",
			context: FeedbackContext{},
		},
		{
			name: "trigger",
			context: FeedbackContext{
				Kind: desktopRouteErrorFeedbackContextKind,
				Error: FeedbackErrorContext{
					Name:    "TypeError",
					Message: "Cannot read properties of undefined",
				},
			},
		},
		{
			name: "error name",
			context: FeedbackContext{
				Kind:    desktopRouteErrorFeedbackContextKind,
				Trigger: "route-errorElement",
				Error: FeedbackErrorContext{
					Name:    "   ",
					Message: "Cannot read properties of undefined",
				},
			},
		},
		{
			name: "error message",
			context: FeedbackContext{
				Kind:    desktopRouteErrorFeedbackContextKind,
				Trigger: "route-errorElement",
				Error: FeedbackErrorContext{
					Name:    "TypeError",
					Message: "\n\t",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest("POST", "/api/feedback", CreateFeedbackRequest{
				Message: "Desktop route crashed",
				Context: &tt.context,
			})
			testutil.Call(t, testHandler.CreateFeedback, req).Want(http.StatusBadRequest)
		})
	}
}

func TestCreateFeedbackEmptyMessage(t *testing.T) {
	req := newRequest("POST", "/api/feedback", CreateFeedbackRequest{Message: "   "})
	testutil.Call(t, testHandler.CreateFeedback, req).Want(http.StatusBadRequest)
}

func TestCreateFeedbackRateLimit(t *testing.T) {
	clearFeedbackForTestUser(t)

	for i := 0; i < feedbackHourlyRateLimit; i++ {
		req := newRequest("POST", "/api/feedback", CreateFeedbackRequest{
			Message: "feedback #" + strconv.Itoa(i),
		})
		w := testutil.Call(t, testHandler.CreateFeedback, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("iteration %d: expected 201, got %d: %s", i, w.Code, w.Body.String())
		}
	}
	req := newRequest("POST", "/api/feedback", CreateFeedbackRequest{Message: "one too many"})
	testutil.Call(t, testHandler.CreateFeedback, req).Want(http.StatusTooManyRequests)
}

// clearFeedbackForTestUser wipes all feedback rows for the shared test user
// at both test start (fresh state) and test end (via t.Cleanup), so tests
// in this file don't interfere with each other or with the hourly rate-limit
// window when run in sequence.
func clearFeedbackForTestUser(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM feedback WHERE user_id = $1`, parseUUID(testUserID)); err != nil {
		t.Fatalf("clear feedback: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM feedback WHERE user_id = $1`, parseUUID(testUserID))
	})
}
