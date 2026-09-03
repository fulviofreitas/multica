// resume.go — the process-local Gateway resume cache and the RESUME
// (opcode 6) frame send (subtask 2.4). This file provides the MECHANISM
// only: recording session state from READY, deciding whether a cached
// session is still fresh enough to attempt RESUME, and sending the RESUME
// frame. It deliberately does NOT decide what to do with a close code
// (subtask 2.5 owns that policy — including IDENTIFY rate spacing) and does
// NOT wire any of this into discordChannel.Connect (subtask 2.6).
//
// Why this cache is process-local and NOT persisted to the database:
// Discord's documented resume window is only a few minutes (see resumeWindow
// below), so a process restart falling back to a fresh IDENTIFY is correct,
// expected behavior — it is exactly what Discord's own protocol design
// assumes happens whenever a client cannot resume. Persisting session state
// to the database would add a write on every READY, a staleness problem
// (the row would almost always be older than the resume window by the time
// anything reads it back after a real restart), and a cross-replica
// correctness hazard (see below) for a benefit that does not exist: nothing
// about surviving a process restart makes a multi-minute-old Discord session
// resumable.
package discord

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// resumeWindow bounds how long a stored ResumeEntry is considered fresh
// enough to attempt RESUME. Discord does not publish an exact resume-session
// TTL; the documented guidance is only that a session "may" be resumable for
// a short time after a disconnect, and Discord itself is the final arbiter —
// it answers a RESUME attempt against an expired session with Invalid
// Session (opcode 9, resumable=false), which is a cheap, well-understood
// fallback path this package already surfaces as
// GatewayError{Reason: ReasonInvalidSession}.
//
// Given that, erring toward "still fresh" costs at most one extra round trip
// before falling back to IDENTIFY. The real risk this constant guards
// against is the opposite mistake: treating a long-dead entry as resumable
// indefinitely, which would keep attempting RESUME against a session Discord
// discarded long ago instead of re-IDENTIFYing. 5 minutes is a conservative
// upper bound — comfortably longer than Discord's actual window (informal
// reports and other client implementations put the real window at well
// under a minute of active disconnection) so reconnect scheduling/backoff
// (subtask 2.5) has room to retry without this cache prematurely discarding
// a session it could have resumed, while still being short enough that an
// entry surviving this long without a successful reconnect is almost
// certainly stale.
const resumeWindow = 5 * time.Minute

// ResumeEntry is the session state needed to attempt Gateway RESUME:
// Discord's session_id, the resume_gateway_url READY told us to dial for a
// resume attempt (which may differ from the URL used to establish the
// original session), and the last sequence number seen on this session.
type ResumeEntry struct {
	SessionID        string
	ResumeGatewayURL string
	Seq              int64
	StoredAt         time.Time
}

// ResumeCacheConfig configures a ResumeCache.
type ResumeCacheConfig struct {
	// Now is overridable for deterministic tests. Defaults to time.Now.
	// Mirrors GatewayConfig.Now — freshness checks must never call
	// time.Now() directly so tests can control elapsed time exactly.
	Now func() time.Time
}

// ResumeCache is a process-wide, goroutine-safe map from installation id to
// that installation's most recent Gateway session, keyed the same way
// wecom's sendersRegistry (see senders_registry.go) keys its process-wide
// installation_id -> live-connection map. That file's clear(id, s) function
// solves a problem this cache has too: a draining/old connection's deferred
// cleanup must not be able to evict or overwrite the entry a successor
// connection has already stored. Store is a plain last-write-wins (a newer
// READY is, by construction, always the freshest available session — there
// is nothing to guard there); Clear is a compare-and-clear on entry
// identity, exactly like wecom's clear. Getting Clear wrong reproduces the
// wecom incident: a reconnect racing its predecessor's shutdown would wipe
// the fresh session of the connection that replaced it, leaving the cache
// empty (and RESUME unavailable) while a healthy resumable session exists.
type ResumeCache struct {
	mu    sync.Mutex
	byKey map[string]*ResumeEntry
	now   func() time.Time
}

// NewResumeCache constructs an empty ResumeCache.
func NewResumeCache(cfg ResumeCacheConfig) *ResumeCache {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &ResumeCache{
		byKey: make(map[string]*ResumeEntry),
		now:   now,
	}
}

