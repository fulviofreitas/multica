// gateway.go — the Discord Gateway WebSocket transport layer: dialing,
// receiving and honoring the mandatory HELLO/heartbeat/HEARTBEAT_ACK
// protocol, and the ctx→read-interrupt watchdog. This file is transport
// only (Task Master subtask 2.2). It deliberately does NOT send IDENTIFY,
// parse READY, cache resume state, or implement close-code reconnect
// policy — those are subtasks 2.3-2.6, built on the seams this file
// exposes (GatewayConn's DispatchFunc callback and its Sequence/SetSequence
// accessors, and the typed GatewayError/DisconnectReason outcomes).
//
// Ownership of the ctx→read-interrupt watchdog invariant (mirrors
// lark.WSLongConnConnector.Run and wecom.wecomChannel.Connect — see
// lark/ws_connector.go:41-53 for the canonical writeup this comment
// reproduces):
//
// gorilla/websocket's ReadMessage blocks on the underlying TCP socket and
// does NOT observe a context. Cancelling ctx flips ctx.Done() but does
// nothing to the blocked read syscall. Run bridges ctx cancellation to the
// read by running a watchdog goroutine that calls conn.Close() the moment
// ctx fires; gorilla's Close causes any in-flight ReadMessage to return
// immediately with a "use of closed connection" error, which Run then
// recognizes (via ctx.Err() != nil) as a clean shutdown rather than a
// failure. The watchdog runs on every Run exit path, not just ctx
// cancellation, so it never leaks a goroutine; conn.Close is idempotent, so
// a watchdog race with Run's own cleanup close is harmless. Without this
// bridge, a lease loss or shutdown signal cannot interrupt a Gateway
// connection blocked on a healthy-looking but silent socket, and — same as
// the WeCom incident this PRD cites — two replicas could end up holding a
// connection for the same installation at once. Any future change to this
// file's read loop MUST preserve this invariant.
package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// DisconnectReason classifies why GatewayConn.Run returned a non-nil error,
// so a caller (subtask 2.5's reconnect/close-code policy) can branch on the
// outcome instead of string-matching an error message.
type DisconnectReason int

const (
	// ReasonUnknown is the zero value; Run never returns it deliberately.
	ReasonUnknown DisconnectReason = iota
	// ReasonServerClosed means the Gateway sent a normal WebSocket close
	// frame.
	ReasonServerClosed
	// ReasonReadError means the socket failed for a reason other than a
	// clean close or ctx cancellation (network error, corrupt frame
	// terminating the transport, closed-by-watchdog-during-shutdown edge
	// cases already handled separately).
	ReasonReadError
	// ReasonZombieConnection means a HEARTBEAT_ACK did not arrive before
	// the next heartbeat came due. Discord's documented defense against a
	// half-dead socket that still looks "open" — same failure class as
	// the WeCom bug this PRD cites.
	ReasonZombieConnection
	// ReasonReconnectRequested means Discord sent opcode 7 (Reconnect):
	// the client should reconnect (and MAY resume) immediately.
	ReasonReconnectRequested
	// ReasonInvalidSession means Discord sent opcode 9 (Invalid Session).
	// GatewayError.Resumable reports the payload's bool: true allows a
	// RESUME attempt after a short wait, false requires a fresh IDENTIFY.
	ReasonInvalidSession
)

func (r DisconnectReason) String() string {
	switch r {
	case ReasonServerClosed:
		return "server_closed"
	case ReasonReadError:
		return "read_error"
	case ReasonZombieConnection:
		return "zombie_connection"
	case ReasonReconnectRequested:
		return "reconnect_requested"
	case ReasonInvalidSession:
		return "invalid_session"
	default:
		return "unknown"
	}
}

// GatewayError is the typed outcome GatewayConn.Run returns for every
// non-nil, non-ctx-cancellation exit. Callers should use errors.As to
// inspect Reason rather than matching on Error()'s text.
type GatewayError struct {
	Reason DisconnectReason
	// Resumable is only meaningful when Reason == ReasonInvalidSession.
	Resumable bool
	// Err is the underlying cause, when there is one (a transport error,
	// or a synthesized zombie-connection error). May be nil for pure
	// protocol signals (ReasonReconnectRequested, ReasonInvalidSession).
	Err error
}

func (e *GatewayError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("discord gateway: %s: %v", e.Reason, e.Err)
	}
	return fmt.Sprintf("discord gateway: %s", e.Reason)
}

func (e *GatewayError) Unwrap() error { return e.Err }

