package discord

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A realistic-shaped (but fake) Discord bot token: three dot-separated
// base64url segments.
const wellFormedToken = "EXAMPLE-NOT-A-REAL-TOKEN.TESTCC.222222222222222222222222222"

func TestValidateTokenShape(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "well-formed token passes", token: wellFormedToken, wantErr: false},
		{name: "empty string", token: "", wantErr: true},
		{name: "whitespace only", token: "   \t\n", wantErr: true},
		{name: "telegram-shaped token (numeric:secret)", token: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11", wantErr: true},
		{name: "missing segments", token: "onlyonesegment", wantErr: true},
		{name: "only two segments", token: "abcdef.ghijklm", wantErr: true},
		{name: "contains invalid characters", token: "abcdef!.ghijklmn.abcdefghijklmnopqrstuvwx", wantErr: true},
		{name: "empty middle segment", token: "abcdefgh..abcdefghijklmnopqrstuvwx", wantErr: true},
		{name: "trailing/leading whitespace around otherwise valid token", token: "  " + wellFormedToken + "  ", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTokenShape(tt.token)
			if tt.wantErr && !errors.Is(err, ErrInvalidBotToken) {
				t.Fatalf("validateTokenShape(%q) = %v, want ErrInvalidBotToken", tt.token, err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateTokenShape(%q) = %v, want nil", tt.token, err)
			}
		})
	}
}

func TestCredentialVerificationClientHasBoundedTimeout(t *testing.T) {
	client := newCredentialVerificationClient()
	if client.Timeout != 15*time.Second {
		t.Fatalf("verification timeout = %s, want 15s", client.Timeout)
	}
}

// jsonHandler builds a fixed-status, fixed-body httptest handler.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestVerifyBotToken(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantErr    error
		wantAppID  string
		wantName   string
		closeSrv   bool // simulate a fully unreachable server
		wantOKBool bool // true when no error is expected
	}{
		{
			name:       "200 with a valid bot user",
			handler:    jsonHandler(http.StatusOK, `{"id":"1058421234567890123","username":"multibot","bot":true}`),
			wantOKBool: true,
			wantAppID:  "1058421234567890123",
			wantName:   "multibot",
		},
		{
			name:    "200 but the identity is not a bot",
			handler: jsonHandler(http.StatusOK, `{"id":"999","username":"a_human","bot":false}`),
			wantErr: ErrCredentialsRejected,
		},
		{
			name:    "401 unauthorized",
			handler: jsonHandler(http.StatusUnauthorized, `{"message":"401: Unauthorized","code":0}`),
			wantErr: ErrCredentialsRejected,
		},
		{
			name:    "403 forbidden is unverifiable, not rejected",
			handler: jsonHandler(http.StatusForbidden, `{"message":"403: Forbidden","code":0}`),
			wantErr: ErrCredentialsUnverifiable,
		},
		{
			name:    "500 internal server error",
			handler: jsonHandler(http.StatusInternalServerError, `{"message":"internal error"}`),
			wantErr: ErrCredentialsUnverifiable,
		},
		{
			name:    "502 bad gateway",
			handler: jsonHandler(http.StatusBadGateway, `bad gateway`),
			wantErr: ErrCredentialsUnverifiable,
		},
		{
			name:    "429 too many requests",
			handler: jsonHandler(http.StatusTooManyRequests, `{"message":"You are being rate limited.","retry_after":0.5,"global":false}`),
			wantErr: ErrCredentialsUnverifiable,
		},
		{
			name:    "200 with malformed/truncated JSON body",
			handler: jsonHandler(http.StatusOK, `{"id":"123","username":"multibot","bot":tr`),
			wantErr: ErrCredentialsUnverifiable,
		},
		{
			name:     "network failure (server closed)",
			closeSrv: true,
			wantErr:  ErrCredentialsUnverifiable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var base string
			if tt.closeSrv {
				srv := httptest.NewServer(jsonHandler(http.StatusOK, `{}`))
				base = srv.URL
				srv.Close() // closed before use: connection refused
			} else {
				srv := httptest.NewServer(tt.handler)
				defer srv.Close()
				base = srv.URL
			}

			got, err := VerifyBotToken(t.Context(), wellFormedToken, base, nil)

			if tt.wantOKBool {
				if err != nil {
					t.Fatalf("VerifyBotToken() unexpected error: %v", err)
				}
				if got.AppID != tt.wantAppID {
					t.Errorf("AppID = %q, want %q", got.AppID, tt.wantAppID)
				}
				if got.Username != tt.wantName {
					t.Errorf("Username = %q, want %q", got.Username, tt.wantName)
				}
				return
			}

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("VerifyBotToken() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyBotToken_RejectsMalformedTokenWithoutNetworkCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := VerifyBotToken(t.Context(), "not-a-token", srv.URL, nil)
	if !errors.Is(err, ErrInvalidBotToken) {
		t.Fatalf("VerifyBotToken() error = %v, want ErrInvalidBotToken", err)
	}
	if called {
		t.Fatal("VerifyBotToken() made a live API call for a shape-invalid token")
	}
}

func TestClassifyCredentialVerificationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "Discord rejects the token (401)",
			err:  &apiError{Code: http.StatusUnauthorized, Body: "Unauthorized"},
			want: ErrCredentialsRejected,
		},
		{
			name: "Discord forbids the request (403) is unverifiable",
			err:  &apiError{Code: http.StatusForbidden, Body: "Forbidden"},
			want: ErrCredentialsUnverifiable,
		},
		{
			name: "Discord rate limits verification (429)",
			err:  &apiError{Code: http.StatusTooManyRequests, Body: "rate limited", RetryAfter: 1.5},
			want: ErrCredentialsUnverifiable,
		},
		{
			name: "Discord 5xx",
			err:  &apiError{Code: http.StatusServiceUnavailable, Body: "unavailable"},
			want: ErrCredentialsUnverifiable,
		},
		{
			name: "transport cannot reach Discord",
			err:  &requestError{cause: errors.New("connection refused")},
			want: ErrCredentialsUnverifiable,
		},
		{
			name: "malformed upstream response",
			err:  errors.New("decode response"),
			want: ErrCredentialsUnverifiable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := classifyCredentialVerificationError(tt.err); !errors.Is(err, tt.want) {
				t.Fatalf("classifyCredentialVerificationError() = %v, want %v", err, tt.want)
			}
		})
	}
}
