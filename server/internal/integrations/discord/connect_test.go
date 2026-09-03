package discord

// connect_test.go exercises discordChannel.connect (Task Master subtask
// 2.6) against the same in-process fake Gateway pattern as gateway_test.go:
// an httptest server upgraded with gorilla/websocket, scripted per test. No
// real network access to Discord happens anywhere in this file.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestChannel builds a discordChannel wired the way newDiscordFactory
// wires one, but with its own private ResumeCache/Reconnector (tests must
// never share these across cases — a shared IdentifyLimiter would leak
// spacing state between unrelated test cases) and gatewayURL pointed at the
// fake Gateway under test.
func newTestChannel(t *testing.T, gatewayURL string) *discordChannel {
	t.Helper()
	return &discordChannel{
		botToken:       "test-token",
		logger:         slog.Default(),
		resumeCache:    NewResumeCache(ResumeCacheConfig{}),
		reconnector:    NewReconnector(nil),
		gatewayURL:     gatewayURL,
		installationID: testInstallationID(t, 1),
	}
}

// readFrame reads and decodes one Gateway frame the client sent.
func readFrame(t *testing.T, conn *websocket.Conn) gatewayFrame {
	t.Helper()
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var frame gatewayFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return frame
}

// readOpFrame reads frames until one with the given opcode arrives (skipping
// heartbeats, which can interleave with whatever the test is waiting for),
// or fails the test after a bounded number of frames.
func readOpFrame(t *testing.T, conn *websocket.Conn, op int) gatewayFrame {
	t.Helper()
	for i := 0; i < 20; i++ {
		f := readFrame(t, conn)
		if f.Op == op {
			return f
		}
	}
	t.Fatalf("did not observe a frame with op %d within 20 frames", op)
	return gatewayFrame{}
}

func sendDispatch(t *testing.T, conn *websocket.Conn, seq int64, eventName string, payload any) {
	t.Helper()
	d, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal dispatch payload: %v", err)
	}
	frame, err := json.Marshal(gatewayFrame{Op: opDispatch, T: eventName, D: d, S: &seq})
	if err != nil {
		t.Fatalf("marshal dispatch frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("send dispatch: %v", err)
	}
}

// answerHeartbeats drains and ACKs every heartbeat until the connection
// closes, so Run's heartbeat loop never sees a missed ACK during a test that
// intentionally keeps the connection open past the events it's waiting for.
func answerHeartbeats(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var hb heartbeatFrame
		if json.Unmarshal(raw, &hb) != nil || hb.Op != opHeartbeat {
			continue
		}
		ack, _ := json.Marshal(gatewayFrame{Op: opHeartbeatACK})
		if err := conn.WriteMessage(websocket.TextMessage, ack); err != nil {
			return
		}
	}
}

func readyPayload(sessionID, resumeURL string) map[string]any {
	return map[string]any{
		"session_id":         sessionID,
		"resume_gateway_url": resumeURL,
		"user":               map[string]any{"id": "999", "username": "MulticaBot"},
	}
}

func messageCreatePayload(id, channelID, content string) map[string]any {
	return map[string]any{
		"id":         id,
		"channel_id": channelID,
		"content":    content,
		"author":     map[string]any{"id": "42", "username": "someone", "bot": false},
	}
}

// ---- happy path ----

func TestConnect_HappyPath_ReadyMessageThenCtxCancelReturnsNil(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 5000)
		readOpFrame(t, conn, opIdentify)
		sendDispatch(t, conn, 1, "READY", readyPayload("sess-happy", "wss://resume.example/gateway"))
		sendDispatch(t, conn, 2, "MESSAGE_CREATE", messageCreatePayload("msg-1", "chan-1", "hello"))
		answerHeartbeats(conn)
	})

	c := newTestChannel(t, wsURL(srv.URL))
	msgCh := make(chan MessageCreateEvent, 1)
	c.onMessageCreate = func(_ context.Context, evt MessageCreateEvent) {
		msgCh <- evt
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.connect(ctx) }()

	select {
	case evt := <-msgCh:
		if evt.Content != "hello" {
			t.Errorf("MessageCreate.Content = %q, want %q", evt.Content, "hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MESSAGE_CREATE to reach onMessageCreate")
	}

	entry, ok := c.resumeCache.Load(c.installationID)
	if !ok || entry.SessionID != "sess-happy" {
		t.Fatalf("resumeCache.Load = (%+v, %v), want the stored READY session", entry, ok)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("connect() = %v, want nil after ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return after ctx cancellation")
	}
}

// ---- the review finding: ctx cancelled during the HELLO wait ----

// TestConnect_CtxCancelledDuringHelloWait_ReturnsNil is the review-flagged
// case: DialGateway's HELLO read (readHello in gateway.go) returns
// ctx.Err() directly (a plain context.Canceled), NOT wrapped in
// *GatewayError, when ctx is cancelled while still waiting for HELLO.
// connect must translate that into nil, not surface it as a failed attempt.
func TestConnect_CtxCancelledDuringHelloWait_ReturnsNil(t *testing.T) {
	serverStuck := make(chan struct{})
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		// Deliberately never send HELLO.
		<-serverStuck
	})
	t.Cleanup(func() { close(serverStuck) })

	c := newTestChannel(t, wsURL(srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.connect(ctx) }()

	// Give connect a moment to actually be parked in DialGateway's HELLO
	// read before cancelling.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("connect() = %v, want nil (ctx cancelled mid-HELLO must not look like a failed attempt)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return within 2s of ctx cancellation during HELLO wait")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("connect took %v to return after ctx cancellation, want well under 2s", elapsed)
	}
}

