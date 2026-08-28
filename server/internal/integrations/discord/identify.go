// identify.go — the IDENTIFY handshake (subtask 2.3): building and sending
// the opcode 2 frame that starts a new Gateway session, parsing the READY
// dispatch it triggers, and decoding dispatch frames generally enough that
// later subtasks can route them. This file does NOT implement RESUME
// (opcode 6, subtask 2.4), close-code/reconnect policy or IDENTIFY rate
// spacing (subtask 2.5), or wiring into discordChannel.Connect (subtask
// 2.6) — those subtasks consume the seams this file exposes: GatewayConn's
// Identify method, ParseReady/ReadyEvent, and DecodeDispatch/NewDispatchFunc.
package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"

	"github.com/gorilla/websocket"
)

// Gateway Intents this bot declares. Bit positions are Discord's documented
// values (https://discord.com/developers/docs/events/gateway#gateway-intents)
// as of this writing:
//
//	GUILDS                    1 << 0
//	GUILD_MEMBERS             1 << 1  (privileged)
//	GUILD_MODERATION          1 << 2
//	GUILD_EMOJIS_AND_STICKERS 1 << 3
//	GUILD_INTEGRATIONS        1 << 4
//	GUILD_WEBHOOKS            1 << 5
//	GUILD_INVITES             1 << 6
//	GUILD_VOICE_STATES        1 << 7
//	GUILD_PRESENCES           1 << 8  (privileged)
//	GUILD_MESSAGES            1 << 9
//	GUILD_MESSAGE_REACTIONS   1 << 10
//	GUILD_MESSAGE_TYPING      1 << 11
//	DIRECT_MESSAGES           1 << 12
//	DIRECT_MESSAGE_REACTIONS  1 << 13
//	DIRECT_MESSAGE_TYPING     1 << 14
//	MESSAGE_CONTENT           1 << 15 (privileged)
//
// This package intentionally declares ONLY the two non-privileged intents
// below. Do not add GUILD_MEMBERS, GUILD_PRESENCES, or MESSAGE_CONTENT
// without a product decision to enable the corresponding privileged-intent
// toggle in the Discord developer portal for every installed bot — Discord
// refuses to IDENTIFY with a privileged intent the application has not been
// granted.
const (
	// IntentGuildMessages (1 << 9) is required to receive MESSAGE_CREATE
	// (and related) events for messages sent in guild (server) text
	// channels the bot can see.
	IntentGuildMessages = 1 << 9
	// IntentDirectMessages (1 << 12) is required to receive MESSAGE_CREATE
	// events for DMs sent directly to the bot.
	IntentDirectMessages = 1 << 12

	// MESSAGE_CONTENT (1 << 15) is deliberately NOT requested. It is a
	// privileged intent gated behind a per-application toggle in the
	// Discord developer portal (and Discord's verification review once an
	// app crosses 100 servers), so requiring it would add an operational
	// dependency this integration does not need: even without
	// MESSAGE_CONTENT, Discord still includes full message content on
	// messages the bot has structural access to — DMs, messages that
	// @-mention the bot, and interactions (slash commands, components).
	// Do not "helpfully" add MESSAGE_CONTENT here; the bot already
	// receives content for the message types it acts on.
	//
	// GUILD_MEMBERS and GUILD_PRESENCES are also not requested: this
	// integration does not consume member lists or presence updates.
	// GUILD_MESSAGE_TYPING / DIRECT_MESSAGE_TYPING are not requested
	// either — the bot only ever SENDS typing indicators (a REST call),
	// it never needs to consume other users' typing events.

	// RequiredIntents is the exact intents bitmask this integration sends
	// in IDENTIFY. Computed as IntentGuildMessages | IntentDirectMessages
	// = (1<<9) | (1<<12) = 512 | 4096 = 4608.
	RequiredIntents = IntentGuildMessages | IntentDirectMessages
)

// identifyProperties is the Gateway's documented "properties" object: a
// client-identification triple with no behavioral effect on Discord's side,
// used only for their own diagnostics.
type identifyProperties struct {
	OS      string `json:"os"`
	Browser string `json:"browser"`
	Device  string `json:"device"`
}

// identifyData is the payload of an opIdentify frame. Deliberately omits
// "shard" (a single connection per installation, no sharding) and
// "presence" (out of scope for this subtask).
type identifyData struct {
	Token      string             `json:"token"`
	Intents    int                `json:"intents"`
	Properties identifyProperties `json:"properties"`
}

// identifyFrame is the client-outbound opIdentify envelope.
type identifyFrame struct {
	Op int          `json:"op"`
	D  identifyData `json:"d"`
}

// identifyOS reports the "os" property. runtime.GOOS on every platform this
// binary is actually built for; "linux" is not a meaningful fallback today
// (GOOS is never empty) but keeps this function total if that ever changes.
func identifyOS() string {
	if runtime.GOOS == "" {
		return "linux"
	}
	return runtime.GOOS
}

