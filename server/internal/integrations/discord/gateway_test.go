package discord

// gateway_test.go exercises the Gateway transport layer (gateway.go)
// against an in-process fake Gateway: an httptest server whose handler
// upgrades every connection with gorilla/websocket's Upgrader and then runs
// a per-test script over the resulting *websocket.Conn. No real network
// access to Discord happens anywhere in this file.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var testUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// newFakeGateway starts an httptest server that upgrades every incoming
// connection and hands it to handle on its own goroutine. handle owns the
// connection's entire lifetime, including closing it.
func newFakeGateway(t *testing.T, handle func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go handle(conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wsURL rewrites an httptest server's http(s):// base URL to ws(s)://.
func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func sendHello(t *testing.T, conn *websocket.Conn, heartbeatIntervalMS int64) {
	t.Helper()
	d, err := json.Marshal(helloData{HeartbeatInterval: heartbeatIntervalMS})
	if err != nil {
		t.Fatalf("marshal hello data: %v", err)
	}
	frame, err := json.Marshal(gatewayFrame{Op: opHello, D: d})
	if err != nil {
		t.Fatalf("marshal hello frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("send hello: %v", err)
	}
}

func noJitter() float64 { return 0 }

// ---- HELLO ----

func TestDialGateway_ParsesHelloHeartbeatInterval(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, 42)
		// Keep the connection open so DialGateway's read doesn't race a
		// server-side close; the test closes via httptest.Server cleanup.
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

	if got, want := gc.HeartbeatInterval(), 42*time.Millisecond; got != want {
		t.Errorf("HeartbeatInterval() = %v, want %v", got, want)
	}
}

func TestDialGateway_HelloTimeout(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		// Never send HELLO; keep the socket open.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := DialGateway(ctx, GatewayConfig{
		URL:          wsURL(srv.URL),
		HelloTimeout: 100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("DialGateway: expected an error when HELLO never arrives")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DialGateway took %v to time out on a missing HELLO, want well under 2s", elapsed)
	}
}

// ---- heartbeat cadence + sequence tracking ----

// TestRun_HeartbeatCadenceAndSequence is the canonical check for opcode 1
// heartbeats: they go out at roughly heartbeat_interval, carry JSON null
// before any dispatch frame has arrived, and switch to carrying the tracked
// sequence number once one has.
func TestRun_HeartbeatCadenceAndSequence(t *testing.T) {
	const interval = 40 * time.Millisecond
	heartbeats := make(chan heartbeatFrame, 16)

	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, interval.Milliseconds())

		// Push a Dispatch frame carrying sequence 7 right after HELLO, so
		// the client's read loop observes it well before Run's first
		// (jitter=0, so near-immediate) heartbeat tick fires.
		seq := int64(7)
		df, _ := json.Marshal(gatewayFrame{Op: opDispatch, T: "READY", D: json.RawMessage(`{}`), S: &seq})
		if err := conn.WriteMessage(websocket.TextMessage, df); err != nil {
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

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(ctx, nil) }()

	// Collect a few heartbeats and confirm at least one eventually carries
	// the tracked sequence number (the very first one is a race against the
	// dispatch frame's arrival, so we don't pin exactly which index it is —
	// only that sequence tracking flows through into the heartbeat frame).
	deadline := time.After(2 * time.Second)
	sawSeq := false
	count := 0
collect:
	for count < 4 {
		select {
		case hb := <-heartbeats:
			count++
			if hb.D != nil && *hb.D == 7 {
				sawSeq = true
				break collect
			}
		case <-deadline:
			break collect
		}
	}
	if !sawSeq {
		t.Error("no heartbeat carried the tracked sequence number 7 within the deadline")
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("Run() = %v, want nil after ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func TestRun_FirstHeartbeatIsNullBeforeAnyDispatch(t *testing.T) {
	const interval = 40 * time.Millisecond
	heartbeats := make(chan heartbeatFrame, 4)

	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, interval.Milliseconds())
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
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(ctx, nil) }()

	select {
	case hb := <-heartbeats:
		if hb.D != nil {
			t.Errorf("first heartbeat d = %v, want JSON null (no dispatch received yet)", *hb.D)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first heartbeat")
	}

	cancel()
	select {
	case <-runErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// ---- zombie connection (missed ACK) ----

func TestRun_MissedAckIsZombieConnection(t *testing.T) {
	const interval = 30 * time.Millisecond

	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, interval.Milliseconds())
		// Deliberately never ACK: drain reads and drop everything.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL), JitterFunc: noJitter})
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}

	start := time.Now()
	runErr := gc.Run(ctx, nil)
	elapsed := time.Since(start)

	if runErr == nil {
		t.Fatal("Run() = nil, want a zombie-connection error")
	}
	var gwErr *GatewayError
	if !errors.As(runErr, &gwErr) {
		t.Fatalf("Run() error = %v (%T), want *GatewayError", runErr, runErr)
	}
	if gwErr.Reason != ReasonZombieConnection {
		t.Errorf("Reason = %v, want ReasonZombieConnection", gwErr.Reason)
	}
	if elapsed > 2*time.Second {
		t.Errorf("zombie detection took %v, want well under 2s with a %v heartbeat interval", elapsed, interval)
	}
}

// ---- ctx watchdog: the load-bearing test ----

// TestRun_ContextCancelUnblocksBlockedRead is the watchdog invariant test:
// the server sends HELLO and then never sends or reads anything again, so
// Run's ReadMessage is genuinely parked on the socket. Cancelling ctx must
// still make Run return promptly — without the watchdog goroutine closing
// the socket, this would hang until the (much longer) read deadline.
func TestRun_ContextCancelUnblocksBlockedRead(t *testing.T) {
	serverStuck := make(chan struct{})
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		// A long heartbeat interval so the read deadline itself is nowhere
		// near firing during this test; only the watchdog should unblock
		// the client's read.
		sendHello(t, conn, (10 * time.Second).Milliseconds())
		<-serverStuck // block until the test is done
	})
	t.Cleanup(func() { close(serverStuck) })

	ctx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()

	gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL)})
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}

	// readEntered is a real synchronization point instead of a sleep-timed
	// guess: gc.testReadEntered (an unexported, test-only hook on
	// GatewayConn — see gateway.go) fires from inside Run's read loop
	// immediately before the blocking ReadMessage call that this test needs
	// to be genuinely parked before it cancels ctx. A fixed sleep can never
	// prove that; a slow CI runner can make Run take arbitrarily long to
	// reach ReadMessage.
	readEntered := make(chan struct{})
	var readEnteredOnce sync.Once
	gc.testReadEntered = func() {
		readEnteredOnce.Do(func() { close(readEntered) })
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(runCtx, nil) }()

	select {
	case <-readEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never entered ReadMessage")
	}

	start := time.Now()
	cancelRun()

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("Run() = %v, want nil on ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancellation — watchdog did not unblock the read")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run took %v to return after ctx cancellation, want well under 2s", elapsed)
	}
}

