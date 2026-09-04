package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeDiscordBindingMinter struct {
	calls int
	raw   string
}

func (f *fakeDiscordBindingMinter) Mint(context.Context, pgtype.UUID, pgtype.UUID, string) (BindingToken, error) {
	f.calls++
	return BindingToken{Raw: f.raw}, nil
}

// fakeControlAckLedger fakes controlAckLedger so replier tests can assert
// post's ledger write without a database. Injected directly onto the
// unexported OutboundReplier.ledger field (same package as this test file)
// rather than through OutboundReplierConfig, because that config's Queries
// field is a concrete *db.Queries (mirroring slack.OutboundReplierConfig
// exactly) and cannot carry a fake.
type fakeControlAckLedger struct {
	mu       sync.Mutex
	recorded []db.RecordChannelOutboundMessageParams
	errOn    string // OutboundMessageID to fail for, or "" for none
	err      error
}

func (f *fakeControlAckLedger) RecordChannelOutboundMessage(_ context.Context, arg db.RecordChannelOutboundMessageParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, arg)
	if f.err != nil && arg.OutboundMessageID == f.errOn {
		return f.err
	}
	return nil
}

func (f *fakeControlAckLedger) snapshot() []db.RecordChannelOutboundMessageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.RecordChannelOutboundMessageParams, len(f.recorded))
	copy(out, f.recorded)
	return out
}

func newReplierTestServer(t *testing.T, respond func(discordMessagePayload) (int, discordMessageResponse)) (*httptest.Server, *discordMessagePayload) {
	t.Helper()
	var got discordMessagePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		status := http.StatusOK
		body := discordMessageResponse{ID: "msg-verdict-1"}
		if respond != nil {
			status, body = respond(got)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func replierTestInstallation(t *testing.T) engine.ResolvedInstallation {
	t.Helper()
	return engine.ResolvedInstallation{
		ID:          dbidTestUUID(30),
		WorkspaceID: dbidTestUUID(31),
		Platform:    db.ChannelInstallation{Config: newTestConfig(t)},
	}
}

// TestReplier_Reply_SendsVerdictReply_AndDecodesPlatformMessageID covers the
// happy path: Reply posts one message into the originating channel, quoting
// the triggering message, and the server's response (including the
// platform-assigned message id) decodes cleanly — the same discordAPI
// request/response shape outbound.go itself relies on for delivery.
func TestReplier_Reply_SendsVerdictReply_AndDecodesPlatformMessageID(t *testing.T) {
	srv, got := newReplierTestServer(t, nil)
	r := NewOutboundReplier(OutboundReplierConfig{
		Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{
		MessageID: "trigger-msg-1",
		Source:    channel.Source{ChatID: "chan-verdict", ChatType: channel.ChatTypeP2P, SenderID: "discord-user"},
	}

	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeAgentOffline})

	if got.Content != msgAgentOffline {
		t.Fatalf("content = %q, want %q", got.Content, msgAgentOffline)
	}
	if got.MessageReference == nil || got.MessageReference.MessageID != "trigger-msg-1" {
		t.Fatalf("expected the reply to quote the triggering message, got %#v", got.MessageReference)
	}
}

func TestReplier_NeedsBinding_GroupChatGetsHintNotBearerLink(t *testing.T) {
	srv, got := newReplierTestServer(t, nil)
	minter := &fakeDiscordBindingMinter{raw: "secret-token"}
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding: minter, Decrypt: nil, AppURL: "https://multica.example",
		APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{Source: channel.Source{
		ChatID: "chan-group", ChatType: channel.ChatTypeGroup, SenderID: "discord-user",
	}}

	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeNeedsBinding, Sender: "discord-user"})

	if minter.calls != 0 {
		t.Fatalf("Mint called %d times for a group prompt", minter.calls)
	}
	if !strings.Contains(got.Content, msgBindingGroupHint) {
		t.Fatalf("group prompt = %q, want %q", got.Content, msgBindingGroupHint)
	}
	if strings.Contains(got.Content, "secret-token") {
		t.Fatalf("group prompt exposed a redeem link: %q", got.Content)
	}
}

func TestReplier_NeedsBinding_DirectMessageMintsBearerLink(t *testing.T) {
	srv, got := newReplierTestServer(t, nil)
	minter := &fakeDiscordBindingMinter{raw: "secret-token"}
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding: minter, Decrypt: nil, AppURL: "https://multica.example",
		APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{Source: channel.Source{
		ChatID: "chan-dm", ChatType: channel.ChatTypeP2P, SenderID: "discord-user",
	}}

	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeNeedsBinding, Sender: "discord-user"})

	if minter.calls != 1 {
		t.Fatalf("Mint called %d times, want 1", minter.calls)
	}
	if !strings.Contains(got.Content, "secret-token") {
		t.Fatalf("dm prompt missing the redeem token: %q", got.Content)
	}
	if !strings.Contains(got.Content, "https://multica.example/discord/bind") {
		t.Fatalf("dm prompt missing the bind URL: %q", got.Content)
	}
}

