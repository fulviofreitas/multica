package discord

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Canonical test layer for the Discord typing indicator (Task Master subtask
// 3.5). No DB, no network: every server is httptest, every clock tick is a
// short, injected duration rather than a sleep-and-hope guess.

// requestLog is a small concurrency-safe recorder for observed POSTs.
type requestLog struct {
	mu    sync.Mutex
	paths []string
	auths []string
}

func (l *requestLog) record(path, auth string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, path)
	l.auths = append(l.auths, auth)
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.paths)
}

func (l *requestLog) last() (path, auth string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.paths) == 0 {
		return "", "", false
	}
	n := len(l.paths) - 1
	return l.paths[n], l.auths[n], true
}

func newTypingTestServer(t *testing.T, status int) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r.URL.Path, r.Header.Get("Authorization"))
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

// newTestNotifier builds a discordTypingNotifier wired to base, with a tight
// refresh interval and safety timeout so tests run fast and deterministically
// instead of relying on production-sized durations.
func newTestNotifier(base string, refresh, maxLifetime time.Duration) *discordTypingNotifier {
	n := NewDiscordTypingNotifier(nil, base, &http.Client{Timeout: time.Second}, nil)
	n.refreshInterval = refresh
	n.maxLifetime = maxLifetime
	return n
}

// newTestNotifierWithLogger is newTestNotifier plus an injected logger, for
// tests that need to capture what postTyping logs rather than only what it
// POSTs. newCapturingLogger (connect_test.go) is the logger this is meant to
// be paired with.
func newTestNotifierWithLogger(base string, refresh, maxLifetime time.Duration, logger *slog.Logger) *discordTypingNotifier {
	n := NewDiscordTypingNotifier(nil, base, &http.Client{Timeout: time.Second}, logger)
	n.refreshInterval = refresh
	n.maxLifetime = maxLifetime
	return n
}

// testInstallationUUID is a fixed, valid installation id for tests that need
// ResolvedInstallation.ID to round-trip through postTyping's "installation_id"
// log field (see TestDiscordTypingSuccessfulRefreshLogsPositiveSignal) without
// every existing testInstallation caller having to supply one.
var testInstallationUUID = pgtype.UUID{Bytes: [16]byte{0xaa}, Valid: true}

func testInstallation(config []byte) engine.ResolvedInstallation {
	return engine.ResolvedInstallation{
		ID:       testInstallationUUID,
		Platform: db.ChannelInstallation{Config: config},
	}
}

func testInboundMessage(channelID string) channel.InboundMessage {
	return channel.InboundMessage{
		Source: channel.Source{ChatID: channelID, ChatType: channel.ChatTypeGroup},
	}
}

// waitFor polls cond every 5ms until it returns true or the deadline passes,
// avoiding a fixed sleep for conditions that usually resolve quickly.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// newTestConfig builds a stored installConfig blob whose bot token round-
// trips to "bot-token-123" through decodeCredentials with a nil Decrypter
// (decryptToken's nil-decrypt path treats the base64-decoded bytes as
// plaintext directly — see config.go).
func newTestConfig(t *testing.T) []byte {
	t.Helper()
	cfg, err := json.Marshal(installConfig{
		AppID:             "app1",
		BotTokenEncrypted: base64.StdEncoding.EncodeToString([]byte("bot-token-123")),
	})
	if err != nil {
		t.Fatalf("marshal installConfig: %v", err)
	}
	return cfg
}

func TestDiscordTypingOnIngestedPostsImmediately(t *testing.T) {
	srv, log := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, time.Hour, time.Hour)
	sessionID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-1"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	if got := log.count(); got != 1 {
		t.Fatalf("expected exactly 1 immediate POST, got %d", got)
	}
	path, auth, ok := log.last()
	if !ok {
		t.Fatalf("expected a recorded request")
	}
	if path != "/channels/chan-1/typing" {
		t.Fatalf("unexpected path: %s", path)
	}
	if auth != "Bot bot-token-123" {
		t.Fatalf("unexpected Authorization header: %q", auth)
	}
}

