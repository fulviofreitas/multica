package discord

// resume_test.go exercises the resume cache and RESUME frame send
// (resume.go, subtask 2.4). Cache tests are pure (no network); the RESUME
// frame-content and sequence-priming tests reuse the in-process fake
// Gateway pattern from gateway_test.go.
//
// resume.go itself holds no logger, so the log-observability tests for a
// successful RESUME (a RESUMED dispatch) and a rejected RESUME falling back
// to fresh IDENTIFY live in connect_test.go instead — see
// TestConnect_ResumeSucceeds_LogsSessionResumed and
// TestConnect_ResumeRejected_LogsDistinctFromFreshSessionInvalidated,
// connect.go being the only place in this package that actually logs a
// reconnect outcome.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgtype"
)

// testInstallationID builds a distinct, valid pgtype.UUID for test fixtures.
func testInstallationID(t *testing.T, b byte) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	id.Bytes[0] = b
	id.Valid = true
	return id
}

// fakeClock lets tests advance time deterministically instead of racing
// wall-clock time.Now(), per resume.go's requirement that freshness checks
// only ever consult the injected clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---- Store / Load ----

func TestResumeCache_StoreThenLoad_ReturnsEntry(t *testing.T) {
	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := NewResumeCache(ResumeCacheConfig{Now: clock.Now})
	id := testInstallationID(t, 1)

	stored := cache.Store(id, "sess-1", "wss://gateway.discord.gg/resume", 42)

	got, ok := cache.Load(id)
	if !ok {
		t.Fatal("Load: ok = false, want true after Store")
	}
	if got != stored {
		t.Error("Load returned a different *ResumeEntry than Store produced, want the same pointer")
	}
	if got.SessionID != "sess-1" || got.ResumeGatewayURL != "wss://gateway.discord.gg/resume" || got.Seq != 42 {
		t.Errorf("Load = %+v, unexpected fields", got)
	}
}

func TestResumeCache_Load_UnknownInstallationIsNotFound(t *testing.T) {
	clock := newFakeClock(time.Now())
	cache := NewResumeCache(ResumeCacheConfig{Now: clock.Now})

	_, ok := cache.Load(testInstallationID(t, 99))
	if ok {
		t.Error("Load: ok = true for an installation that was never stored, want false")
	}
}

// TestResumeCache_StaleEntry_TreatedAsAbsent is the freshness-window
// contract: an entry older than resumeWindow must be reported absent so the
// caller falls back to a fresh IDENTIFY instead of attempting a RESUME
// Discord will refuse anyway.
func TestResumeCache_StaleEntry_TreatedAsAbsent(t *testing.T) {
	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := NewResumeCache(ResumeCacheConfig{Now: clock.Now})
	id := testInstallationID(t, 2)

	cache.Store(id, "sess-2", "wss://gateway.discord.gg/resume", 7)

	// Still within the window: present.
	clock.Advance(resumeWindow - time.Second)
	if _, ok := cache.Load(id); !ok {
		t.Fatal("Load: ok = false just under resumeWindow, want true")
	}

	// Past the window: absent.
	clock.Advance(2 * time.Second)
	if _, ok := cache.Load(id); ok {
		t.Error("Load: ok = true for an entry older than resumeWindow, want false (caller should IDENTIFY, not RESUME)")
	}
}

// TestResumeCache_Clear_DoesNotEvictSuccessorEntry is the wecom bug class
// this cache must not reproduce (see senders_registry.go's clear and this
// file's doc comment): an old/draining connection's deferred cleanup must
// never be able to wipe out the entry a successor connection has already
// stored for the same installation.
func TestResumeCache_Clear_DoesNotEvictSuccessorEntry(t *testing.T) {
	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := NewResumeCache(ResumeCacheConfig{Now: clock.Now})
	id := testInstallationID(t, 3)

	// Old connection stores its session.
	oldEntry := cache.Store(id, "sess-old", "wss://gateway.discord.gg/resume", 1)

	// A successor connection reconnects and stores its own, newer session
	// before the old connection's shutdown path gets around to clearing.
	newEntry := cache.Store(id, "sess-new", "wss://gateway.discord.gg/resume", 99)
	if newEntry == oldEntry {
		t.Fatal("test setup: Store returned the same pointer for two calls, fixture is broken")
	}

	// The old connection's deferred cleanup now runs, clearing what IT
	// stored. It must be a no-op: the entry currently in the cache belongs
	// to the successor, not to oldEntry.
	cache.Clear(id, oldEntry)

	got, ok := cache.Load(id)
	if !ok {
		t.Fatal("Load: ok = false after an old connection's Clear raced a successor's Store, want the successor's entry to survive")
	}
	if got != newEntry {
		t.Errorf("Load = %+v, want the successor's entry (sess-new) to have survived the old connection's Clear", got)
	}
	if got.SessionID != "sess-new" {
		t.Errorf("SessionID = %q, want %q (successor-eviction guard failed)", got.SessionID, "sess-new")
	}
}

// TestResumeCache_Clear_RemovesItsOwnEntry is the complementary case: when
// no successor has raced ahead, Clear must actually remove the entry it owns
// (the guard must not make Clear a permanent no-op).
func TestResumeCache_Clear_RemovesItsOwnEntry(t *testing.T) {
	clock := newFakeClock(time.Now())
	cache := NewResumeCache(ResumeCacheConfig{Now: clock.Now})
	id := testInstallationID(t, 4)

	entry := cache.Store(id, "sess-4", "wss://gateway.discord.gg/resume", 1)
	cache.Clear(id, entry)

	if _, ok := cache.Load(id); ok {
		t.Error("Load: ok = true after Clear removed the only stored entry, want false")
	}
}