// newIdentifyFrame builds the opIdentify frame this integration sends.
func newIdentifyFrame(token string) identifyFrame {
	return identifyFrame{
		Op: opIdentify,
		D: identifyData{
			Token:   token,
			Intents: RequiredIntents,
			Properties: identifyProperties{
				OS:      identifyOS(),
				Browser: "multica",
				Device:  "multica",
			},
		},
	}
}

// Identify sends the opIdentify frame that starts a new Gateway session.
// Callers send it once, between DialGateway (which blocks for HELLO) and
// Run (which drives the heartbeat/read loop and will deliver the resulting
// READY as a Dispatch event through the DispatchFunc passed to Run).
//
// Serializes through the same writeMu sendHeartbeat uses: gorilla/websocket
// permits only one concurrent writer per connection, and Run's heartbeat
// goroutine may be writing concurrently once Run has started. Callers MUST
// send IDENTIFY before calling Run (there is no heartbeat goroutine yet to
// race with), but Identify still takes the lock for defense in depth and
// because a future subtask (2.4/2.5) may call it after Run has already
// started (e.g. re-IDENTIFY after a non-resumable invalid session).
func (gc *GatewayConn) Identify(token string) error {
	frame := newIdentifyFrame(token)
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("discord: marshal identify frame: %w", err)
	}

	gc.writeMu.Lock()
	defer gc.writeMu.Unlock()
	if err := gc.conn.SetWriteDeadline(gc.cfg.Now().Add(gc.cfg.WriteTimeout)); err != nil {
		return fmt.Errorf("discord: set identify write deadline: %w", err)
	}
	if err := gc.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("discord: send identify: %w", err)
	}
	return nil
}

// ReadyUser is the subset of the READY event's bot user object this
// integration needs.
type ReadyUser struct {
	ID       string
	Username string
}

// ReadyEvent captures the fields of a READY dispatch this integration needs
// to persist for later reconnects (subtask 2.4's resume cache) and to know
// the bot's own identity (e.g. for @-mention matching).
type ReadyEvent struct {
	SessionID        string
	ResumeGatewayURL string
	User             ReadyUser
}

// ParseReady decodes a READY dispatch's "d" payload. It returns an error if
// session_id or resume_gateway_url is missing: without both, a later
// RESUME attempt is impossible, so a caller must treat this as a hard
// failure of the handshake rather than silently limping on with a session
// it cannot recover.
func ParseReady(data json.RawMessage) (ReadyEvent, error) {
	var raw struct {
		SessionID        string `json:"session_id"`
		ResumeGatewayURL string `json:"resume_gateway_url"`
		User             struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ReadyEvent{}, fmt.Errorf("discord: decode READY payload: %w", err)
	}
	if raw.SessionID == "" {
		return ReadyEvent{}, errors.New("discord: READY missing session_id, cannot resume")
	}
	if raw.ResumeGatewayURL == "" {
		return ReadyEvent{}, errors.New("discord: READY missing resume_gateway_url, cannot resume")
	}
	return ReadyEvent{
		SessionID:        raw.SessionID,
		ResumeGatewayURL: raw.ResumeGatewayURL,
		User: ReadyUser{
			ID:       raw.User.ID,
			Username: raw.User.Username,
		},
	}, nil
}

// MessageAuthor is the subset of a Discord message's "author" object this
// integration needs. Bot distinguishes a message authored by another bot
// (including this bot's own echo) from a human's.
type MessageAuthor struct {
	ID       string
	Username string
	Bot      bool
}

// MessageCreateEvent is a decoded MESSAGE_CREATE dispatch. This is
// deliberately NOT mapped to channel.InboundMessage — that normalization
// (mention stripping, thread/DM classification, attachment handling, ...)
// is task 3.1's job. This struct only carries enough of the raw Discord
// shape for a later subtask to do that mapping. GuildID is empty for a DM.
type MessageCreateEvent struct {
	ID        string
	ChannelID string
	GuildID   string
	Content   string
	Author    MessageAuthor

	// ReplyToMessageID is message_reference.message_id: the id of the
	// message this one replies to, or "" if this message is not a reply.
	// Discord always includes message_reference on a reply, even when the
	// referenced message itself was deleted — so this is populated
	// independently of ReplyToAuthorID below.
	ReplyToMessageID string

	// ReplyToAuthorID is referenced_message.author.id: the author of the
	// message being replied to. It is "" whenever Discord omits or nulls
	// out referenced_message, which happens whenever the referenced
	// message was deleted, and is not guaranteed on every reply payload in
	// general. Callers MUST treat an empty ReplyToAuthorID as "unknown, not
	// a bot reply" rather than falling back to a REST call to fetch the
	// referenced message: a false negative here costs the user one extra
	// @-mention, whereas a wrong REST-derived guess (or worse, treating
	// "unknown" as "yes, it's the bot") risks the bot answering a reply
	// that was actually addressed to someone else. See inbound.go's
	// repliedToBot.
	ReplyToAuthorID string
}