func TestDiscordTypingRefreshesPeriodically(t *testing.T) {
	srv, log := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, 15*time.Millisecond, time.Hour)
	sessionID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-2"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	if !waitFor(t, 2*time.Second, func() bool { return log.count() >= 4 }) {
		t.Fatalf("expected refresh loop to post repeatedly, got %d requests", log.count())
	}
}

func TestDiscordTypingOnSettledStopsLoop(t *testing.T) {
	srv, log := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, 10*time.Millisecond, time.Hour)
	sessionID := pgtype.UUID{Bytes: [16]byte{3}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-3"), sessionID)

	if !waitFor(t, time.Second, func() bool { return log.count() >= 2 }) {
		t.Fatalf("expected at least 2 posts before settling, got %d", log.count())
	}

	n.mu.Lock()
	loop := n.loops[sessionID]
	n.mu.Unlock()
	if loop == nil {
		t.Fatalf("expected a registered loop before OnSettled")
	}

	n.OnSettled(context.Background(), sessionID)

	// Deterministic exit signal instead of a sleep-and-hope: runLoop closes
	// loop.done only after its for-select has returned.
	select {
	case <-loop.done:
	case <-time.After(time.Second):
		t.Fatalf("loop did not exit after OnSettled")
	}

	n.mu.Lock()
	_, stillRegistered := n.loops[sessionID]
	n.mu.Unlock()
	if stillRegistered {
		t.Fatalf("expected loop to be deregistered after OnSettled")
	}

	after := log.count()
	time.Sleep(60 * time.Millisecond) // several refresh intervals
	if got := log.count(); got != after {
		t.Fatalf("expected no further posts after OnSettled: had %d, now %d", after, got)
	}
}

// TestDiscordTypingClearsOnTerminalBusEvent guards the wiring the fake-Gateway
// suite could not exercise: OnIngested starts the refresh loop, but nothing
// settled a run that actually enqueued a task — the engine's OnSettled only
// fires on its "no task enqueued" branch — so the loop survived until the
// safety timeout, leaving "typing…" stuck for ~10 minutes after a completed or
// failed run. Register subscribes the notifier to the task-lifecycle bus; each
// terminal event must stop the loop for its session. Found on a live guild,
// 2026-09-02.
func TestDiscordTypingClearsOnTerminalBusEvent(t *testing.T) {
	cases := []struct {
		name  string
		event func(sessionID pgtype.UUID) events.Event
	}{
		// chat:done and task:cancelled carry the session on the envelope.
		{protocol.EventChatDone, func(id pgtype.UUID) events.Event {
			return events.Event{Type: protocol.EventChatDone, ChatSessionID: util.UUIDToString(id)}
		}},
		{protocol.EventTaskCancelled, func(id pgtype.UUID) events.Event {
			return events.Event{Type: protocol.EventTaskCancelled, ChatSessionID: util.UUIDToString(id)}
		}},
		// task:failed carries the session only in the broadcast payload map.
		{protocol.EventTaskFailed, func(id pgtype.UUID) events.Event {
			return events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"chat_session_id": util.UUIDToString(id)}}
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, log := newTypingTestServer(t, http.StatusNoContent)
			// A long refresh and a long safety timeout leave the bus event
			// under test as the ONLY thing that can stop the loop in-window.
			n := newTestNotifier(srv.URL, 20*time.Millisecond, time.Hour)
			bus := events.New()
			n.Register(bus)

			sessionID := pgtype.UUID{Bytes: [16]byte{byte(10 + i)}, Valid: true}
			n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-term"), sessionID)

			n.mu.Lock()
			loop := n.loops[sessionID]
			n.mu.Unlock()
			if loop == nil {
				t.Fatalf("expected a registered loop after OnIngested")
			}

			bus.Publish(tc.event(sessionID))

			select {
			case <-loop.done:
			case <-time.After(2 * time.Second):
				t.Fatalf("loop did not stop after %s", tc.name)
			}

			n.mu.Lock()
			_, still := n.loops[sessionID]
			n.mu.Unlock()
			if still {
				t.Fatalf("loop still registered after %s", tc.name)
			}

			// No further POSTs once the loop has exited.
			settled := log.count()
			time.Sleep(60 * time.Millisecond)
			if got := log.count(); got != settled {
				t.Fatalf("expected no posts after %s settled the loop: had %d, now %d", tc.name, settled, got)
			}
		})
	}
}

