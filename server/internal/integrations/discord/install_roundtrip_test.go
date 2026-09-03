package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// TestInstallToChannelRoundTrip covers the seam between the install path and
// the runtime path: PrepareInstall seals the pasted bot token into
// installConfig, and the channel Factory later decrypts that same blob back
// into a live credential. Those two halves encode and decode independently —
// the config JSON is the only contract between them — so a mismatch in the
// base64 alphabet, the field names, or the sealed-value layout would not fail
// either side's own unit tests. It would surface as every installation
// failing to build at supervisor start, long after install reported success.
func TestInstallToChannelRoundTrip(t *testing.T) {
	const botToken = "EXAMPLE-NOT-A-REAL-TOKEN.TESTAA.000000000000000000000000000"

	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bot "+botToken {
			t.Errorf("Authorization header = %q, want the pasted token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1986224834719252480","username":"multica-bot","bot":true}`))
	}))
	defer srv.Close()

	svc, err := NewInstallService(box, srv.Client(), nil)
	if err != nil {
		t.Fatalf("NewInstallService: %v", err)
	}
	svc.apiBase = srv.URL

	prepared, err := svc.PrepareInstall(context.Background(), botToken)
	if err != nil {
		t.Fatalf("PrepareInstall: %v", err)
	}

	// This is what 4.3 will write into channel_installation.config.
	raw, err := json.Marshal(prepared.Config)
	if err != nil {
		t.Fatalf("marshal installConfig: %v", err)
	}

	// And this is what the supervisor does with that row at connect time.
	factory := newDiscordFactory(ChannelDeps{Decrypt: box.Open})
	built, err := factory(channel.Config{Type: TypeDiscord, Raw: raw})
	if err != nil {
		t.Fatalf("factory rejected the config install produced: %v", err)
	}

	dc, ok := built.(*discordChannel)
	if !ok {
		t.Fatalf("factory returned %T, want *discordChannel", built)
	}
	if dc.botToken != botToken {
		t.Errorf("token did not survive the install -> config -> factory round trip")
	}
	if dc.appID != prepared.AppIDKey {
		t.Errorf("appID = %q, want the routing key %q", dc.appID, prepared.AppIDKey)
	}
	if dc.botUsername != "multica-bot" {
		t.Errorf("botUsername = %q, want %q", dc.botUsername, "multica-bot")
	}
}