// ---- server-initiated close ----

func TestRun_ServerCloseSurfacesAsError(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, (5 * time.Second).Milliseconds())
		time.Sleep(30 * time.Millisecond)
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL)})
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(ctx, nil) }()

	select {
	case err := <-runErrCh:
		if err == nil {
			t.Fatal("Run() = nil, want an error on server-initiated close")
		}
		var gwErr *GatewayError
		if !errors.As(err, &gwErr) {
			t.Fatalf("Run() error = %v (%T), want *GatewayError", err, err)
		}
		if gwErr.Reason != ReasonServerClosed {
			t.Errorf("Reason = %v, want ReasonServerClosed", gwErr.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of the server closing the connection")
	}
}

// ---- opcode 7 / opcode 9 typed outcomes ----

func TestRun_ReconnectOpcodeIsTypedOutcome(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, (5 * time.Second).Milliseconds())
		time.Sleep(20 * time.Millisecond)
		frame, _ := json.Marshal(gatewayFrame{Op: opReconnect})
		_ = conn.WriteMessage(websocket.TextMessage, frame)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL)})
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(ctx, nil) }()

	select {
	case err := <-runErrCh:
		var gwErr *GatewayError
		if !errors.As(err, &gwErr) {
			t.Fatalf("Run() error = %v (%T), want *GatewayError", err, err)
		}
		if gwErr.Reason != ReasonReconnectRequested {
			t.Errorf("Reason = %v, want ReasonReconnectRequested", gwErr.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of opcode 7")
	}
}

func TestRun_InvalidSessionOpcodeIsTypedOutcome(t *testing.T) {
	cases := []struct {
		name      string
		resumable bool
	}{
		{"resumable", true},
		{"not_resumable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resumable := tc.resumable
			srv := newFakeGateway(t, func(conn *websocket.Conn) {
				sendHello(t, conn, (5 * time.Second).Milliseconds())
				time.Sleep(20 * time.Millisecond)
				d, _ := json.Marshal(resumable)
				frame, _ := json.Marshal(gatewayFrame{Op: opInvalidSession, D: d})
				_ = conn.WriteMessage(websocket.TextMessage, frame)
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			gc, err := DialGateway(ctx, GatewayConfig{URL: wsURL(srv.URL)})
			if err != nil {
				t.Fatalf("DialGateway: %v", err)
			}

			runErrCh := make(chan error, 1)
			go func() { runErrCh <- gc.Run(ctx, nil) }()

			select {
			case err := <-runErrCh:
				var gwErr *GatewayError
				if !errors.As(err, &gwErr) {
					t.Fatalf("Run() error = %v (%T), want *GatewayError", err, err)
				}
				if gwErr.Reason != ReasonInvalidSession {
					t.Errorf("Reason = %v, want ReasonInvalidSession", gwErr.Reason)
				}
				if gwErr.Resumable != tc.resumable {
					t.Errorf("Resumable = %v, want %v", gwErr.Resumable, tc.resumable)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return within 2s of opcode 9")
			}
		})
	}
}