// ---- concurrency ----

// TestResumeCache_ConcurrentStoreLoadClear exercises Store/Load/Clear from
// many goroutines across a handful of installation ids, so `go test -race`
// can catch any unguarded access to the underlying map.
func TestResumeCache_ConcurrentStoreLoadClear(t *testing.T) {
	clock := newFakeClock(time.Now())
	cache := NewResumeCache(ResumeCacheConfig{Now: clock.Now})

	ids := make([]pgtype.UUID, 4)
	for i := range ids {
		ids[i] = testInstallationID(t, byte(i+1))
	}

	var wg sync.WaitGroup
	const workersPerID = 8
	const iterations = 200
	for _, id := range ids {
		id := id
		for w := 0; w < workersPerID; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					entry := cache.Store(id, "sess", "wss://gateway.discord.gg/resume", int64(i))
					cache.Load(id)
					// Racing Clear against other goroutines' fresher Store
					// calls is exactly the scenario the identity guard
					// exists for; it must never panic or corrupt the map.
					cache.Clear(id, entry)
				}
			}()
		}
	}
	wg.Wait()
}

// ---- RESUME frame over the fake Gateway ----

func TestGatewayConn_Resume_SendsExpectedFrame(t *testing.T) {
	const token = "test-bot-token"

	type receivedResume struct {
		Op int `json:"op"`
		D  struct {
			Token     string `json:"token"`
			SessionID string `json:"session_id"`
			Seq       int64  `json:"seq"`
		} `json:"d"`
	}

	resumeCh := make(chan receivedResume, 1)

	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, (5 * time.Second).Milliseconds())
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var got receivedResume
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("server: decode resume frame: %v", err)
			return
		}
		resumeCh <- got
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL)})
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}
	defer gc.Close()

	entry := &ResumeEntry{SessionID: "sess-resume-1", ResumeGatewayURL: wsURL(srv.URL), Seq: 55}
	if err := gc.Resume(token, entry); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	select {
	case got := <-resumeCh:
		if got.Op != opResume {
			t.Errorf("op = %d, want %d", got.Op, opResume)
		}
		if got.D.Token != token {
			t.Errorf("token = %q, want %q", got.D.Token, token)
		}
		if got.D.SessionID != entry.SessionID {
			t.Errorf("session_id = %q, want %q", got.D.SessionID, entry.SessionID)
		}
		if got.D.Seq != entry.Seq {
			t.Errorf("seq = %d, want %d", got.D.Seq, entry.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the RESUME frame")
	}
}

// TestGatewayConn_Resume_PrimesSequenceBeforeReturning is the direct,
// synchronous check for requirement 6: Sequence() must already report the
// cached value the instant Resume returns, before Run (and therefore the
// heartbeat loop) has even started.
func TestGatewayConn_Resume_PrimesSequenceBeforeReturning(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, (5 * time.Second).Milliseconds())
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL)})
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}
	defer gc.Close()

	if _, ok := gc.Sequence(); ok {
		t.Fatal("Sequence() already has a value before Resume, fixture is broken")
	}

	entry := &ResumeEntry{SessionID: "sess-resume-2", ResumeGatewayURL: wsURL(srv.URL), Seq: 123}
	if err := gc.Resume("test-token", entry); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	seq, ok := gc.Sequence()
	if !ok || seq != entry.Seq {
		t.Fatalf("Sequence() = (%d, %v) immediately after Resume, want (%d, true)", seq, ok, entry.Seq)
	}
}

// TestRun_FirstHeartbeatAfterResumeCarriesPrimedSequence is the end-to-end
// version of requirement 6: it drives Resume and then Run over the fake
// Gateway and asserts the very FIRST heartbeat Run sends already carries the
// cached sequence number, proving priming happened before the heartbeat
// loop could send a stale/nil "s" — not merely before Run started, but
// before any post-resume dispatch could have primed it instead.
func TestRun_FirstHeartbeatAfterResumeCarriesPrimedSequence(t *testing.T) {
	const interval = 40 * time.Millisecond
	const cachedSeq = int64(77)
	heartbeats := make(chan heartbeatFrame, 4)

	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, interval.Milliseconds())

		// Wait for the client's RESUME before doing anything else — no
		// RESUMED dispatch or replay is sent, so the ONLY way the first
		// heartbeat can carry cachedSeq is Resume having primed it.
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var resume struct {
			Op int `json:"op"`
		}
		if err := json.Unmarshal(raw, &resume); err != nil || resume.Op != opResume {
			t.Errorf("server: first client frame was not RESUME: %s", raw)
			return
		}

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var hb heartbeatFrame
			if json.Unmarshal(raw, &hb) != nil || hb.Op != opHeartbeat {
				continue
			}
			select {
			case heartbeats <- hb:
			default:
			}
			ack, _ := json.Marshal(gatewayFrame{Op: opHeartbeatACK})
			if err := conn.WriteMessage(websocket.TextMessage, ack); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL), JitterFunc: noJitter})
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}

	entry := &ResumeEntry{SessionID: "sess-resume-3", ResumeGatewayURL: wsURL(srv.URL), Seq: cachedSeq}
	if err := gc.Resume("test-token", entry); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(ctx, nil) }()

	select {
	case hb := <-heartbeats:
		if hb.D == nil {
			t.Fatal("first post-resume heartbeat d = null, want the primed cached sequence")
		}
		if *hb.D != cachedSeq {
			t.Errorf("first post-resume heartbeat d = %d, want %d (primed from the resume cache)", *hb.D, cachedSeq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first post-resume heartbeat")
	}

	cancel()
	select {
	case runErr := <-runErrCh:
		if runErr != nil {
			t.Errorf("Run() = %v, want nil after ctx cancellation", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}