func TestReplier_NeedsBinding_WithoutBindingConfigured_DoesNotPanic(t *testing.T) {
	srv, got := newReplierTestServer(t, nil)
	r := NewOutboundReplier(OutboundReplierConfig{
		Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{Source: channel.Source{
		ChatID: "chan-dm2", ChatType: channel.ChatTypeP2P, SenderID: "discord-user",
	}}

	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeNeedsBinding, Sender: "discord-user"})

	if got.Content != "" {
		t.Fatalf("expected no message posted when Binding is unconfigured, got %q", got.Content)
	}
}

func TestReplier_Ingested_IssueCreatedAndDuplicate(t *testing.T) {
	t.Run("created", func(t *testing.T) {
		srv, got := newReplierTestServer(t, nil)
		r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
		inst := replierTestInstallation(t)
		msg := channel.InboundMessage{Source: channel.Source{ChatID: "chan-issue", ChatType: channel.ChatTypeGroup}}

		r.Reply(context.Background(), inst, msg, engine.Result{
			Outcome: engine.OutcomeIngested, IssueID: dbidTestUUID(40),
			IssueIdentifier: "MUL-100", IssueTitle: "Fix the bug",
		})
		if !strings.Contains(got.Content, "MUL-100") || !strings.Contains(got.Content, "Fix the bug") {
			t.Fatalf("created text = %q", got.Content)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		srv, got := newReplierTestServer(t, nil)
		r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
		inst := replierTestInstallation(t)
		msg := channel.InboundMessage{Source: channel.Source{ChatID: "chan-issue2", ChatType: channel.ChatTypeGroup}}

		r.Reply(context.Background(), inst, msg, engine.Result{
			Outcome: engine.OutcomeIngested, IssueID: dbidTestUUID(41),
			IssueIdentifier: "MUL-101", IssueDuplicate: true,
		})
		if !strings.Contains(got.Content, "already exists") {
			t.Fatalf("duplicate text = %q", got.Content)
		}
	})
}

func TestReplier_Dropped_NonWorkspaceMemberOnAddressedIssueCommand(t *testing.T) {
	srv, got := newReplierTestServer(t, nil)
	r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{
		AddressedToBot: true,
		CommandText:    "/issue Fix the bug",
		Source:         channel.Source{ChatID: "chan-drop", ChatType: channel.ChatTypeGroup},
	}

	r.Reply(context.Background(), inst, msg, engine.Result{
		Outcome: engine.OutcomeDropped, DropReason: engine.DropReasonNonWorkspaceMember,
	})

	if got.Content != msgIssueNotMember {
		t.Fatalf("content = %q, want %q", got.Content, msgIssueNotMember)
	}
}

func TestReplier_Reply_MissingPlatformRow_DoesNotPanic(t *testing.T) {
	srv, got := newReplierTestServer(t, nil)
	r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
	inst := engine.ResolvedInstallation{ID: dbidTestUUID(32)} // Platform left nil
	msg := channel.InboundMessage{Source: channel.Source{ChatID: "chan-x", ChatType: channel.ChatTypeP2P}}

	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeAgentOffline})

	if got.Content != "" {
		t.Fatalf("expected no message posted without a platform row, got %q", got.Content)
	}
}

// ---- control-ack outbound ledger (T7) ----
//
// Canonical layer for acceptance criterion (c): the replier path's
// outbound_kind values must match Slack's own kind values
// (slack/replier.go:169-172: "control_ack" / "issue_ack").

func TestReplier_Post_RecordsControlAckLedgerRow(t *testing.T) {
	srv, got := newReplierTestServer(t, func(discordMessagePayload) (int, discordMessageResponse) {
		return http.StatusOK, discordMessageResponse{ID: "msg-ack-1"}
	})
	r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
	ledger := &fakeControlAckLedger{}
	r.ledger = ledger
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{Source: channel.Source{ChatID: "chan-ack", ChatType: channel.ChatTypeP2P}}
	res := engine.Result{
		Outcome:              engine.OutcomeAgentOffline,
		ChannelBindingID:     dbidTestUUID(60),
		ChannelRouteRevision: 9,
	}

	if err := r.post(context.Background(), inst, msg, res, "hi"); err != nil {
		t.Fatalf("post returned %v", err)
	}
	if got.Content != "hi" {
		t.Fatalf("posted content = %q, want %q", got.Content, "hi")
	}

	recorded := ledger.snapshot()
	if len(recorded) != 1 {
		t.Fatalf("expected exactly 1 ledger row, got %d: %#v", len(recorded), recorded)
	}
	row := recorded[0]
	if row.OutboundMessageID != "msg-ack-1" {
		t.Fatalf("message id = %q, want %q", row.OutboundMessageID, "msg-ack-1")
	}
	if row.OutboundInstallationID != inst.ID {
		t.Fatalf("installation id = %v, want %v", row.OutboundInstallationID, inst.ID)
	}
	if row.OutboundChannelType != string(TypeDiscord) {
		t.Fatalf("channel type = %q, want %q", row.OutboundChannelType, TypeDiscord)
	}
	if row.OutboundBindingID != res.ChannelBindingID {
		t.Fatalf("binding id = %v, want %v", row.OutboundBindingID, res.ChannelBindingID)
	}
	if row.OutboundRouteRevision != res.ChannelRouteRevision {
		t.Fatalf("route revision = %d, want %d", row.OutboundRouteRevision, res.ChannelRouteRevision)
	}
	if row.OutboundKind != "control_ack" {
		t.Fatalf("kind = %q, want %q (matching slack's own kind value)", row.OutboundKind, "control_ack")
	}
}

