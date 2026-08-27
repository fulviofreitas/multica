package testutil

// Equal reports a value mismatch without encoding any product rule. Context
// is included verbatim so callers can retain the diagnostic that matters to
// the test case.
func Equal[T comparable](t TB, got, want T, context string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", context, got, want)
	}
}