// TestDiscordTypingTerminalEventWithoutSessionIsIgnored covers the one branch a
// regression could silently break: an issue/autopilot task publishes a terminal
// event carrying no chat_session_id, and it must NOT clear an unrelated chat
// session's live refresh loop. Without this, a broadened chatSessionIDFromEvent
// (e.g. one that fell back to some other id) would tear down healthy loops.
func TestDiscordTypingTerminalEventWithoutSessionIsIgnored(t *testing.T) {
	srv, _ := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, 20*time.Millisecond, time.Hour)
	bus := events.New()
	n.Register(bus)

	sessionID := pgtype.UUID{Bytes: [16]byte{20}, Valid: true}
	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-noid"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	// Terminal events with no recoverable session id (non-chat tasks): empty
	// envelope, and a payload map without chat_session_id.
	bus.Publish(events.Event{Type: protocol.EventChatDone})
	bus.Publish(events.Event{Type: protocol.EventTaskFailed, Payload: map[string]any{"task_id": "x"}})
	bus.Publish(events.Event{Type: protocol.EventTaskCancelled})

	n.mu.Lock()
	_, still := n.loops[sessionID]
	n.mu.Unlock()
	if !still {
		t.Fatalf("a session-less terminal event cleared an unrelated loop")
	}
}

func TestDiscordTypingSafetyTimeoutStopsForgottenLoop(t *testing.T) {
	srv, _ := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, 5*time.Millisecond, 40*time.Millisecond)
	sessionID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-4"), sessionID)

	n.mu.Lock()
	loop := n.loops[sessionID]
	n.mu.Unlock()
	if loop == nil {
		t.Fatalf("expected a registered loop")
	}

	// OnSettled is deliberately never called: the safety timeout must stop
	// the loop on its own.
	select {
	case <-loop.done:
	case <-time.After(time.Second):
		t.Fatalf("safety timeout did not stop the loop")
	}

	n.mu.Lock()
	_, stillRegistered := n.loops[sessionID]
	n.mu.Unlock()
	if stillRegistered {
		t.Fatalf("expected loop to deregister itself after the safety timeout")
	}
}

func TestDiscordTypingConcurrentSessionsAreIndependent(t *testing.T) {
	srv, log := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, 10*time.Millisecond, time.Hour)
	sessionA := pgtype.UUID{Bytes: [16]byte{5}, Valid: true}
	sessionB := pgtype.UUID{Bytes: [16]byte{6}, Valid: true}

	cfg := newTestConfig(t)
	n.OnIngested(context.Background(), testInstallation(cfg), testInboundMessage("chan-a"), sessionA)
	n.OnIngested(context.Background(), testInstallation(cfg), testInboundMessage("chan-b"), sessionB)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionB) })

	if !waitFor(t, time.Second, func() bool { return log.count() >= 4 }) {
		t.Fatalf("expected both sessions to post, got %d", log.count())
	}

	n.mu.Lock()
	loopA := n.loops[sessionA]
	n.mu.Unlock()
	if loopA == nil {
		t.Fatalf("expected session A to have a registered loop")
	}

	n.OnSettled(context.Background(), sessionA)
	select {
	case <-loopA.done:
	case <-time.After(time.Second):
		t.Fatalf("session A loop did not exit after its own OnSettled")
	}

	n.mu.Lock()
	_, bStillRegistered := n.loops[sessionB]
	n.mu.Unlock()
	if !bStillRegistered {
		t.Fatalf("settling session A must not stop session B's loop")
	}

	beforeB := log.count()
	if !waitFor(t, time.Second, func() bool { return log.count() > beforeB }) {
		t.Fatalf("expected session B to keep posting after session A settled")
	}
}