// Store records session state from a READY dispatch (or from a RESUMED
// confirmation carrying an updated sequence — subtask 2.6's choice whether
// to call it there too). It always overwrites any existing entry for id:
// a newly received READY is, by construction, the freshest session
// available, so there is no successor to protect against here — only Clear
// needs the identity guard, because only Clear can run after a fresher Store
// has already happened.
//
// Returns the stored *ResumeEntry. Callers that later need to Clear this
// specific entry (e.g. on disconnect, to avoid leaving a stale entry as
// "fresh" for longer than necessary) must keep this pointer and pass it back
// to Clear — passing a freshly constructed entry would defeat the identity
// check.
func (c *ResumeCache) Store(id pgtype.UUID, sessionID, resumeGatewayURL string, seq int64) *ResumeEntry {
	entry := &ResumeEntry{
		SessionID:        sessionID,
		ResumeGatewayURL: resumeGatewayURL,
		Seq:              seq,
		StoredAt:         c.now(),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey[util.UUIDToString(id)] = entry
	return entry
}

// Load returns id's cached session and whether it is present AND still
// fresh (within resumeWindow of c.now()). This is the resume/IDENTIFY
// decision point this subtask owns: (entry, true) means a caller may attempt
// RESUME with entry; (nil, false) — whether because nothing was ever stored,
// or because what was stored is now stale — means the caller must fall back
// to a fresh IDENTIFY. Stale entries are not evicted here; they are simply
// reported absent (the next successful READY's Store will overwrite them,
// same as any other entry).
func (c *ResumeCache) Load(id pgtype.UUID) (*ResumeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byKey[util.UUIDToString(id)]
	if !ok {
		return nil, false
	}
	if c.now().Sub(entry.StoredAt) > resumeWindow {
		return nil, false
	}
	return entry, true
}

// Clear removes id's entry, but only if entry is still the exact value
// currently stored under id (pointer identity, not a value comparison — two
// distinct entries can otherwise carry identical field values and still
// belong to different connections). This is the same compare-and-clear
// discipline as wecom's sendersRegistry.clear: a connection that is
// draining/closing must only ever clear the entry IT stored, never a
// successor's. If a reconnect has already raced ahead and called Store with
// a fresh session by the time the old connection's deferred Clear runs, Clear
// is a no-op — the successor's entry survives untouched.
func (c *ResumeCache) Clear(id pgtype.UUID, entry *ResumeEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := util.UUIDToString(id)
	if cur, ok := c.byKey[key]; ok && cur != entry {
		return
	}
	delete(c.byKey, key)
}

// resumeData is the payload of an opResume frame, per Discord's documented
// RESUME shape: token, session_id, and the last sequence number this client
// saw on the session being resumed.
type resumeData struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}

// resumeFrame is the client-outbound opResume envelope.
type resumeFrame struct {
	Op int        `json:"op"`
	D  resumeData `json:"d"`
}

// newResumeFrame builds the opResume frame this integration sends.
func newResumeFrame(token string, entry *ResumeEntry) resumeFrame {
	return resumeFrame{
		Op: opResume,
		D: resumeData{
			Token:     token,
			SessionID: entry.SessionID,
			Seq:       entry.Seq,
		},
	}
}

// Resume sends the opcode 6 RESUME frame that asks Discord to replay events
// missed since entry.Seq on the session identified by entry.SessionID.
// Callers send it, like Identify, between DialGateway (dialed against
// entry.ResumeGatewayURL, not the original Gateway URL — READY documents
// them as potentially different) and Run.
//
// Resume primes gc's sequence tracker from entry.Seq BEFORE sending the
// frame (and therefore strictly before Run's heartbeat loop can start and
// send its first heartbeat): Discord replays missed events and ends the
// replay with a RESUMED dispatch, but the heartbeat loop must never send a
// stale/nil sequence number in between, and priming here — rather than
// leaving it to whichever dispatch happens to arrive first — makes that
// impossible regardless of replay timing or an empty replay.
//
// Serializes through the same writeMu discipline as sendHeartbeat and
// Identify: gorilla/websocket permits only one concurrent writer per
// connection, and (same as Identify) a future subtask may call Resume after
// Run's heartbeat goroutine has already started.
func (gc *GatewayConn) Resume(token string, entry *ResumeEntry) error {
	gc.SetSequence(entry.Seq)

	frame := newResumeFrame(token, entry)
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("discord: marshal resume frame: %w", err)
	}

	gc.writeMu.Lock()
	defer gc.writeMu.Unlock()
	if err := gc.conn.SetWriteDeadline(gc.cfg.Now().Add(gc.cfg.WriteTimeout)); err != nil {
		return fmt.Errorf("discord: set resume write deadline: %w", err)
	}
	if err := gc.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("discord: send resume: %w", err)
	}
	return nil
}