// GatewayDialer opens the Gateway WebSocket transport. *GorillaGatewayDialer
// satisfies it against the real Discord Gateway; tests inject a fake that
// points at an httptest server.
type GatewayDialer interface {
	DialContext(ctx context.Context, urlStr string, requestHeader http.Header) (WSConn, *http.Response, error)
}

// WSConn is the subset of *websocket.Conn this file uses. Extracted as an
// interface (mirroring lark.WSConn / wecom.wsConn) so tests can inject a
// fully in-process fake when they need to script behavior finer than an
// httptest server allows; the test suite in gateway_test.go primarily uses
// a real httptest + gorilla-upgrader server instead.
type WSConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	Close() error
}

// GorillaGatewayDialer is the production GatewayDialer.
type GorillaGatewayDialer struct {
	HandshakeTimeout time.Duration
}

// DialContext implements GatewayDialer using gorilla's default dialer with
// the configured handshake timeout.
func (g *GorillaGatewayDialer) DialContext(ctx context.Context, urlStr string, requestHeader http.Header) (WSConn, *http.Response, error) {
	timeout := g.HandshakeTimeout
	if timeout <= 0 {
		timeout = defaultHandshakeTimeout
	}
	dialer := &websocket.Dialer{HandshakeTimeout: timeout}
	conn, resp, err := dialer.DialContext(ctx, urlStr, requestHeader)
	if err != nil {
		return nil, resp, err
	}
	return conn, resp, nil
}

const (
	defaultHandshakeTimeout  = 10 * time.Second
	defaultHelloTimeout      = 10 * time.Second
	defaultWriteTimeout      = 10 * time.Second
	defaultReadTimeoutMargin = 10 * time.Second
)

// GatewayConfig configures DialGateway / GatewayConn.Run. All duration
// fields default when zero; see withDefaults.
type GatewayConfig struct {
	// URL is the Gateway WebSocket endpoint to dial. Required. Production
	// resolves this from GET /gateway (a later subtask's concern); tests
	// point it at an httptest server's ws(s):// URL.
	URL string

	// Dialer opens the transport. Defaults to *GorillaGatewayDialer.
	Dialer GatewayDialer

	// HandshakeTimeout bounds the WebSocket upgrade handshake. Zero
	// defaults to 10s.
	HandshakeTimeout time.Duration

	// HelloTimeout bounds how long DialGateway waits for the mandatory
	// first HELLO frame before giving up. Zero defaults to 10s.
	HelloTimeout time.Duration

	// WriteTimeout bounds a single WriteMessage call (heartbeats). Zero
	// defaults to 10s.
	WriteTimeout time.Duration

	// ReadTimeoutMargin is added on top of the negotiated
	// heartbeat_interval to compute each read deadline: a read is only
	// unusual once we are overdue for both a heartbeat's ACK and Discord's
	// own liveness traffic. Zero defaults to 10s.
	ReadTimeoutMargin time.Duration

	// JitterFunc returns a value in [0,1) used to delay the first
	// heartbeat by heartbeat_interval*jitter, per Discord's documented
	// startup requirement (spreads reconnect storms across the interval
	// instead of every client heartbeating in lockstep). Defaults to
	// math/rand's global source. Tests inject a fixed func for
	// determinism.
	JitterFunc func() float64

	// Now is overridable for deterministic tests. Defaults to time.Now.
	Now func() time.Time

	// Logger is optional; defaults to slog.Default().
	Logger *slog.Logger
}

