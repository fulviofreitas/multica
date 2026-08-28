package discord

import "encoding/json"

// gateway_frames.go — the Gateway wire opcodes and the JSON envelope every
// Gateway payload (inbound and outbound) is wrapped in. See
// https://discord.com/developers/docs/topics/gateway#payloads for the
// authoritative opcode table; only the opcodes this transport layer must
// recognize are named below. Opcodes owned by later subtasks (IDENTIFY,
// RESUME, DISPATCH event parsing) are listed for completeness but this file
// does not implement their payloads.
const (
	// opDispatch (0) carries a named event ("t") with its data ("d") and
	// always carries the sequence number ("s"). This subtask only tracks
	// the sequence number; decoding "t"/"d" into normalized events is
	// subtask 2.3's job (see GatewayConn.Run's DispatchFunc parameter).
	opDispatch = 0
	// opHeartbeat (1) is sent by the client on the negotiated cadence, and
	// may also be sent BY DISCORD to request an immediate client
	// heartbeat outside the normal cadence — Run answers that request
	// inline.
	opHeartbeat = 1
	// opIdentify (2) starts a new session. Owned by subtask 2.3.
	opIdentify = 2
	// opResume (6) resumes a previous session. Owned by subtask 2.4.
	opResume = 6
	// opReconnect (7) tells the client to reconnect (and, if it has a
	// session, resume) immediately. Surfaced as GatewayError{Reason:
	// ReasonReconnectRequested}; deciding whether/how to resume is a
	// later subtask.
	opReconnect = 7
	// opInvalidSession (9) tells the client its session is invalid. The
	// payload "d" is a bool: true means the session MAY be resumed after
	// a short wait, false means it must re-IDENTIFY from scratch.
	// Surfaced as GatewayError{Reason: ReasonInvalidSession, Resumable}.
	opInvalidSession = 9
	// opHello (10) is always the first frame the Gateway sends after the
	// WebSocket handshake completes. Its payload carries
	// heartbeat_interval, which this file's DialGateway consumes.
	opHello = 10
	// opHeartbeatACK (11) is Discord's reply to a client heartbeat. A
	// missed ACK is this package's zombie-connection signal.
	opHeartbeatACK = 11
)

// gatewayFrame is the generic envelope every Gateway payload (send or
// receive) uses. "d" is left as json.RawMessage because its shape depends
// entirely on "op" (and, for Dispatch, on "t") — decoding it further is the
// concern of whichever opcode handler consumes it.
type gatewayFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// helloData is the payload of an opHello frame.
type helloData struct {
	HeartbeatInterval int64 `json:"heartbeat_interval"`
}

// heartbeatFrame is the client-outbound opHeartbeat payload. D is a pointer
// so "no sequence received yet" marshals as the JSON literal `null` (per
// Discord's documented requirement) rather than being omitted — this
// struct's "d" tag deliberately has no `omitempty`.
type heartbeatFrame struct {
	Op int    `json:"op"`
	D  *int64 `json:"d"`
}