// ---- ctx cancelled while blocked on the read loop ----

func TestConnect_CtxCancelledDuringReadLoop_ReturnsNilPromptly(t *testing.T) {
	serverStuck := make(chan struct{})
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, (10 * time.Second).Milliseconds())
		readOpFrame(t, conn, opIdentify)
		<-serverStuck // never send READY; block the connection open
	})
	t.Cleanup(func() { close(serverStuck) })

	c := newTestChannel(t, wsURL(srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.connect(ctx) }()

	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("connect() = %v, want nil on ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return within 2s of ctx cancellation during the read loop")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("connect took %v to return after ctx cancellation, want well under 2s", elapsed)
	}
}

// ---- reconnect policy wiring ----

func TestConnect_ResumableDrop_ReturnsErrorAndKeepsCache(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 5000)
		readOpFrame(t, conn, opIdentify)
		sendDispatch(t, conn, 1, "READY", readyPayload("sess-resumable", "wss://resume.example/gateway"))
		time.Sleep(30 * time.Millisecond)
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	})

	c := newTestChannel(t, wsURL(srv.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.connect(ctx)
	if err == nil {
		t.Fatal("connect() = nil, want an error (server_closed classifies ActionResume: retry)")
	}
	if _, ok := c.resumeCache.Load(c.installationID); !ok {
		t.Error("resumeCache entry was cleared, want it kept for an ActionResume outcome")
	}
}

func TestConnect_FreshIdentifyDecision_ClearsCache(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 5000)
		readOpFrame(t, conn, opIdentify)
		sendDispatch(t, conn, 1, "READY", readyPayload("sess-stale", "wss://resume.example/gateway"))
		time.Sleep(30 * time.Millisecond)
		// opInvalidSession with resumable=false classifies ActionFreshIdentify.
		d, _ := json.Marshal(false)
		frame, _ := json.Marshal(gatewayFrame{Op: opInvalidSession, D: d})
		_ = conn.WriteMessage(websocket.TextMessage, frame)
	})

	c := newTestChannel(t, wsURL(srv.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.connect(ctx)
	if err == nil {
		t.Fatal("connect() = nil, want an error (invalid_session, not resumable)")
	}
	if _, ok := c.resumeCache.Load(c.installationID); ok {
		t.Error("resumeCache entry was kept, want it cleared for an ActionFreshIdentify outcome")
	}
}

func TestConnect_FatalCloseCode_ReturnsErrorWithoutInternalRetry(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 5000)
		readOpFrame(t, conn, opIdentify)
		time.Sleep(20 * time.Millisecond)
		// 4014: disallowed intents — FATAL, per reconnect.go's discordCloseCodes.
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(4014, "disallowed intents"))
	})

	c := newTestChannel(t, wsURL(srv.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := c.connect(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("connect() = nil, want an error for a fatal close code")
	}
	if elapsed > 2*time.Second {
		t.Errorf("connect took %v to return on a fatal close code, want well under 2s (no internal retry loop)", elapsed)
	}
}

// ---- contract requirement 4: repeated Connect on a fresh ctx ----

func TestConnect_CanBeCalledAgainOnFreshCtxAfterReturning(t *testing.T) {
	srv1 := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 5000)
		readOpFrame(t, conn, opIdentify)
		time.Sleep(20 * time.Millisecond)
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	})

	c := newTestChannel(t, wsURL(srv1.URL))
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	if err := c.connect(ctx1); err == nil {
		t.Fatal("first connect() = nil, want an error from the scripted server close")
	}

	srv2 := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 5000)
		readOpFrame(t, conn, opIdentify)
		sendDispatch(t, conn, 1, "READY", readyPayload("sess-second", "wss://resume.example/gateway"))
		answerHeartbeats(conn)
	})
	c.gatewayURL = wsURL(srv2.URL)

	ctx2, cancel2 := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.connect(ctx2) }()

	// Give the second attempt time to reach READY before cancelling.
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := c.resumeCache.Load(c.installationID); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second connect() never stored a READY session")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel2()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("second connect() = %v, want nil after ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second connect did not return after ctx cancellation")
	}
}

// ---- resume vs identify dial selection ----

func TestConnect_WithCachedEntry_DialsResumeURLAndSendsOpResume(t *testing.T) {
	resumeSeen := make(chan gatewayFrame, 1)
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 5000)
		frame := readOpFrame(t, conn, opResume)
		resumeSeen <- frame
		answerHeartbeats(conn)
	})

	// gatewayURL deliberately points somewhere unreachable/unused: a
	// resumable entry must make connect dial ResumeGatewayURL instead.
	c := newTestChannel(t, "ws://127.0.0.1:0")
	c.resumeCache.Store(c.installationID, "sess-cached", wsURL(srv.URL), 42)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.connect(ctx) }()

	select {
	case frame := <-resumeSeen:
		if frame.Op != opResume {
			t.Errorf("client sent op %d, want opResume (%d)", frame.Op, opResume)
		}
		var d resumeData
		if err := json.Unmarshal(frame.D, &d); err != nil {
			t.Fatalf("decode resume payload: %v", err)
		}
		if d.SessionID != "sess-cached" {
			t.Errorf("resume session_id = %q, want %q", d.SessionID, "sess-cached")
		}
		if d.Seq != 42 {
			t.Errorf("resume seq = %d, want 42", d.Seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the client to send RESUME (op 6)")
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return after ctx cancellation")
	}
}