func TestReplier_Post_IssueAck_KindMatchesSlack(t *testing.T) {
	srv, _ := newReplierTestServer(t, func(discordMessagePayload) (int, discordMessageResponse) {
		return http.StatusOK, discordMessageResponse{ID: "msg-ack-2"}
	})
	r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
	ledger := &fakeControlAckLedger{}
	r.ledger = ledger
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{Source: channel.Source{ChatID: "chan-issue-ack", ChatType: channel.ChatTypeGroup}}
	res := engine.Result{
		Outcome: engine.OutcomeIngested, IssueID: dbidTestUUID(61),
		ChannelBindingID: dbidTestUUID(62), ChannelRouteRevision: 2,
	}

	if err := r.post(context.Background(), inst, msg, res, "created"); err != nil {
		t.Fatalf("post returned %v", err)
	}

	recorded := ledger.snapshot()
	if len(recorded) != 1 || recorded[0].OutboundKind != "issue_ack" {
		t.Fatalf("recorded = %#v, want exactly 1 row with kind %q", recorded, "issue_ack")
	}
}

// TestReplier_Post_LedgerWriteFailureDoesNotFailReply is the T7 amendment's
// binding requirement applied to the replier path: post already delivered
// the verdict reply via api.CreateMessage by the time the ledger write runs,
// so a ledger failure must be logged and swallowed, never turned into a
// reported reply failure (Reply's only signal that a reply failed is post
// returning a non-nil error).
func TestReplier_Post_LedgerWriteFailureDoesNotFailReply(t *testing.T) {
	srv, got := newReplierTestServer(t, func(discordMessagePayload) (int, discordMessageResponse) {
		return http.StatusOK, discordMessageResponse{ID: "msg-ack-3"}
	})
	r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
	ledger := &fakeControlAckLedger{errOn: "msg-ack-3", err: errors.New("ledger insert exploded")}
	r.ledger = ledger
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{Source: channel.Source{ChatID: "chan-ack-fail", ChatType: channel.ChatTypeP2P}}
	res := engine.Result{
		Outcome:              engine.OutcomeAgentOffline,
		ChannelBindingID:     dbidTestUUID(63),
		ChannelRouteRevision: 1,
	}

	err := r.post(context.Background(), inst, msg, res, "still delivered")
	if err != nil {
		t.Fatalf("post returned %v, want nil: a ledger-write failure must not turn an already-delivered reply into a reported failure", err)
	}
	if got.Content != "still delivered" {
		t.Fatalf("posted content = %q, want %q — delivery must not be gated on the ledger write", got.Content, "still delivered")
	}
	if len(ledger.snapshot()) != 1 {
		t.Fatalf("expected the ledger write to still be attempted once despite the injected failure, got %d", len(ledger.snapshot()))
	}
}

// TestReplier_Post_NoLedgerConfigured_DoesNotPanic guards the nil-safety fix
// documented on NewOutboundReplier: an unconfigured Queries (the production
// default until router.go's discord.NewOutboundReplier call site is wired,
// which is out of this change's file scope) must degrade to skipping the
// ledger write, not panic on a typed-nil interface.
func TestReplier_Post_NoLedgerConfigured_DoesNotPanic(t *testing.T) {
	srv, got := newReplierTestServer(t, nil)
	r := NewOutboundReplier(OutboundReplierConfig{Decrypt: nil, APIBase: srv.URL, HTTPClient: srv.Client()})
	inst := replierTestInstallation(t)
	msg := channel.InboundMessage{Source: channel.Source{ChatID: "chan-no-ledger", ChatType: channel.ChatTypeP2P}}
	res := engine.Result{Outcome: engine.OutcomeAgentOffline, ChannelBindingID: dbidTestUUID(64), ChannelRouteRevision: 1}

	if err := r.post(context.Background(), inst, msg, res, "no ledger configured"); err != nil {
		t.Fatalf("post returned %v", err)
	}
	if got.Content != "no ledger configured" {
		t.Fatalf("posted content = %q", got.Content)
	}
}
