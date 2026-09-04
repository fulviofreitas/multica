// reconnect_test.go exercises the reconnect POLICY layer (reconnect.go,
// subtask 2.5): close-code classification and IDENTIFY-budget spacing.
// reconnect.go itself holds no logger — it only classifies and decides, it
// never logs — so the log-observability tests proving a rejected-RESUME
// fresh-identify decision is distinguishable in logs from a fresh-IDENTIFY
// session invalidating on its own live in connect_test.go instead, where
// the decision this file computes is actually logged. See
// TestConnect_ResumeRejected_LogsDistinctFromFreshSessionInvalidated and
// TestConnect_FreshIdentifyDecision_ClearsCache.
package discord

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgtype"
)

// reconnectTestID builds a distinct, valid pgtype.UUID for test fixtures.
// testInstallationID (resume_test.go) and fakeClock (resume_test.go) are
// reused as-is from this file: same package, same shape this file would
// otherwise duplicate.
func reconnectTestID(t *testing.T, b byte) pgtype.UUID {
	return testInstallationID(t, b)
}

func closeErr(code int) *websocket.CloseError {
	return &websocket.CloseError{Code: code, Text: "test"}
}

// ---- close-code triage ----

func TestClassifyCloseCode_FullTable(t *testing.T) {
	// The complete Discord Gateway close-code -> action mapping. See
	// reconnect.go's discordCloseCodes doc comment for the per-code
	// reasoning, including which codes are flagged UNCERTAIN.
	cases := []struct {
		code   int
		name   string
		action ReconnectAction
	}{
		{4000, "unknown_error", ActionResume},
		{4001, "unknown_opcode", ActionResume},
		{4002, "decode_error", ActionResume},
		{4003, "not_authenticated", ActionFreshIdentify},
		{4004, "authentication_failed", ActionFatal},
		{4005, "already_authenticated", ActionResume},
		{4007, "invalid_seq", ActionFreshIdentify},
		{4008, "rate_limited", ActionResume},
		{4009, "session_timed_out", ActionResume},
		{4010, "invalid_shard", ActionFatal},
		{4011, "sharding_required", ActionFatal},
		{4012, "invalid_api_version", ActionFatal},
		{4013, "invalid_intents", ActionFatal},
		{4014, "disallowed_intents", ActionFatal},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d_%s", tc.code, tc.name), func(t *testing.T) {
			info := classifyCloseCode(tc.code)
			if info.action != tc.action {
				t.Errorf("code %d: action = %v, want %v", tc.code, info.action, tc.action)
			}
			if info.name != tc.name {
				t.Errorf("code %d: name = %q, want %q", tc.code, info.name, tc.name)
			}
			if tc.action == ActionFatal && info.operatorHint == "" {
				t.Errorf("code %d: FATAL classification must carry an operator hint", tc.code)
			}
			if tc.action != ActionFatal && info.operatorHint != "" {
				t.Errorf("code %d: non-fatal classification should not carry an operator hint, got %q", tc.code, info.operatorHint)
			}
		})
	}
}

// TestClassifyCloseCode_UnknownDefaultsToResumable asserts the explicit
// safe default this package relies on: any close code Discord has not
// documented here (including the unassigned 4006, and anything above 4014)
// must default to ActionResume, never to ActionFreshIdentify (would burn
// IDENTIFY budget on every unrecognized code) or ActionFatal (would
// permanently kill a possibly-recoverable installation).
func TestClassifyCloseCode_UnknownDefaultsToResumable(t *testing.T) {
	for _, code := range []int{4006, 4015, 4999} {
		info := classifyCloseCode(code)
		if info.action != ActionResume {
			t.Errorf("unmapped code %d: action = %v, want ActionResume", code, info.action)
		}
	}
}

func TestDiscordCloseCode_ExtractsFromWrappedCloseError(t *testing.T) {
	wrapped := fmt.Errorf("discord gateway: read_error: %w", closeErr(4004))
	code, ok := discordCloseCode(wrapped)
	if !ok || code != 4004 {
		t.Fatalf("discordCloseCode(wrapped) = (%d, %v), want (4004, true)", code, ok)
	}
}