func TestDiscordTypingFailingRequestIsSwallowed(t *testing.T) {
	srv, log := newTypingTestServer(t, http.StatusInternalServerError)
	n := newTestNotifier(srv.URL, time.Hour, time.Hour)
	sessionID := pgtype.UUID{Bytes: [16]byte{7}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-5"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	if got := log.count(); got != 1 {
		t.Fatalf("expected the POST to still be attempted once, got %d", got)
	}

	// A closed server (connection refused) must be swallowed too.
	closedSrv, _ := newTypingTestServer(t, http.StatusOK)
	closedSrv.Close()
	n2 := newTestNotifier(closedSrv.URL, time.Hour, time.Hour)
	sessionID2 := pgtype.UUID{Bytes: [16]byte{8}, Valid: true}

	// Must not panic and must return promptly.
	done := make(chan struct{})
	go func() {
		n2.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-6"), sessionID2)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("OnIngested against a closed server did not return")
	}
	t.Cleanup(func() { n2.OnSettled(context.Background(), sessionID2) })
}

func TestDiscordTypingNoGoroutineLeak(t *testing.T) {
	srv, _ := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, 5*time.Millisecond, time.Hour)

	runtime.GC()
	before := runtime.NumGoroutine()

	const sessions = 8
	var uuids [sessions]pgtype.UUID
	var loops [sessions]*discordTypingLoop
	cfg := newTestConfig(t)
	for i := 0; i < sessions; i++ {
		uuids[i] = pgtype.UUID{Bytes: [16]byte{byte(20 + i)}, Valid: true}
		n.OnIngested(context.Background(), testInstallation(cfg), testInboundMessage("chan-leak"), uuids[i])
		n.mu.Lock()
		loops[i] = n.loops[uuids[i]]
		n.mu.Unlock()
	}

	for i := 0; i < sessions; i++ {
		n.OnSettled(context.Background(), uuids[i])
	}

	for i := 0; i < sessions; i++ {
		select {
		case <-loops[i].done:
		case <-time.After(time.Second):
			t.Fatalf("session %d loop did not exit", i)
		}
	}

	// Every loop already proved it exited via loop.done above; this is a
	// belt-and-suspenders check that NumGoroutine settles back down too.
	// Idle keep-alive connections held by the shared http.Client's transport
	// close on their own schedule, so poll instead of sampling once.
	n.client.CloseIdleConnections()
	var after int
	settled := waitFor(t, time.Second, func() bool {
		runtime.GC()
		after = runtime.NumGoroutine()
		return after <= before+2 // +2 slack for unrelated background goroutines (GC, etc.)
	})
	if !settled {
		t.Fatalf("possible goroutine leak: before=%d after=%d", before, after)
	}
}

