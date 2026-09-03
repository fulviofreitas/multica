package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
