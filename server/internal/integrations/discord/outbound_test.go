package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Canonical test layer for the Discord streaming outbound worker (Task
// Master task 7). No DB, no network: every server is httptest and every
// queries dependency is an in-memory fake.

// fakeOutboundQueries is the outboundQueries fake. Keyed by task id / bot
// key string so tests can wire up exactly the rows resolveTarget needs
// without a database.
type fakeOutboundQueries struct {
	mu           sync.Mutex
	deliveries   map[string]db.ChannelTaskDelivery
	installation db.ChannelInstallation
}

func newFakeOutboundQueries() *fakeOutboundQueries {
	return &fakeOutboundQueries{deliveries: make(map[string]db.ChannelTaskDelivery)}
}

func (f *fakeOutboundQueries) setDelivery(taskID pgtype.UUID, d db.ChannelTaskDelivery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries[util.UUIDToString(taskID)] = d
}

func (f *fakeOutboundQueries) GetChannelTaskDelivery(ctx context.Context, taskID pgtype.UUID) (db.ChannelTaskDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deliveries[util.UUIDToString(taskID)]
	if !ok {
		return db.ChannelTaskDelivery{}, pgx.ErrNoRows
	}
	return d, nil
}

func (f *fakeOutboundQueries) GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if arg.ChannelType != string(TypeDiscord) || f.installation.ID != arg.ID {
		return db.ChannelInstallation{}, pgx.ErrNoRows
	}
	return f.installation, nil
}

// recordingMetrics is a DeliveryMetrics fake that records every call so
// tests can assert the generated/delivered counters move as expected.
type recordingMetrics struct {
	mu        sync.Mutex
	generated []string
	delivered []string // "channelType:outcome"
}

func (m *recordingMetrics) RecordReplyGenerated(channelType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generated = append(m.generated, channelType)
}

func (m *recordingMetrics) RecordReplyDelivered(channelType, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivered = append(m.delivered, channelType+":"+outcome)
}

func (m *recordingMetrics) deliveredCount(want string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, d := range m.delivered {
		if d == want {
			n++
		}
	}
	return n
}

func (m *recordingMetrics) generatedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.generated)
}

// outboundRequest records one observed Discord API call.
type outboundRequest struct {
	method  string
	path    string
	payload discordMessagePayload
}

type outboundRequestLog struct {
	mu    sync.Mutex
	items []outboundRequest
}

func (l *outboundRequestLog) add(r outboundRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, r)
}

func (l *outboundRequestLog) snapshot() []outboundRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]outboundRequest, len(l.items))
	copy(out, l.items)
	return out
}

func (l *outboundRequestLog) countByMethod(method string) int {
	n := 0
	for _, r := range l.snapshot() {
		if r.method == method {
			n++
		}
	}
	return n
}

