package discord

// identify_test.go exercises the IDENTIFY handshake and dispatch decoding
// (subtask 2.3) against the same in-process fake Gateway pattern as
// gateway_test.go (see that file's package-level comment).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---- IDENTIFY frame contents ----

func TestIdentify_SendsExpectedFrame(t *testing.T) {
	const token = "test-bot-token"

	type receivedIdentify struct {
		Op int `json:"op"`
		D  struct {
			Token      string `json:"token"`
			Intents    int    `json:"intents"`
			Properties struct {
				OS      string `json:"os"`
				Browser string `json:"browser"`
				Device  string `json:"device"`
			} `json:"properties"`
		} `json:"d"`
	}

	identifyCh := make(chan receivedIdentify, 1)

	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, (5 * time.Second).Milliseconds())
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var got receivedIdentify
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("server: decode identify frame: %v", err)
			return
		}
		identifyCh <- got
		// Keep the connection open so nothing else in the test races a
		// server-side close.
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

	if err := gc.Identify(token); err != nil {
		t.Fatalf("Identify: %v", err)
	}

	select {
	case got := <-identifyCh:
		if got.Op != opIdentify {
			t.Errorf("op = %d, want %d", got.Op, opIdentify)
		}
		if got.D.Token != token {
			t.Errorf("token = %q, want %q", got.D.Token, token)
		}
		if got.D.Intents != RequiredIntents {
			t.Errorf("intents = %d, want %d", got.D.Intents, RequiredIntents)
		}
		if got.D.Properties.Browser != "multica" || got.D.Properties.Device != "multica" {
			t.Errorf("properties = %+v, want browser/device = multica", got.D.Properties)
		}
		if got.D.Properties.OS == "" {
			t.Error("properties.os is empty, want a non-empty GOOS value")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the IDENTIFY frame")
	}
}

// TestRequiredIntents_ExcludesPrivilegedAndUnneededIntents is the product
// guarantee this subtask must not silently regress: the intents bitmask we
// actually send never includes MESSAGE_CONTENT, GUILD_MEMBERS, or
// GUILD_PRESENCES, and does not include the typing intents either. Bits are
// computed independently here (not by referencing package constants) so a
// bug in this package's own constants cannot hide the regression.
func TestRequiredIntents_ExcludesPrivilegedAndUnneededIntents(t *testing.T) {
	const (
		guildMembers        = 1 << 1
		guildPresences      = 1 << 8
		guildMessageTyping  = 1 << 11
		directMessageTyping = 1 << 14
		messageContent      = 1 << 15
		wantGuildMessages   = 1 << 9
		wantDirectMessages  = 1 << 12
	)

	if RequiredIntents != wantGuildMessages|wantDirectMessages {
		t.Fatalf("RequiredIntents = %d, want %d (GUILD_MESSAGES|DIRECT_MESSAGES)", RequiredIntents, wantGuildMessages|wantDirectMessages)
	}
	if RequiredIntents&messageContent != 0 {
		t.Error("RequiredIntents includes MESSAGE_CONTENT (privileged), want it excluded")
	}
	if RequiredIntents&guildMembers != 0 {
		t.Error("RequiredIntents includes GUILD_MEMBERS (privileged), want it excluded")
	}
	if RequiredIntents&guildPresences != 0 {
		t.Error("RequiredIntents includes GUILD_PRESENCES (privileged), want it excluded")
	}
	if RequiredIntents&guildMessageTyping != 0 {
		t.Error("RequiredIntents includes GUILD_MESSAGE_TYPING, want it excluded (we send, not consume, typing)")
	}
	if RequiredIntents&directMessageTyping != 0 {
		t.Error("RequiredIntents includes DIRECT_MESSAGE_TYPING, want it excluded (we send, not consume, typing)")
	}
	if RequiredIntents&wantGuildMessages == 0 {
		t.Error("RequiredIntents does not include GUILD_MESSAGES")
	}
	if RequiredIntents&wantDirectMessages == 0 {
		t.Error("RequiredIntents does not include DIRECT_MESSAGES")
	}
}

// ---- READY parsing ----

