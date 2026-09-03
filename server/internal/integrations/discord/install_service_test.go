package discord

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// A token that satisfies discordTokenShape (install.go): three
// dot-separated base64url segments, min lengths 6/4/20.
const testValidBotToken = "EXAMPLE-NOT-A-REAL-TOKEN.TESTBB.111111111111111111111111111"

func testInstallBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func TestNewInstallService_RejectsNilBox(t *testing.T) {
	svc, err := NewInstallService(nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for nil box, got nil")
	}
	if svc != nil {
		t.Fatalf("expected nil service on error, got %+v", svc)
	}
}

func TestNewInstallService_AcceptsBoxOnly(t *testing.T) {
	svc, err := NewInstallService(testInstallBox(t), nil, nil)
	if err != nil {
		t.Fatalf("NewInstallService: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	if svc.httpClient == nil {
		t.Error("expected default http client to be filled in")
	}
	if svc.logger == nil {
		t.Error("expected default logger to be filled in")
	}
}

// discordUsersMeServer returns an httptest server that serves GET
// /users/@me, mirroring install_test.go's fixture shape.
func discordUsersMeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/@me" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestPrepareInstall_HappyPath(t *testing.T) {
	srv := discordUsersMeServer(t, http.StatusOK, `{"id":"98765","username":"multica-bot","bot":true}`)
	defer srv.Close()

	box := testInstallBox(t)
	svc, err := NewInstallService(box, srv.Client(), nil)
	if err != nil {
		t.Fatalf("NewInstallService: %v", err)
	}
	svc.apiBase = srv.URL

	got, err := svc.PrepareInstall(context.Background(), testValidBotToken)
	if err != nil {
		t.Fatalf("PrepareInstall: %v", err)
	}

	if got.Config.AppID != "98765" {
		t.Errorf("AppID = %q, want %q", got.Config.AppID, "98765")
	}
	if got.AppIDKey != "98765" {
		t.Errorf("AppIDKey = %q, want %q", got.AppIDKey, "98765")
	}
	if got.Config.BotUsername != "multica-bot" {
		t.Errorf("BotUsername = %q, want %q", got.Config.BotUsername, "multica-bot")
	}
	if got.Config.BotTokenEncrypted == "" {
		t.Fatal("expected non-empty BotTokenEncrypted")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(got.Config.BotTokenEncrypted)
	if err != nil {
		t.Fatalf("BotTokenEncrypted is not valid base64: %v", err)
	}
	plaintext, err := box.Open(ciphertext)
	if err != nil {
		t.Fatalf("box.Open(BotTokenEncrypted): %v", err)
	}
	if string(plaintext) != testValidBotToken {
		t.Errorf("decrypted token = %q, want %q", string(plaintext), testValidBotToken)
	}
}

func TestPrepareInstall_EncryptedValueIsNotPlaintext(t *testing.T) {
	srv := discordUsersMeServer(t, http.StatusOK, `{"id":"98765","username":"multica-bot","bot":true}`)
	defer srv.Close()

	svc, err := NewInstallService(testInstallBox(t), srv.Client(), nil)
	if err != nil {
		t.Fatalf("NewInstallService: %v", err)
	}
	svc.apiBase = srv.URL

	got, err := svc.PrepareInstall(context.Background(), testValidBotToken)
	if err != nil {
		t.Fatalf("PrepareInstall: %v", err)
	}

	if strings.Contains(got.Config.BotTokenEncrypted, testValidBotToken) {
		t.Fatal("BotTokenEncrypted contains the plaintext token substring")
	}

	raw, err := json.Marshal(got.Config)
	if err != nil {
		t.Fatalf("json.Marshal(Config): %v", err)
	}
	if bytes.Contains(raw, []byte(testValidBotToken)) {
		t.Fatal("marshalled installConfig contains the plaintext token substring")
	}
}

func TestPrepareInstall_ErrorPropagation(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		status     int
		body       string
		wantTarget error
	}{
		{
			name:       "shape invalid",
			token:      "not-a-real-token",
			wantTarget: ErrInvalidBotToken,
		},
		{
			name:       "401 rejected",
			token:      testValidBotToken,
			status:     http.StatusUnauthorized,
			body:       `{"message":"401: Unauthorized","code":0}`,
			wantTarget: ErrCredentialsRejected,
		},
		{
			name:       "500 unverifiable",
			token:      testValidBotToken,
			status:     http.StatusInternalServerError,
			body:       `internal server error`,
			wantTarget: ErrCredentialsUnverifiable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, err := NewInstallService(testInstallBox(t), nil, nil)
			if err != nil {
				t.Fatalf("NewInstallService: %v", err)
			}
			if tt.status != 0 {
				srv := discordUsersMeServer(t, tt.status, tt.body)
				defer srv.Close()
				svc.httpClient = srv.Client()
				svc.apiBase = srv.URL
			}

			_, err = svc.PrepareInstall(context.Background(), tt.token)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tt.wantTarget) {
				t.Fatalf("error = %v, want errors.Is target %v", err, tt.wantTarget)
			}
		})
	}
}

// TestPrepareInstall_NeverLeaksPlaintextToken asserts the plaintext token
// never surfaces in an error message or in anything written through the
// service's logger, across both the shape-invalid path and the
// live-rejection path.
func TestPrepareInstall_NeverLeaksPlaintextToken(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	// A token that is shape-invalid but still contains a canary substring,
	// to prove the canary never round-trips into an error message.
	const canaryToken = "super-secret-canary-token-value"

	svc, err := NewInstallService(testInstallBox(t), nil, logger)
	if err != nil {
		t.Fatalf("NewInstallService: %v", err)
	}
	if _, err := svc.PrepareInstall(context.Background(), canaryToken); err == nil {
		t.Fatal("expected shape error for malformed token")
	} else if strings.Contains(err.Error(), canaryToken) {
		t.Fatalf("error leaked plaintext token: %v", err)
	}

	// Live rejection path, with a valid-shaped token so it reaches the API
	// call rather than failing shape validation.
	srv := discordUsersMeServer(t, http.StatusUnauthorized, `{"message":"401: Unauthorized","code":0}`)
	defer srv.Close()
	svc.httpClient = srv.Client()
	svc.apiBase = srv.URL

	if _, err := svc.PrepareInstall(context.Background(), testValidBotToken); err == nil {
		t.Fatal("expected rejection error")
	} else if strings.Contains(err.Error(), testValidBotToken) {
		t.Fatalf("error leaked plaintext token: %v", err)
	}

	if strings.Contains(logBuf.String(), canaryToken) || strings.Contains(logBuf.String(), testValidBotToken) {
		t.Fatalf("logger captured plaintext token: %s", logBuf.String())
	}
}