func (c GatewayConfig) withDefaults() GatewayConfig {
	if c.Dialer == nil {
		c.Dialer = &GorillaGatewayDialer{HandshakeTimeout: c.HandshakeTimeout}
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = defaultHandshakeTimeout
	}
	if c.HelloTimeout <= 0 {
		c.HelloTimeout = defaultHelloTimeout
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = defaultWriteTimeout
	}
	if c.ReadTimeoutMargin <= 0 {
		c.ReadTimeoutMargin = defaultReadTimeoutMargin
	}
	if c.JitterFunc == nil {
		c.JitterFunc = rand.Float64
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// GatewayConn is one dialed Gateway WebSocket session's transport state:
// the socket, the negotiated heartbeat interval, and the last received
// sequence number. It is the seam subtasks 2.3-2.6 build IDENTIFY/RESUME,
// session bookkeeping, and reconnect policy on top of — this file only
// implements the transport-level contract every Gateway session needs
// regardless of what protocol state sits above it.
type GatewayConn struct {
	conn              WSConn
	cfg               GatewayConfig
	heartbeatInterval time.Duration

	seq    atomic.Int64
	hasSeq atomic.Bool

	// closedByWatchdog is the authoritative, race-free signal that a
	// ctx-cancellation watchdog goroutine (readHello's or Run's) is the one
	// that closed conn. It is set strictly BEFORE the watchdog's Close()
	// call, never after, so there is a happens-before edge from the Store to
	// the Close syscall to the blocked ReadMessage's return to the Load: by
	// the time a read that this watchdog unblocked comes back with an
	// error, the flag is already visible to the goroutine that reads it.
	//
	// Checking ctx.Err() alone (the previous approach) only proves ctx *was*
	// cancelled by the time of the check; it does not prove ctx cancellation
	// is *why* the pending read returned. A read can fail for an unrelated
	// reason (peer close, network blip) microseconds before or after an
	// unrelated ctx cancellation becomes visible on the reading goroutine,
	// and that interleaving is exactly what made
	// TestRun_ContextCancelUnblocksBlockedRead flaky under plain (non -race)
	// scheduling in CI: the ctx.Err() check is a timing guess, not a
	// synchronization point. This flag replaces the guess with an explicit
	// fact set by the one goroutine that actually decided to close the
	// socket for shutdown reasons.
	//
	// Only the ctx-driven watchdog goroutines may set this flag. In
	// particular heartbeatLoop's zombie-connection and heartbeat-write-error
	// close paths must NEVER set it — those are real failures, and a flag
	// set there would make Run report a genuine outage as a clean shutdown,
	// which is worse than the race this flag fixes. heartbeatLoop calls the
	// shared closeConn helper directly, without touching this field.
	closedByWatchdog atomic.Bool

	writeMu sync.Mutex

	// testReadEntered, when non-nil, is invoked by Run's read loop
	// immediately before every blocking ReadMessage call. It exists solely
	// so gateway_test.go can deterministically synchronize with "Run is
	// genuinely parked in ReadMessage" instead of guessing with a
	// time.Sleep; production code never sets it. The extra nil check on the
	// hot read-loop path is negligible.
	testReadEntered func()
}

// DispatchFunc receives each Dispatch (opcode 0) frame's event name and raw
// "d" payload, after Run has already updated the sequence tracker. Nil is
// valid: Run still tracks the sequence number and simply drops the event,
// which is everything this subtask needs — decoding named events (READY,
// MESSAGE_CREATE, ...) is subtask 2.3's job.
type DispatchFunc func(eventName string, data json.RawMessage)

// DialGateway opens the Gateway WebSocket and blocks until the mandatory
// first HELLO frame (or cfg.HelloTimeout, or ctx cancellation) resolves.
// Splitting Dial from Run lets a caller send IDENTIFY/RESUME between the two
// (subtask 2.3/2.4) without this package knowing about either — Run only
// needs the negotiated heartbeat interval, which is available on the
// returned GatewayConn once DialGateway succeeds.
func DialGateway(ctx context.Context, cfg GatewayConfig) (*GatewayConn, error) {
	cfg = cfg.withDefaults()
	if cfg.URL == "" {
		return nil, errors.New("discord: gateway URL is required")
	}

	dialCtx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancel()

	conn, _, err := cfg.Dialer.DialContext(dialCtx, cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("discord: dial gateway: %w", err)
	}

	gc := &GatewayConn{conn: conn, cfg: cfg}
	if err := gc.readHello(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return gc, nil
}

// readHello waits for the single HELLO frame Discord guarantees is the
// first message on a freshly dialed Gateway socket. It applies the same
// ctx→read-interrupt watchdog as Run's main loop: HelloTimeout alone would
// not let an outer ctx cancellation (e.g. a shutdown that lands mid-handshake)
// interrupt a blocked ReadMessage.
func (gc *GatewayConn) readHello(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Set the flag BEFORE Close: see GatewayConn.closedByWatchdog
			// for why ordering here matters.
			gc.closedByWatchdog.Store(true)
			_ = gc.conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	if err := gc.conn.SetReadDeadline(gc.cfg.Now().Add(gc.cfg.HelloTimeout)); err != nil {
		return fmt.Errorf("discord: set hello read deadline: %w", err)
	}
	_, raw, err := gc.conn.ReadMessage()
	if err != nil {
		if gc.closedByWatchdog.Load() || ctx.Err() != nil {
			// DialGateway's contract (see connect.go's dial() caller) is to
			// surface ctx.Err() here, not nil — this file's readHello has
			// no equivalent of Run's "return nil on clean ctx cancellation"
			// promise, and connect.go already translates this specific
			// non-nil-but-actually-clean case itself. The flag only makes
			// *entering* this branch race-free; it does not change what we
			// return.
			return ctx.Err()
		}
		return fmt.Errorf("discord: read hello: %w", err)
	}

	var frame gatewayFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return fmt.Errorf("discord: decode hello frame: %w", err)
	}
	if frame.Op != opHello {
		return fmt.Errorf("discord: expected HELLO (op %d) as the first frame, got op %d", opHello, frame.Op)
	}
	var hello helloData
	if err := json.Unmarshal(frame.D, &hello); err != nil {
		return fmt.Errorf("discord: decode hello payload: %w", err)
	}
	if hello.HeartbeatInterval <= 0 {
		return fmt.Errorf("discord: hello heartbeat_interval must be positive, got %d", hello.HeartbeatInterval)
	}
	gc.heartbeatInterval = time.Duration(hello.HeartbeatInterval) * time.Millisecond
	if frame.S != nil {
		gc.SetSequence(*frame.S)
	}
	return nil
}

// HeartbeatInterval reports the interval negotiated via HELLO. Only valid
// after DialGateway returns successfully.
func (gc *GatewayConn) HeartbeatInterval() time.Duration { return gc.heartbeatInterval }

// Sequence returns the last sequence number seen on any frame that carried
// one, and whether any frame has carried one yet. Exposed for subtask 2.3's
// dispatch decoding and subtask 2.4's resume bookkeeping — sequence
// tracking lives here because every incoming frame (not just Dispatch) can
// carry "s", and the heartbeat loop needs it regardless of whether anything
// above this layer decodes dispatch events at all.
func (gc *GatewayConn) Sequence() (seq int64, ok bool) {
	return gc.seq.Load(), gc.hasSeq.Load()
}

// SetSequence primes the sequence tracker. Exposed for subtask 2.4, which
// needs to restore the sequence number a resumed session left off at before
// the first heartbeat after resume goes out.
func (gc *GatewayConn) SetSequence(s int64) {
	gc.seq.Store(s)
	gc.hasSeq.Store(true)
}

// Close closes the underlying socket. Safe to call more than once and safe
// to call after Run has already returned.
func (gc *GatewayConn) Close() error { return gc.conn.Close() }

// Run drives one Gateway session: starts the heartbeat loop (delayed by the
// mandatory startup jitter), starts the ctx watchdog, and reads frames until
// ctx is cancelled or the connection ends. It returns:
//
//   - nil when ctx is cancelled (the watchdog closed the socket to unblock
//     the read; this is a clean shutdown, not a failure);
//   - a *GatewayError for every other exit, with Reason identifying why.
//
// onDispatch is invoked synchronously from the read loop for every Dispatch
// (opcode 0) frame, after the sequence tracker has already been updated;
// pass nil if the caller does not need dispatch events yet (this subtask's
// own tests do not).
func (gc *GatewayConn) Run(ctx context.Context, onDispatch DispatchFunc) error {
	log := gc.cfg.Logger

	// runCtx fans cancellation out to the watchdog and heartbeat goroutines
	// on EVERY Run exit, not just outer-ctx cancellation — a zombie
	// connection or a read error must also stop the heartbeat loop, or the
	// deferred wait below would hang.
	runCtx, runCancel := context.WithCancel(ctx)

	var closeOnce sync.Once
	closeConn := func() {
		closeOnce.Do(func() {
			_ = gc.conn.Close()
		})
	}

	// Watchdog: see this file's package-level doc comment for the full
	// reasoning. Runs on every exit path (via `watchdogDone`), not just
	// ctx cancellation, so it never leaks.
	watchdogDone := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			// Set the flag BEFORE closing: see GatewayConn.closedByWatchdog
			// for why the ordering here is what makes the read-error checks
			// below race-free instead of a timing guess.
			gc.closedByWatchdog.Store(true)
			closeConn()
		case <-watchdogDone:
		}
	}()

	// ackPending is true from the moment a heartbeat is sent until its
	// HEARTBEAT_ACK arrives. Shared between the heartbeat goroutine (which
	// checks it to detect a zombie connection) and Run's read loop (which
	// clears it on opHeartbeatACK, and sets it again when answering a
	// server-requested opHeartbeat).
	var ackPending atomic.Bool

	hbErrCh := make(chan error, 1)
	hbDone := make(chan struct{})
	go gc.heartbeatLoop(runCtx, closeConn, &ackPending, hbErrCh, hbDone)

	defer func() {
		runCancel()
		closeConn()
		close(watchdogDone)
		<-hbDone
	}()

	for {
		// Re-armed before every read. A read is only "stuck" once we are
		// overdue for both our own heartbeat's ACK and Discord's own
		// traffic, which heartbeatInterval+ReadTimeoutMargin bounds.
		deadline := gc.cfg.Now().Add(gc.heartbeatInterval + gc.cfg.ReadTimeoutMargin)
		if err := gc.conn.SetReadDeadline(deadline); err != nil {
			if gc.closedByWatchdog.Load() || ctx.Err() != nil {
				return nil
			}
			return &GatewayError{Reason: ReasonReadError, Err: fmt.Errorf("set read deadline: %w", err)}
		}

		if gc.testReadEntered != nil {
			gc.testReadEntered()
		}
		_, raw, err := gc.conn.ReadMessage()
		if err != nil {
			if gc.closedByWatchdog.Load() || ctx.Err() != nil {
				return nil
			}
			// Prefer the heartbeat goroutine's verdict when both fired at
			// once (it is what actually closed the socket).
			select {
			case hbErr := <-hbErrCh:
				return hbErr
			default:
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return &GatewayError{Reason: ReasonServerClosed, Err: err}
			}
			return &GatewayError{Reason: ReasonReadError, Err: err}
		}

		var frame gatewayFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			log.Warn("discord gateway: frame decode failed", "err", err.Error(), "raw_len", len(raw))
			continue
		}
		if frame.S != nil {
			gc.SetSequence(*frame.S)
		}

		switch frame.Op {
		case opDispatch:
			if onDispatch != nil {
				onDispatch(frame.T, frame.D)
			}
		case opHeartbeat:
			// Discord may request an out-of-cadence heartbeat. Answer
			// immediately; this counts as a new heartbeat awaiting ACK.
			if err := gc.sendHeartbeat(&ackPending); err != nil {
				return &GatewayError{Reason: ReasonReadError, Err: fmt.Errorf("send requested heartbeat: %w", err)}
			}
		case opHeartbeatACK:
			ackPending.Store(false)
		case opReconnect:
			return &GatewayError{Reason: ReasonReconnectRequested}
		case opInvalidSession:
			var resumable bool
			_ = json.Unmarshal(frame.D, &resumable)
			return &GatewayError{Reason: ReasonInvalidSession, Resumable: resumable}
		default:
			log.Debug("discord gateway: unhandled opcode", "op", frame.Op)
		}
	}
}

