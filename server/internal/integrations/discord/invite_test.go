package discord

import (
	"math"
	"net/url"
	"strings"
	"testing"
)

// TestBuildInviteURL_ExactURL pins the full generated URL, including the
// computed decimal permission mask, so an accidental change to the mask or
// scope fails loudly rather than silently shipping a different permission
// ask.
func TestBuildInviteURL_ExactURL(t *testing.T) {
	got, err := BuildInviteURL("123456789012345678")
	if err != nil {
		t.Fatalf("BuildInviteURL: unexpected error: %v", err)
	}
	want := "https://discord.com/oauth2/authorize?client_id=123456789012345678&permissions=274878024704&scope=bot"
	if got != want {
		t.Fatalf("BuildInviteURL mismatch:\n got:  %s\n want: %s", got, want)
	}
}

// TestBuildInviteURL_NoSlashCommandsScope is a product guarantee: native
// slash commands are deferred out of v1, so the invite must never ask for
// applications.commands.
func TestBuildInviteURL_NoSlashCommandsScope(t *testing.T) {
	got, err := BuildInviteURL("123456789012345678")
	if err != nil {
		t.Fatalf("BuildInviteURL: unexpected error: %v", err)
	}
	if strings.Contains(got, "applications.commands") {
		t.Fatalf("BuildInviteURL must never request applications.commands, got: %s", got)
	}
}

// TestInvitePermissions_MatchesDocumentedBits recomputes the expected mask
// from the literal Discord-documented bit positions, independently of the
// package's named constants, so a future wrong constant in invite.go is
// caught here rather than only in the (also independently-computed)
// exact-URL test above.
func TestInvitePermissions_MatchesDocumentedBits(t *testing.T) {
	const (
		viewChannel           = 1 << 10 // VIEW_CHANNEL
		sendMessages          = 1 << 11 // SEND_MESSAGES
		embedLinks            = 1 << 14 // EMBED_LINKS
		attachFiles           = 1 << 15 // ATTACH_FILES
		readMessageHistory    = 1 << 16 // READ_MESSAGE_HISTORY
		sendMessagesInThreads = 1 << 38 // SEND_MESSAGES_IN_THREADS
	)
	var want uint64 = viewChannel | sendMessages | embedLinks | attachFiles | readMessageHistory | sendMessagesInThreads

	if invitePermissions != want {
		t.Fatalf("invitePermissions = %d, want %d (recomputed from documented bit positions)", invitePermissions, want)
	}
	if want != 274878024704 {
		t.Fatalf("sanity check failed: recomputed mask = %d, want 274878024704", want)
	}
}

// TestInvitePermissions_ExceedsUint32 guards against the mask silently
// truncating: permSendMessagesInThreads (1 << 38) only survives if the mask
// is carried as a 64-bit value end to end.
func TestInvitePermissions_ExceedsUint32(t *testing.T) {
	if invitePermissions <= math.MaxUint32 {
		t.Fatalf("invitePermissions = %d must exceed math.MaxUint32 (%d); a 32-bit mask would truncate SEND_MESSAGES_IN_THREADS (1<<38)", invitePermissions, uint32(math.MaxUint32))
	}
}

func TestBuildInviteURL_EmptyAppID(t *testing.T) {
	_, err := BuildInviteURL("")
	if err == nil {
		t.Fatal("BuildInviteURL(\"\"): expected error, got nil")
	}
}

// TestBuildInviteURL_EscapesHostileAppID ensures an app id containing
// URL-hostile characters (e.g. an attempted query-string injection) is
// percent-escaped as a value rather than being able to inject extra query
// parameters, since app id is untrusted stored config.
func TestBuildInviteURL_EscapesHostileAppID(t *testing.T) {
	hostile := "123&scope=admin"
	got, err := BuildInviteURL(hostile)
	if err != nil {
		t.Fatalf("BuildInviteURL: unexpected error: %v", err)
	}
	if strings.Contains(got, "scope=admin") {
		t.Fatalf("hostile app id injected an extra query parameter: %s", got)
	}
	if !strings.Contains(got, url.QueryEscape(hostile)) {
		t.Fatalf("expected escaped app id %q to appear in URL, got: %s", url.QueryEscape(hostile), got)
	}
	// The URL must still contain exactly one scope=bot, not an injected one.
	if strings.Count(got, "scope=bot") != 1 {
		t.Fatalf("expected exactly one scope=bot, got: %s", got)
	}
}