func TestDiscordCloseCode_NonCloseErrorIsAbsent(t *testing.T) {
	if _, ok := discordCloseCode(errors.New("boom")); ok {
		t.Fatal("discordCloseCode should report absent for a non-CloseError")
	}
	if _, ok := discordCloseCode(nil); ok {
		t.Fatal("discordCloseCode should report absent for a nil error")
	}
}

func TestDiscordCloseCode_OutOfRangeIsAbsent(t *testing.T) {
	// A normal-closure/going-away code (< 4000) is not a Discord
	// app-specific code and must not be misclassified via this path.
	if _, ok := discordCloseCode(closeErr(1000)); ok {
		t.Fatal("discordCloseCode should report absent for a non-Discord (<4000) close code")
	}
}

// ---- DisconnectReason mapping ----

func TestClassifyGatewayError_DisconnectReasons(t *testing.T) {
	cases := []struct {
		name   string
		err    *GatewayError
		action ReconnectAction
	}{
		{"zombie_connection", &GatewayError{Reason: ReasonZombieConnection, Err: errors.New("no ack")}, ActionResume},
		{"reconnect_requested", &GatewayError{Reason: ReasonReconnectRequested}, ActionResume},
		{"server_closed", &GatewayError{Reason: ReasonServerClosed, Err: &websocket.CloseError{Code: websocket.CloseNormalClosure}}, ActionResume},
		{"read_error_generic", &GatewayError{Reason: ReasonReadError, Err: errors.New("connection reset")}, ActionResume},
		{"invalid_session_resumable_true", &GatewayError{Reason: ReasonInvalidSession, Resumable: true}, ActionResume},
		{"invalid_session_resumable_false", &GatewayError{Reason: ReasonInvalidSession, Resumable: false}, ActionFreshIdentify},
		// A Discord app-specific close code arriving via ReasonReadError
		// (see gateway.go's Run: only CloseNormalClosure/CloseGoingAway
		// are recognized as ReasonServerClosed, everything else — including
		// 4000-4014 — falls through to ReasonReadError) must take priority
		// over the coarser reason.
		{"read_error_with_discord_close_code", &GatewayError{Reason: ReasonReadError, Err: closeErr(4013)}, ActionFatal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := classifyGatewayError(tc.err)
			if info.action != tc.action {
				t.Errorf("%s: action = %v, want %v", tc.name, info.action, tc.action)
			}
		})
	}
}

func TestClassify_NonGatewayErrorDefaultsToResumable(t *testing.T) {
	// DialGateway's own pre-session failures (dial error, HELLO timeout,
	// etc.) are plain errors, not *GatewayError. No session exists yet, so
	// there is nothing to invalidate; the safe move is the same as any
	// other resumable outcome.
	info := classify(errors.New("dial tcp: connection refused"))
	if info.action != ActionResume {
		t.Errorf("non-GatewayError: action = %v, want ActionResume", info.action)
	}
}

// ---- Reconnector.Decide ----

func TestReconnector_FatalDecisionsDoNotRetryAndCarryOperatorMessage(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	r := NewReconnector(clk.Now)
	id := reconnectTestID(t, 1)

	fatalCodes := []int{4004, 4010, 4011, 4012, 4013, 4014}
	for _, code := range fatalCodes {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			err := &GatewayError{Reason: ReasonReadError, Err: closeErr(code)}
			d := r.Decide(id, err)
			if d.Retry {
				t.Fatalf("close code %d: Retry = true, want false", code)
			}
			if d.Action != ActionFatal {
				t.Fatalf("close code %d: Action = %v, want ActionFatal", code, d.Action)
			}
			if d.OperatorMessage == "" {
				t.Fatalf("close code %d: OperatorMessage is empty, want an operator-actionable message", code)
			}
			if d.CloseCodeName == "" {
				t.Fatalf("close code %d: CloseCodeName is empty", code)
			}
		})
	}
}