// parseMessageCreate decodes a MESSAGE_CREATE dispatch's "d" payload.
func parseMessageCreate(data json.RawMessage) (MessageCreateEvent, error) {
	var raw struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
		Content   string `json:"content"`
		Author    struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Bot      bool   `json:"bot"`
		} `json:"author"`
		// MessageReference carries message_id (present on every reply,
		// regardless of whether the referenced message still exists).
		MessageReference *struct {
			MessageID string `json:"message_id"`
		} `json:"message_reference"`
		// ReferencedMessage is Discord's full copy of the replied-to
		// message. It is a pointer so both an absent field and an explicit
		// JSON null (Discord's documented behavior when the referenced
		// message was deleted) decode to nil, giving parseMessageCreate one
		// uniform "unknown" case to handle rather than two.
		ReferencedMessage *struct {
			Author struct {
				ID string `json:"id"`
			} `json:"author"`
		} `json:"referenced_message"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return MessageCreateEvent{}, fmt.Errorf("discord: decode MESSAGE_CREATE payload: %w", err)
	}
	evt := MessageCreateEvent{
		ID:        raw.ID,
		ChannelID: raw.ChannelID,
		GuildID:   raw.GuildID,
		Content:   raw.Content,
		Author: MessageAuthor{
			ID:       raw.Author.ID,
			Username: raw.Author.Username,
			Bot:      raw.Author.Bot,
		},
	}
	if raw.MessageReference != nil {
		evt.ReplyToMessageID = raw.MessageReference.MessageID
	}
	if raw.ReferencedMessage != nil {
		evt.ReplyToAuthorID = raw.ReferencedMessage.Author.ID
	}
	return evt, nil
}

// DispatchEventKind discriminates DispatchEvent's decoded payload.
type DispatchEventKind int

const (
	// EventUnhandled is any dispatch event name this package does not
	// (yet) decode into a typed payload. Discord adds new event types
	// over time; an unrecognized "t" is normal, not an error, and must
	// never terminate the connection.
	EventUnhandled DispatchEventKind = iota
	// EventReady is a decoded READY event; DispatchEvent.Ready is set.
	EventReady
	// EventMessageCreate is a decoded MESSAGE_CREATE event;
	// DispatchEvent.MessageCreate is set.
	EventMessageCreate
)

// DispatchEvent is a Dispatch (opcode 0) frame decoded far enough for a
// caller to route it. EventName always carries the raw "t" value, even for
// EventUnhandled, so a caller can log or fan out on the exact Discord event
// name without this package needing to know about it.
type DispatchEvent struct {
	Kind          DispatchEventKind
	EventName     string
	Ready         *ReadyEvent
	MessageCreate *MessageCreateEvent
}

// DecodeDispatch turns a Dispatch frame's "t" and "d" into a DispatchEvent.
// It recognizes READY and MESSAGE_CREATE by name; every other event name
// decodes successfully as EventUnhandled (Kind zero value) with no error —
// Discord adding event types over time must never be treated as a protocol
// failure. It DOES return an error when a recognized event's payload fails
// to decode (malformed JSON) or, for READY, is missing the fields resume
// depends on (see ParseReady); callers that cannot tolerate a single
// malformed frame killing the connection should use NewDispatchFunc, which
// routes decode errors to a separate callback instead of propagating them.
func DecodeDispatch(eventName string, data json.RawMessage) (DispatchEvent, error) {
	switch eventName {
	case "READY":
		ready, err := ParseReady(data)
		if err != nil {
			return DispatchEvent{}, err
		}
		return DispatchEvent{Kind: EventReady, EventName: eventName, Ready: &ready}, nil
	case "MESSAGE_CREATE":
		msg, err := parseMessageCreate(data)
		if err != nil {
			return DispatchEvent{}, err
		}
		return DispatchEvent{Kind: EventMessageCreate, EventName: eventName, MessageCreate: &msg}, nil
	default:
		return DispatchEvent{Kind: EventUnhandled, EventName: eventName}, nil
	}
}

// NewDispatchFunc adapts a typed DispatchEvent callback into the
// DispatchFunc signature GatewayConn.Run expects. onEvent is invoked for
// every successfully decoded event, including EventUnhandled. onError (may
// be nil) is invoked instead of onEvent when DecodeDispatch fails — a
// single malformed or short-of-required-fields frame (e.g. a READY missing
// session_id) is reported, not allowed to propagate, since DispatchFunc has
// no error return and a decode failure on one event must not terminate the
// Gateway connection.
func NewDispatchFunc(onEvent func(DispatchEvent), onError func(eventName string, err error)) DispatchFunc {
	return func(eventName string, data json.RawMessage) {
		evt, err := DecodeDispatch(eventName, data)
		if err != nil {
			if onError != nil {
				onError(eventName, err)
			}
			return
		}
		if onEvent != nil {
			onEvent(evt)
		}
	}
}