func TestParseReady_CapturesSessionAndUser(t *testing.T) {
	d, err := json.Marshal(map[string]any{
		"session_id":         "sess-abc123",
		"resume_gateway_url": "wss://gateway.discord.gg/resume",
		"user": map[string]any{
			"id":       "1234567890",
			"username": "multica-bot",
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	ready, err := ParseReady(d)
	if err != nil {
		t.Fatalf("ParseReady: %v", err)
	}
	if ready.SessionID != "sess-abc123" {
		t.Errorf("SessionID = %q, want %q", ready.SessionID, "sess-abc123")
	}
	if ready.ResumeGatewayURL != "wss://gateway.discord.gg/resume" {
		t.Errorf("ResumeGatewayURL = %q, want %q", ready.ResumeGatewayURL, "wss://gateway.discord.gg/resume")
	}
	if ready.User.ID != "1234567890" {
		t.Errorf("User.ID = %q, want %q", ready.User.ID, "1234567890")
	}
	if ready.User.Username != "multica-bot" {
		t.Errorf("User.Username = %q, want %q", ready.User.Username, "multica-bot")
	}
}

func TestParseReady_MissingSessionIDIsError(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"resume_gateway_url": "wss://gateway.discord.gg/resume",
		"user":               map[string]any{"id": "1", "username": "bot"},
	})
	if _, err := ParseReady(d); err == nil {
		t.Fatal("ParseReady: got nil error, want an error for missing session_id")
	}
}

func TestParseReady_MissingResumeGatewayURLIsError(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"session_id": "sess-abc123",
		"user":       map[string]any{"id": "1", "username": "bot"},
	})
	if _, err := ParseReady(d); err == nil {
		t.Fatal("ParseReady: got nil error, want an error for missing resume_gateway_url")
	}
}

// ---- dispatch decoding ----

func TestDecodeDispatch_UnknownEventIsUnhandledWithoutError(t *testing.T) {
	evt, err := DecodeDispatch("GUILD_CREATE", json.RawMessage(`{"id":"1"}`))
	if err != nil {
		t.Fatalf("DecodeDispatch: %v, want nil error for an unrecognized event name", err)
	}
	if evt.Kind != EventUnhandled {
		t.Errorf("Kind = %v, want EventUnhandled", evt.Kind)
	}
	if evt.EventName != "GUILD_CREATE" {
		t.Errorf("EventName = %q, want %q", evt.EventName, "GUILD_CREATE")
	}
}

func TestDecodeDispatch_MessageCreate(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"id":         "999",
		"channel_id": "chan-1",
		"guild_id":   "guild-1",
		"content":    "hello @bot",
		"author": map[string]any{
			"id":       "42",
			"username": "someuser",
			"bot":      false,
		},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	if evt.Kind != EventMessageCreate {
		t.Fatalf("Kind = %v, want EventMessageCreate", evt.Kind)
	}
	if evt.MessageCreate == nil {
		t.Fatal("MessageCreate is nil")
	}
	mc := evt.MessageCreate
	if mc.ID != "999" || mc.ChannelID != "chan-1" || mc.GuildID != "guild-1" || mc.Content != "hello @bot" {
		t.Errorf("MessageCreate = %+v, unexpected fields", mc)
	}
	if mc.Author.ID != "42" || mc.Author.Username != "someuser" || mc.Author.Bot {
		t.Errorf("MessageCreate.Author = %+v, unexpected fields", mc.Author)
	}
	if mc.ReplyToMessageID != "" {
		t.Errorf("ReplyToMessageID = %q, want empty for a non-reply message (backward-compat guard)", mc.ReplyToMessageID)
	}
	if mc.ReplyToAuthorID != "" {
		t.Errorf("ReplyToAuthorID = %q, want empty for a non-reply message (backward-compat guard)", mc.ReplyToAuthorID)
	}
}

// ---- mentions array decoding ----

