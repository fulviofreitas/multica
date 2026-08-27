package testutil

import (
	"strings"
	"testing"
)

func TestEqual(t *testing.T) {
	spy := &spyTB{}
	Equal(spy, 201, 201, "response body")
	if spy.failed {
		t.Fatal("Equal failed for identical values")
	}

	spy.capture(func() {
		Equal(spy, 400, 201, `{"error":"invalid"}`)
	})
	for _, want := range []string{"invalid", "got 400", "want 201"} {
		if !strings.Contains(spy.message, want) {
			t.Fatalf("failure message %q missing %q", spy.message, want)
		}
	}
}