// heartbeatLoop sends heartbeats on the negotiated cadence (delayed by the
// mandatory startup jitter for the first one) and detects a zombie
// connection: if ackPending is still true when the next heartbeat comes
// due, no HEARTBEAT_ACK arrived in time, so the socket is closed and a
// typed error is delivered on errCh.
func (gc *GatewayConn) heartbeatLoop(ctx context.Context, closeConn func(), ackPending *atomic.Bool, errCh chan<- error, done chan<- struct{}) {
	defer close(done)

	jitter := gc.cfg.JitterFunc()
	if jitter < 0 || jitter >= 1 {
		// A misbehaving injected JitterFunc must not produce a negative or
		// unbounded delay; fall back to "no delay" rather than trust it.
		jitter = 0
	}
	firstDelay := time.Duration(float64(gc.heartbeatInterval) * jitter)

	timer := time.NewTimer(firstDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ackPending.Load() {
				err := &GatewayError{
					Reason: ReasonZombieConnection,
					Err:    errors.New("no HEARTBEAT_ACK received before the next heartbeat was due"),
				}
				select {
				case errCh <- err:
				default:
				}
				closeConn()
				return
			}
			if err := gc.sendHeartbeat(ackPending); err != nil {
				select {
				case errCh <- &GatewayError{Reason: ReasonReadError, Err: fmt.Errorf("send heartbeat: %w", err)}:
				default:
				}
				closeConn()
				return
			}
			timer.Reset(gc.heartbeatInterval)
		}
	}
}

// sendHeartbeat writes one opcode 1 frame carrying the last received
// sequence number (or JSON null when none has arrived yet), and marks
// ackPending so the zombie check can see it.
func (gc *GatewayConn) sendHeartbeat(ackPending *atomic.Bool) error {
	var seqPtr *int64
	if s, ok := gc.Sequence(); ok {
		seqPtr = &s
	}
	payload, err := json.Marshal(heartbeatFrame{Op: opHeartbeat, D: seqPtr})
	if err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}

	ackPending.Store(true)

	gc.writeMu.Lock()
	defer gc.writeMu.Unlock()
	if err := gc.conn.SetWriteDeadline(gc.cfg.Now().Add(gc.cfg.WriteTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	return gc.conn.WriteMessage(websocket.TextMessage, payload)
}