func TestDecodeDispatch_MessageCreate_MentionsArray(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"id":         "999",
		"channel_id": "chan-1",
		"guild_id":   "guild-1",
		"content":    "thanks for the help",
		"author": map[string]any{
			"id": "42", "username": "someuser", "bot": false,
		},
		"mentions": []map[string]any{
			{"id": "bot-id-1", "username": "multica-bot", "bot": true},
			{"id": "other-user", "username": "someone-else", "bot": false},
		},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	mc := evt.MessageCreate
	if len(mc.Mentions) != 2 {
		t.Fatalf("Mentions = %+v, want 2 entries", mc.Mentions)
	}
	if mc.Mentions[0].ID != "bot-id-1" || mc.Mentions[1].ID != "other-user" {
		t.Errorf("Mentions = %+v, want ids [bot-id-1 other-user]", mc.Mentions)
	}
}

func TestDecodeDispatch_MessageCreate_MentionsArrayAbsent(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"id":         "999",
		"channel_id": "chan-1",
		"guild_id":   "guild-1",
		"content":    "hello",
		"author": map[string]any{
			"id": "42", "username": "someuser", "bot": false,
		},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	if len(evt.MessageCreate.Mentions) != 0 {
		t.Errorf("Mentions = %+v, want empty when the field is absent", evt.MessageCreate.Mentions)
	}
}

// ---- reply linkage decoding (message_reference / referenced_message) ----

func TestDecodeDispatch_MessageCreate_ReplyWithReferencedMessage(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"id":         "999",
		"channel_id": "chan-1",
		"guild_id":   "guild-1",
		"content":    "thanks!",
		"author": map[string]any{
			"id": "42", "username": "someuser", "bot": false,
		},
		"message_reference": map[string]any{
			"message_id": "parent-1",
			"channel_id": "chan-1",
			"guild_id":   "guild-1",
		},
		"referenced_message": map[string]any{
			"id": "parent-1",
			"author": map[string]any{
				"id": "bot-id-1", "username": "multica-bot", "bot": true,
			},
		},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	mc := evt.MessageCreate
	if mc.ReplyToMessageID != "parent-1" {
		t.Errorf("ReplyToMessageID = %q, want %q", mc.ReplyToMessageID, "parent-1")
	}
	if mc.ReplyToAuthorID != "bot-id-1" {
		t.Errorf("ReplyToAuthorID = %q, want %q", mc.ReplyToAuthorID, "bot-id-1")
	}
}