// newOutboundTestServer builds an httptest server that records every
// CreateMessage/EditMessage call and returns handler-controlled responses.
// respond is called once per request and returns the status code + body to
// write; it may be nil for a default 200 + a synthesized message id.
func newOutboundTestServer(t *testing.T, respond func(n int, req outboundRequest) (int, any)) (*httptest.Server, *outboundRequestLog) {
	t.Helper()
	log := &outboundRequestLog{}
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p discordMessagePayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		count := int(atomic.AddInt32(&n, 1))
		req := outboundRequest{method: r.Method, path: r.URL.Path, payload: p}
		log.add(req)

		status := http.StatusOK
		var body any = discordMessageResponse{ID: "msg-auto"}
		if respond != nil {
			status, body = respond(count, req)
		}
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func newOutboundTestInstallation(t *testing.T, instID pgtype.UUID) db.ChannelInstallation {
	t.Helper()
	return db.ChannelInstallation{
		ID:          instID,
		ChannelType: string(TypeDiscord),
		Status:      "active",
		Config:      newTestConfig(t),
	}
}

func newOutbound(t *testing.T, srvURL string, q outboundQueries, metrics DeliveryMetrics) *Outbound {
	t.Helper()
	o := NewOutbound(q, nil, srvURL, &http.Client{Timeout: 2 * time.Second}, metrics, nil)
	return o
}

func chatDoneEvent(taskID pgtype.UUID, sessionID pgtype.UUID, content string) protocol.ChatDonePayload {
	return protocol.ChatDonePayload{
		ChatSessionID: util.UUIDToString(sessionID),
		TaskID:        util.UUIDToString(taskID),
		Content:       content,
	}
}

// ---- streaming partials ----

func TestOutbound_StreamPartials_PlaceholderPostedOnceEditsThrottled(t *testing.T) {
	srv, log := newOutboundTestServer(t, nil)
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 1)
	taskID := dbidTestUUID(2)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-1",
	})

	o := newOutbound(t, srv.URL, q, nil)
	fixedNow := time.Now()
	o.now = func() time.Time { return fixedNow }

	ev := func(content string) events.Event {
		return events.Event{TaskID: util.UUIDToString(taskID), Payload: protocol.TaskMessagePayload{
			TaskID: util.UUIDToString(taskID), Type: "text", Content: content,
		}}
	}

	// First frame: posts the placeholder.
	o.handleTaskMessage(ev("Hello"))
	// Rapid-fire frames within the throttle window must NOT trigger a PATCH.
	for i := 0; i < 5; i++ {
		o.handleTaskMessage(ev(" more"))
	}
	if got := log.countByMethod(http.MethodPost); got != 1 {
		t.Fatalf("expected exactly 1 placeholder POST, got %d: %#v", got, log.snapshot())
	}
	if got := log.countByMethod(http.MethodPatch); got != 0 {
		t.Fatalf("expected 0 PATCHes inside the throttle window, got %d", got)
	}

	// Advance the clock past the throttle interval: the next frame should edit.
	fixedNow = fixedNow.Add(editThrottleInterval + time.Millisecond)
	o.handleTaskMessage(ev(" final"))
	if got := log.countByMethod(http.MethodPatch); got != 1 {
		t.Fatalf("expected exactly 1 PATCH after the throttle window elapsed, got %d", got)
	}

	// Six more frames spread across many throttle windows would, if
	// unthrottled, produce six PATCHes; assert the throttle still holds far
	// fewer edits than content updates over a longer synthetic run.
	for i := 0; i < 20; i++ {
		o.handleTaskMessage(ev(" x"))
	}
	if got := log.countByMethod(http.MethodPatch); got != 1 {
		t.Fatalf("burst of frames inside one throttle window produced %d PATCHes, want 1", got)
	}
}

// ---- finalize (EventChatDone) ----

func TestOutbound_Finalize_SingleChunk(t *testing.T) {
	srv, log := newOutboundTestServer(t, nil)
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 3)
	taskID := dbidTestUUID(4)
	sessionID := dbidTestUUID(5)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-2",
	})

	metrics := &recordingMetrics{}
	o := newOutbound(t, srv.URL, q, metrics)
	o.Start(context.Background())

	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, "the final answer"),
	})
	if !o.WaitWithTimeout(2 * time.Second) {
		t.Fatal("finalize did not complete in time")
	}

	posted := log.snapshot()
	if len(posted) != 1 {
		t.Fatalf("expected exactly 1 posted message, got %d: %#v", len(posted), posted)
	}
	if posted[0].payload.Content != "the final answer" {
		t.Fatalf("posted content = %q, want %q", posted[0].payload.Content, "the final answer")
	}
	if metrics.generatedCount() != 1 {
		t.Fatalf("generated count = %d, want 1", metrics.generatedCount())
	}
	if got := metrics.deliveredCount("discord:delivered"); got != 1 {
		t.Fatalf("delivered{delivered} count = %d, want 1", got)
	}
}

func TestOutbound_Finalize_StreamedPlaceholderIsEdited(t *testing.T) {
	srv, log := newOutboundTestServer(t, nil)
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 6)
	taskID := dbidTestUUID(7)
	sessionID := dbidTestUUID(8)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-3",
	})

	o := newOutbound(t, srv.URL, q, nil)
	o.Start(context.Background())

	o.handleTaskMessage(events.Event{TaskID: util.UUIDToString(taskID), Payload: protocol.TaskMessagePayload{
		TaskID: util.UUIDToString(taskID), Type: "text", Content: "partial",
	}})
	if got := log.countByMethod(http.MethodPost); got != 1 {
		t.Fatalf("expected the placeholder POST, got %d", got)
	}

	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, "final content"),
	})
	if !o.WaitWithTimeout(2 * time.Second) {
		t.Fatal("finalize did not complete in time")
	}

	if got := log.countByMethod(http.MethodPatch); got != 1 {
		t.Fatalf("expected the placeholder to be PATCHed exactly once on finalize, got %d", got)
	}
	if got := log.countByMethod(http.MethodPost); got != 1 {
		t.Fatalf("finalize must not POST a new message when the placeholder edit succeeds; got %d POSTs", got)
	}
}