// TestDiscordTypingRestartReplacesStaleLoop covers the OnIngested-called-
// twice-for-the-same-session case: the second call must stop the first
// loop rather than leaving two loops posting for one session.
func TestDiscordTypingRestartReplacesStaleLoop(t *testing.T) {
	srv, log := newTypingTestServer(t, http.StatusNoContent)
	n := newTestNotifier(srv.URL, 10*time.Millisecond, time.Hour)
	sessionID := pgtype.UUID{Bytes: [16]byte{9}, Valid: true}
	cfg := newTestConfig(t)

	n.OnIngested(context.Background(), testInstallation(cfg), testInboundMessage("chan-7"), sessionID)
	n.mu.Lock()
	firstLoop := n.loops[sessionID]
	n.mu.Unlock()

	n.OnIngested(context.Background(), testInstallation(cfg), testInboundMessage("chan-7"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	select {
	case <-firstLoop.done:
	case <-time.After(time.Second):
		t.Fatalf("first loop was not stopped by the restart")
	}

	if !waitFor(t, time.Second, func() bool { return log.count() >= 2 }) {
		t.Fatalf("expected the new loop to keep posting, got %d", log.count())
	}
}

// TestDiscordTypingSuccessfulRefreshLogsPositiveSignal is the gap this task
// exists to close: before postTyping's Debug "refresh posted" line, a
// successful typing POST was completely silent — postTyping only ever
// logged failures — so a live-Gateway validation run had no way to tell
// "the refresh fired" from "nothing happened at all" apart from watching
// Discord's UI directly. This asserts the positive signal exists, is
// distinguishable from the failure line, never leaks the bot token, and
// carries "installation_id" — the same field name connect.go's resume/
// reconnect lines use — so the two log families can be joined on a live
// host without an out-of-band session-to-installation lookup.
func TestDiscordTypingSuccessfulRefreshLogsPositiveSignal(t *testing.T) {
	srv, _ := newTypingTestServer(t, http.StatusNoContent)
	logger, buf := newCapturingLogger(slog.LevelDebug)
	n := newTestNotifierWithLogger(srv.URL, time.Hour, time.Hour, logger)
	sessionID := pgtype.UUID{Bytes: [16]byte{30}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-success"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	got := buf.String()
	if !strings.Contains(got, "discord typing: refresh posted") {
		t.Fatalf("expected a \"refresh posted\" log line for a successful POST, got: %s", got)
	}
	if strings.Contains(got, "non-2xx") || strings.Contains(got, "failed") {
		t.Errorf("a successful POST must not also log a failure line: %s", got)
	}
	if strings.Contains(got, "bot-token-123") {
		t.Errorf("captured log contains the bot token, want it never logged: %s", got)
	}
	if !strings.Contains(got, "chan-success") {
		t.Errorf("expected the channel id in the refresh-posted line for correlation, got: %s", got)
	}
	wantInstallationID := util.UUIDToString(testInstallationUUID)
	if !strings.Contains(got, "installation_id="+wantInstallationID) {
		t.Errorf("expected installation_id=%s in the refresh-posted line (the join key with connect.go's resume/reconnect lines), got: %s", wantInstallationID, got)
	}
}

// TestDiscordTypingFailedRefreshDoesNotLogSuccessSignal is the complement:
// the two outcomes (successful refresh vs. failed refresh) must be
// distinguishable purely by which line appears, since both are otherwise
// silent to anything but the log.
func TestDiscordTypingFailedRefreshDoesNotLogSuccessSignal(t *testing.T) {
	srv, _ := newTypingTestServer(t, http.StatusInternalServerError)
	logger, buf := newCapturingLogger(slog.LevelDebug)
	n := newTestNotifierWithLogger(srv.URL, time.Hour, time.Hour, logger)
	sessionID := pgtype.UUID{Bytes: [16]byte{31}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-fail"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	got := buf.String()
	if !strings.Contains(got, "discord typing: non-2xx response") {
		t.Fatalf("expected the existing non-2xx failure line, got: %s", got)
	}
	if strings.Contains(got, "refresh posted") {
		t.Errorf("a failed POST must not also log the success signal: %s", got)
	}
}

// TestDiscordTypingRefreshPostedIsDebugNotWarnOrInfo guards the log-level
// choice itself: this line fires every discordTypingRefreshInterval for
// every in-flight run, so it must stay below the level a production
// deployment normally captures, unlike the existing failure lines (Warn).
func TestDiscordTypingRefreshPostedIsDebugNotWarnOrInfo(t *testing.T) {
	srv, _ := newTypingTestServer(t, http.StatusNoContent)
	logger, buf := newCapturingLogger(slog.LevelInfo) // production-typical level
	n := newTestNotifierWithLogger(srv.URL, time.Hour, time.Hour, logger)
	sessionID := pgtype.UUID{Bytes: [16]byte{32}, Valid: true}

	n.OnIngested(context.Background(), testInstallation(newTestConfig(t)), testInboundMessage("chan-quiet"), sessionID)
	t.Cleanup(func() { n.OnSettled(context.Background(), sessionID) })

	if got := buf.String(); strings.Contains(got, "refresh posted") {
		t.Errorf("refresh-posted line was emitted at Info level, want Debug (would flood production logs at 1/8s per in-flight run): %s", got)
	}
}