func TestDecodeDispatch_MessageCreate_ReplyWithNullReferencedMessage(t *testing.T) {
	// Discord sets referenced_message to explicit JSON null (not just
	// omits it) when the referenced message was deleted.
	d, _ := json.Marshal(map[string]any{
		"id":         "999",
		"channel_id": "chan-1",
		"guild_id":   "guild-1",
		"content":    "thanks!",
		"author": map[string]any{
			"id": "42", "username": "someuser", "bot": false,
		},
		"message_reference": map[string]any{
			"message_id": "parent-1",
		},
		"referenced_message": nil,
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	mc := evt.MessageCreate
	if mc.ReplyToMessageID != "parent-1" {
		t.Errorf("ReplyToMessageID = %q, want %q", mc.ReplyToMessageID, "parent-1")
	}
	if mc.ReplyToAuthorID != "" {
		t.Errorf("ReplyToAuthorID = %q, want empty when referenced_message is null (unknown, not guessed)", mc.ReplyToAuthorID)
	}
}

func TestDecodeDispatch_MessageCreate_ReplyWithMissingReferencedMessage(t *testing.T) {
	// message_reference present, referenced_message field entirely absent
	// (same outcome as an explicit null: must not panic, must not guess).
	d, _ := json.Marshal(map[string]any{
		"id":         "999",
		"channel_id": "chan-1",
		"guild_id":   "guild-1",
		"content":    "thanks!",
		"author": map[string]any{
			"id": "42", "username": "someuser", "bot": false,
		},
		"message_reference": map[string]any{
			"message_id": "parent-1",
		},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	mc := evt.MessageCreate
	if mc.ReplyToMessageID != "parent-1" {
		t.Errorf("ReplyToMessageID = %q, want %q", mc.ReplyToMessageID, "parent-1")
	}
	if mc.ReplyToAuthorID != "" {
		t.Errorf("ReplyToAuthorID = %q, want empty when referenced_message is absent", mc.ReplyToAuthorID)
	}
}

// ---- message "type" decoding (Task Master T1) ----
//
// These pin the fix for the single biggest evidence gap in the
// not_addressed_in_group investigation: nothing decoded the "type" field, so
// a SYSTEM message (thread notice, pin notice, member-join notice, ...) and
// an ordinary user message with no mentions and no content were structurally
// indistinguishable. See MessageCreateEvent.Type's doc comment.

func TestDecodeDispatch_MessageCreate_TypeDecoded(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"id": "999", "channel_id": "chan-1", "guild_id": "guild-1",
		"content": "",
		"type":    7, // GUILD_MEMBER_JOIN, a SYSTEM message type
		"author":  map[string]any{"id": "42", "username": "someuser", "bot": false},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	if evt.MessageCreate.Type != 7 {
		t.Errorf("Type = %d, want 7", evt.MessageCreate.Type)
	}
}

func TestDecodeDispatch_MessageCreate_TypeAbsentDefaultsToZero(t *testing.T) {
	// No "type" field at all — Discord always sends one in practice, but the
	// decoder must not panic or misreport if it were ever missing. Zero here
	// is the DEFAULT message type, indistinguishable from an explicit
	// "type":0; that ambiguity belongs to Discord's wire format, not to this
	// decoder.
	d, _ := json.Marshal(map[string]any{
		"id": "999", "channel_id": "chan-1", "guild_id": "guild-1",
		"content": "hello",
		"author":  map[string]any{"id": "42", "username": "someuser", "bot": false},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	if evt.MessageCreate.Type != 0 {
		t.Errorf("Type = %d, want 0 (DEFAULT) when the field is absent", evt.MessageCreate.Type)
	}
}

// ---- HasMessageReference decoding (Task Master T1) ----

func TestDecodeDispatch_MessageCreate_HasMessageReferenceTrue(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"id": "999", "channel_id": "chan-1", "guild_id": "guild-1",
		"content": "thanks!",
		"author":  map[string]any{"id": "42", "username": "someuser", "bot": false},
		"message_reference": map[string]any{
			"message_id": "parent-1",
		},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	if !evt.MessageCreate.HasMessageReference {
		t.Error("HasMessageReference = false, want true when message_reference is present")
	}
}

func TestDecodeDispatch_MessageCreate_HasMessageReferenceFalse(t *testing.T) {
	d, _ := json.Marshal(map[string]any{
		"id": "999", "channel_id": "chan-1", "guild_id": "guild-1",
		"content": "hello",
		"author":  map[string]any{"id": "42", "username": "someuser", "bot": false},
	})

	evt, err := DecodeDispatch("MESSAGE_CREATE", d)
	if err != nil {
		t.Fatalf("DecodeDispatch: %v", err)
	}
	if evt.MessageCreate.HasMessageReference {
		t.Error("HasMessageReference = true, want false when message_reference is absent")
	}
	if evt.MessageCreate.ReplyToMessageID != "" {
		t.Errorf("ReplyToMessageID = %q, want empty", evt.MessageCreate.ReplyToMessageID)
	}
}

// ---- end-to-end over Run: IDENTIFY -> READY, unknown event tolerance, sequence tracking ----

// TestRun_DispatchesReadyAndTracksSequence drives a full handshake over the
// fake Gateway: HELLO, client sends IDENTIFY, server replies with READY (a
// Dispatch frame), then an unrecognized event, then a MESSAGE_CREATE, each
// carrying an increasing sequence number. It asserts NewDispatchFunc routes
// each to the right typed event, the unknown event does not error or stop
// Run, and GatewayConn.Sequence() advances across all of them.
func TestRun_DispatchesReadyAndTracksSequence(t *testing.T) {
	const interval = 5 * time.Second // long enough that no heartbeat noise interferes

	type result struct {
		kind DispatchEventKind
		name string
	}
	events := make(chan result, 8)
	decodeErrs := make(chan error, 8)

	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, interval.Milliseconds())

		// Wait for the client's IDENTIFY before sending anything dispatch-shaped.
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var identify struct {
			Op int `json:"op"`
		}
		if err := json.Unmarshal(raw, &identify); err != nil || identify.Op != opIdentify {
			t.Errorf("server: first client frame was not IDENTIFY: %s", raw)
			return
		}

		send := func(seq int64, eventName string, data map[string]any) {
			d, _ := json.Marshal(data)
			frame, _ := json.Marshal(gatewayFrame{Op: opDispatch, T: eventName, D: d, S: &seq})
			_ = conn.WriteMessage(websocket.TextMessage, frame)
		}

		send(1, "READY", map[string]any{
			"session_id":         "sess-xyz",
			"resume_gateway_url": "wss://gateway.discord.gg/resume",
			"user":               map[string]any{"id": "1", "username": "multica-bot"},
		})
		send(2, "GUILD_CREATE", map[string]any{"id": "guild-1"})
		send(3, "MESSAGE_CREATE", map[string]any{
			"id": "msg-1", "channel_id": "chan-1", "guild_id": "",
			"content": "hi", "author": map[string]any{"id": "42", "username": "u", "bot": false},
		})

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

	if err := gc.Identify("test-token"); err != nil {
		t.Fatalf("Identify: %v", err)
	}

	dispatch := NewDispatchFunc(
		func(evt DispatchEvent) { events <- result{kind: evt.Kind, name: evt.EventName} },
		func(eventName string, err error) { decodeErrs <- err },
	)

	runCtx, runCancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(runCtx, dispatch) }()
	defer runCancel()

	wantKinds := []DispatchEventKind{EventReady, EventUnhandled, EventMessageCreate}
	for i, want := range wantKinds {
		select {
		case got := <-events:
			if got.kind != want {
				t.Errorf("event %d: kind = %v, want %v (name=%q)", i, got.kind, want, got.name)
			}
		case err := <-decodeErrs:
			t.Fatalf("event %d: unexpected decode error: %v", i, err)
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d: timed out waiting for dispatch event", i)
		}
	}

	// Sequence must have advanced to the last frame's "s" (3).
	deadline := time.After(2 * time.Second)
	for {
		if seq, ok := gc.Sequence(); ok && seq == 3 {
			break
		}
		select {
		case <-deadline:
			seq, ok := gc.Sequence()
			t.Fatalf("Sequence() = (%d, %v), want (3, true) after all dispatch frames", seq, ok)
		case <-time.After(10 * time.Millisecond):
		}
	}

	runCancel()
	select {
	case runErr := <-runErrCh:
		if runErr != nil {
			t.Errorf("Run() = %v, want nil after ctx cancellation (unknown event must not terminate Run)", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func TestRun_ReadyMissingRequiredFieldsIsReportedAsDecodeError(t *testing.T) {
	srv := newFakeGateway(t, func(conn *websocket.Conn) {
		sendHello(t, conn, (5 * time.Second).Milliseconds())
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var identify struct {
			Op int `json:"op"`
		}
		if err := json.Unmarshal(raw, &identify); err != nil || identify.Op != opIdentify {
			return
		}
		// READY with no session_id / resume_gateway_url.
		d, _ := json.Marshal(map[string]any{"user": map[string]any{"id": "1", "username": "bot"}})
		seq := int64(1)
		frame, _ := json.Marshal(gatewayFrame{Op: opDispatch, T: "READY", D: d, S: &seq})
		_ = conn.WriteMessage(websocket.TextMessage, frame)
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
	if err := gc.Identify("test-token"); err != nil {
		t.Fatalf("Identify: %v", err)
	}

	decodeErrs := make(chan error, 1)
	dispatch := NewDispatchFunc(
		func(evt DispatchEvent) { t.Errorf("onEvent called with %+v, want onError for a malformed READY", evt) },
		func(eventName string, err error) { decodeErrs <- err },
	)

	runCtx, runCancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- gc.Run(runCtx, dispatch) }()

	select {
	case err := <-decodeErrs:
		if err == nil {
			t.Error("decode error channel received nil, want a non-nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the malformed READY to be reported as a decode error")
	}

	// Run must still be alive (a decode error must not terminate the
	// connection): cancel and confirm a clean nil exit rather than the
	// error having already surfaced from Run itself.
	runCancel()
	select {
	case runErr := <-runErrCh:
		if runErr != nil {
			t.Errorf("Run() = %v, want nil after ctx cancellation", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}