func TestOutbound_Finalize_MultiChunkPostsAdditionalMessages(t *testing.T) {
	srv, log := newOutboundTestServer(t, nil)
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 9)
	taskID := dbidTestUUID(10)
	sessionID := dbidTestUUID(11)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-4",
	})

	o := newOutbound(t, srv.URL, q, nil)
	o.Start(context.Background())

	longContent := strings.Repeat("word ", 1000) // forces multiple chunks
	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, longContent),
	})
	if !o.WaitWithTimeout(2 * time.Second) {
		t.Fatal("finalize did not complete in time")
	}

	posted := log.snapshot()
	if len(posted) < 2 {
		t.Fatalf("expected multiple chunks to be posted for long content, got %d", len(posted))
	}
	for _, r := range posted {
		if r.method != http.MethodPost {
			t.Fatalf("expected every chunk of a no-placeholder finalize to be a POST, got %s", r.method)
		}
	}
}

// TestOutbound_Finalize_NoPriorStreamState is the anti-#7215 guard: this
// process never called handleTaskMessage (so o.streams is empty), and the
// Outbound holds nothing Gateway-shaped at all (no discordChannel, no
// ResumeCache, no Reconnector — this type does not even import them).
// EventChatDone alone must still deliver, exactly as it would on a replica
// that never held this installation's Gateway connection.
func TestOutbound_Finalize_NoPriorStreamState(t *testing.T) {
	srv, log := newOutboundTestServer(t, nil)
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 12)
	taskID := dbidTestUUID(13)
	sessionID := dbidTestUUID(14)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-5",
	})

	o := newOutbound(t, srv.URL, q, nil)
	// Deliberately do NOT call o.Start: this is exactly the shape a stateless
	// REST worker on any replica needs to support (no lifecycle ceremony
	// tied to a Gateway connection this process never opened).

	if len(o.streams) != 0 {
		t.Fatalf("test setup invariant violated: streams must start empty")
	}
	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, "delivered with no gateway state"),
	})
	if !o.WaitWithTimeout(2 * time.Second) {
		t.Fatal("finalize did not complete in time")
	}

	posted := log.snapshot()
	if len(posted) != 1 {
		t.Fatalf("expected exactly 1 posted message with no prior stream state, got %d: %#v", len(posted), posted)
	}
	if posted[0].payload.Content != "delivered with no gateway state" {
		t.Fatalf("posted content = %q", posted[0].payload.Content)
	}
}

// TestOutbound_Finalize_429DuringEditIsRetried covers the placeholder-edit
// 429 path: editWithRetry (sender.go) must retry and still land the final
// content, not fall back to posting a duplicate fresh message.
func TestOutbound_Finalize_429DuringEditIsRetried(t *testing.T) {
	var patchAttempts int32
	srv, log := newOutboundTestServer(t, func(n int, req outboundRequest) (int, any) {
		if req.method == http.MethodPatch {
			attempt := atomic.AddInt32(&patchAttempts, 1)
			if attempt == 1 {
				return http.StatusTooManyRequests, map[string]any{"retry_after": 0.001}
			}
		}
		return http.StatusOK, discordMessageResponse{ID: "msg-final"}
	})
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 15)
	taskID := dbidTestUUID(16)
	sessionID := dbidTestUUID(17)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-6",
	})

	o := newOutbound(t, srv.URL, q, nil)
	o.Start(context.Background())

	o.handleTaskMessage(events.Event{TaskID: util.UUIDToString(taskID), Payload: protocol.TaskMessagePayload{
		TaskID: util.UUIDToString(taskID), Type: "text", Content: "partial",
	}})
	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, "final after 429"),
	})
	if !o.WaitWithTimeout(2 * time.Second) {
		t.Fatal("finalize did not complete in time")
	}

	if got := atomic.LoadInt32(&patchAttempts); got != 2 {
		t.Fatalf("expected exactly 2 PATCH attempts (1 rate-limited + 1 success), got %d", got)
	}
	if got := log.countByMethod(http.MethodPost); got != 1 {
		t.Fatalf("expected exactly 1 POST (the placeholder); the final content must land via the retried PATCH, got %d POSTs", got)
	}
}