// TestReconnector_ResumeDecisionsAreNotSpaced asserts identify spacing does
// NOT apply to RESUME decisions: back-to-back resumable failures for the
// same installation must never be delayed by identifySpacing, since a
// RESUME attempt does not consume IDENTIFY budget.
func TestReconnector_ResumeDecisionsAreNotSpaced(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	r := NewReconnector(clk.Now)
	id := reconnectTestID(t, 1)

	for i := 0; i < 5; i++ {
		d := r.Decide(id, &GatewayError{Reason: ReasonZombieConnection, Err: errors.New("no ack")})
		if !d.Retry || d.Action != ActionResume {
			t.Fatalf("iteration %d: Retry=%v Action=%v, want Retry=true Action=ActionResume", i, d.Retry, d.Action)
		}
		if d.Wait != 0 {
			t.Fatalf("iteration %d: Wait = %v, want 0 (resume must not be identify-spaced)", i, d.Wait)
		}
		// No clock advance between iterations: proves spacing genuinely
		// does not apply, not that we happened to wait long enough.
	}
}

// TestReconnector_FreshIdentifyIsSpaced asserts the inverse: two
// back-to-back fresh-IDENTIFY decisions for the SAME installation, with no
// simulated time elapsed, must be spaced by identifySpacing.
func TestReconnector_FreshIdentifyIsSpaced(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	r := NewReconnector(clk.Now)
	id := reconnectTestID(t, 1)

	err := &GatewayError{Reason: ReasonInvalidSession, Resumable: false}

	first := r.Decide(id, err)
	if first.Wait != 0 {
		t.Fatalf("first fresh-identify decision: Wait = %v, want 0 (nothing reserved yet)", first.Wait)
	}

	second := r.Decide(id, err)
	if second.Wait != identifySpacing {
		t.Fatalf("second fresh-identify decision (no elapsed time): Wait = %v, want %v", second.Wait, identifySpacing)
	}
}

// TestReconnector_FreshIdentifySpacing_DoesNotThrottleOtherInstallations
// asserts one installation's IDENTIFY spacing never delays another's,
// verified concurrently under -race.
func TestReconnector_FreshIdentifySpacing_DoesNotThrottleOtherInstallations(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	r := NewReconnector(clk.Now)
	idA := reconnectTestID(t, 1)
	idB := reconnectTestID(t, 2)

	err := &GatewayError{Reason: ReasonInvalidSession, Resumable: false}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Decide(idA, err)
		}()
	}
	wg.Wait()

	// idB's very first decision must be unaffected by however much idA's
	// spacing schedule has been reserved.
	dB := r.Decide(idB, err)
	if dB.Wait != 0 {
		t.Fatalf("installation B: Wait = %v, want 0 (must not be throttled by installation A)", dB.Wait)
	}
}

// TestReconnector_ConcurrentDifferentInstallations exercises many
// installation ids deciding concurrently, for -race coverage of
// IdentifyLimiter's internal map.
func TestReconnector_ConcurrentDifferentInstallations(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	r := NewReconnector(clk.Now)
	err := &GatewayError{Reason: ReasonInvalidSession, Resumable: false}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		id := reconnectTestID(t, byte(i+1))
		for j := 0; j < 10; j++ {
			wg.Add(1)
			go func(id pgtype.UUID) {
				defer wg.Done()
				_ = r.Decide(id, err)
			}(id)
		}
	}
	wg.Wait()
}