func TestOutbound_Finalize_ResolveFailureRecordsFailedMetric(t *testing.T) {
	q := newFakeOutboundQueries()
	q.installation = db.ChannelInstallation{} // no matching installation -> GetChannelInstallation errors
	instID := testInstallationID(t, 18)
	taskID := dbidTestUUID(19)
	sessionID := dbidTestUUID(20)
	// Delivery references an installation id the fake never registers.
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-7",
	})

	metrics := &recordingMetrics{}
	o := newOutbound(t, "http://127.0.0.1:0", q, metrics)
	o.Start(context.Background())

	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, "unreachable installation"),
	})
	if !o.WaitWithTimeout(2 * time.Second) {
		t.Fatal("finalize did not complete in time")
	}
	if got := metrics.deliveredCount("discord:failed"); got != 1 {
		t.Fatalf("delivered{failed} count = %d, want 1", got)
	}
}

func TestOutbound_Finalize_FatalErrorRecordsFailedMetric(t *testing.T) {
	srv, _ := newOutboundTestServer(t, func(n int, req outboundRequest) (int, any) {
		return http.StatusBadRequest, map[string]any{"message": "bad request"}
	})
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 21)
	taskID := dbidTestUUID(22)
	sessionID := dbidTestUUID(23)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-8",
	})

	metrics := &recordingMetrics{}
	o := newOutbound(t, srv.URL, q, metrics)
	o.Start(context.Background())

	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, "will be rejected"),
	})
	if !o.WaitWithTimeout(2 * time.Second) {
		t.Fatal("finalize did not complete in time")
	}
	if got := metrics.deliveredCount("discord:failed"); got != 1 {
		t.Fatalf("delivered{failed} count = %d, want 1", got)
	}
	if got := metrics.deliveredCount("discord:delivered"); got != 0 {
		t.Fatalf("delivered{delivered} count = %d, want 0", got)
	}
}

func TestOutbound_ChatDone_EmptyContentSkipsDelivery(t *testing.T) {
	srv, log := newOutboundTestServer(t, nil)
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 24)
	taskID := dbidTestUUID(25)
	sessionID := dbidTestUUID(26)
	q.installation = newOutboundTestInstallation(t, instID)
	q.setDelivery(taskID, db.ChannelTaskDelivery{
		TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
		ChannelChatID: "chan-9",
	})

	metrics := &recordingMetrics{}
	o := newOutbound(t, srv.URL, q, metrics)
	o.Start(context.Background())

	o.handleChatDone(events.Event{
		TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
		Payload: chatDoneEvent(taskID, sessionID, ""),
	})
	if !o.WaitWithTimeout(time.Second) {
		t.Fatal("wait timed out")
	}
	if len(log.snapshot()) != 0 {
		t.Fatalf("expected no HTTP calls for empty content, got %d", len(log.snapshot()))
	}
	if metrics.generatedCount() != 0 {
		t.Fatalf("generated count = %d, want 0 for empty content", metrics.generatedCount())
	}
}

// ---- goroutine leak guard ----

func TestOutbound_NoGoroutineLeak(t *testing.T) {
	srv, _ := newOutboundTestServer(t, nil)
	q := newFakeOutboundQueries()
	instID := testInstallationID(t, 27)
	q.installation = newOutboundTestInstallation(t, instID)

	o := newOutbound(t, srv.URL, q, nil)
	o.Start(context.Background())

	runtime.GC()
	before := runtime.NumGoroutine()

	const n = 20
	for i := 0; i < n; i++ {
		taskID := dbidTestUUID(byte(100 + i))
		sessionID := dbidTestUUID(byte(150 + i))
		q.setDelivery(taskID, db.ChannelTaskDelivery{
			TaskID: taskID, InstallationID: instID, ChannelType: string(TypeDiscord),
			ChannelChatID: "chan-leak",
		})
		o.handleChatDone(events.Event{
			TaskID: util.UUIDToString(taskID), ChatSessionID: util.UUIDToString(sessionID),
			Payload: chatDoneEvent(taskID, sessionID, "reply"),
		})
	}
	if !o.WaitWithTimeout(3 * time.Second) {
		t.Fatal("finalizes did not complete in time")
	}

	srv.CloseClientConnections()
	var after int
	settled := waitFor(t, time.Second, func() bool {
		runtime.GC()
		after = runtime.NumGoroutine()
		return after <= before+2
	})
	if !settled {
		t.Fatalf("possible goroutine leak: before=%d after=%d", before, after)
	}
}

// ---- shared test helpers ----

func dbidTestUUID(b byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{b}, Valid: true}
}