// TestIdentifyBudget_PermanentFailureStaysUnder1000PerDay is the guard
// against the outage this subtask exists to prevent: a permanently failing
// installation (every single attempt ends in a not-resumable close, the
// worst case for IDENTIFY consumption) must never approach — let alone
// exceed — Discord's 1000-fresh-IDENTIFY-per-24h-per-bot budget. Exceeding
// it resets the bot token, which requires a human to paste a new one before
// the installation can work again.
//
// Arithmetic this test verifies empirically: steady-state rate is 24h / 90s
// identifySpacing = 960 fresh IDENTIFYs/day. The very first IDENTIFY of the
// simulated window is "free" (nothing was reserved before the window
// started), so the precise fencepost bound over any exact 24h window is
// floor(86400/90)+1 = 961 — still comfortably under 1000.
func TestIdentifyBudget_PermanentFailureStaysUnder1000PerDay(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	r := NewReconnector(clk.Now)
	id := reconnectTestID(t, 1)

	// Every attempt ends in a not-resumable Invalid Session: the worst
	// case, since every single decision demands a fresh IDENTIFY.
	err := &GatewayError{Reason: ReasonInvalidSession, Resumable: false}

	start := clk.Now()
	const window = 24 * time.Hour
	identifies := 0

	// Iteration cap as a belt-and-suspenders guard against an infinite loop
	// if this invariant is ever broken (only the very first Reserve() may
	// legitimately return Wait==0; every subsequent one for the same
	// installation must return a positive wait once the loop's own clock
	// advances). 2x the theoretical maximum is generous headroom.
	const maxIterations = 2000

	for clk.Now().Sub(start) < window {
		if identifies > maxIterations {
			t.Fatalf("exceeded %d iterations without covering the simulated 24h window; IdentifyLimiter.Reserve is not advancing time as expected", maxIterations)
		}
		d := r.Decide(id, err)
		if !d.Retry {
			t.Fatalf("a resumable/fresh-identify-eligible failure must always retry, got Retry=false (%s)", d.CloseCodeName)
		}
		if d.Action != ActionFreshIdentify {
			t.Fatalf("this scenario must always classify as ActionFreshIdentify, got %v", d.Action)
		}
		identifies++
		clk.Advance(d.Wait)
	}

	t.Logf("simulated 24h of permanent failure: %d fresh IDENTIFYs (budget: 1000/24h, steady-state spacing rate: 960/24h)", identifies)
	// 961, not 960: see this test's doc comment for the fencepost reasoning.
	// Either way this is comfortably under Discord's 1000/24h budget.
	if identifies > 961 {
		t.Fatalf("fresh IDENTIFYs over simulated 24h = %d, want <= 961 (24h/%v spacing, +1 fencepost)", identifies, identifySpacing)
	}
	if identifies >= 1000 {
		t.Fatalf("CATASTROPHIC: fresh IDENTIFYs over simulated 24h = %d, at/over Discord's 1000/24h budget — this would reset the bot token", identifies)
	}
}

// ---- IdentifyLimiter directly ----

func TestIdentifyLimiter_FirstReservationIsImmediate(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	l := NewIdentifyLimiter(clk.Now)
	id := reconnectTestID(t, 1)

	if wait := l.Reserve(id); wait != 0 {
		t.Fatalf("first Reserve() = %v, want 0", wait)
	}
}

func TestIdentifyLimiter_SecondReservationIsSpacedWhenNoTimeElapses(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	l := NewIdentifyLimiter(clk.Now)
	id := reconnectTestID(t, 1)

	l.Reserve(id)
	if wait := l.Reserve(id); wait != identifySpacing {
		t.Fatalf("second Reserve() with no elapsed time = %v, want %v", wait, identifySpacing)
	}
}

func TestIdentifyLimiter_ElapsedTimeReducesWait(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	l := NewIdentifyLimiter(clk.Now)
	id := reconnectTestID(t, 1)

	l.Reserve(id)
	clk.Advance(30 * time.Second)
	wait := l.Reserve(id)
	want := identifySpacing - 30*time.Second
	if wait != want {
		t.Fatalf("Reserve() after 30s elapsed = %v, want %v", wait, want)
	}
}

func TestIdentifyLimiter_ElapsedTimePastSpacingResetsToImmediate(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	l := NewIdentifyLimiter(clk.Now)
	id := reconnectTestID(t, 1)

	l.Reserve(id)
	clk.Advance(identifySpacing + time.Second)
	if wait := l.Reserve(id); wait != 0 {
		t.Fatalf("Reserve() after spacing fully elapsed = %v, want 0", wait)
	}
}

func TestIdentifyLimiter_DifferentInstallationsAreIndependent(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0))
	l := NewIdentifyLimiter(clk.Now)
	idA := reconnectTestID(t, 1)
	idB := reconnectTestID(t, 2)

	l.Reserve(idA)
	l.Reserve(idA)
	if wait := l.Reserve(idB); wait != 0 {
		t.Fatalf("installation B's first Reserve() = %v, want 0 (must not inherit A's schedule)", wait)
	}
}
