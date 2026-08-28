package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file is the Discord bot-token validation and error-classification
// building block: given a pasted bot token, check its shape, call the
// Discord REST API live to confirm it is a real bot credential, and
// classify any failure into "the token is bad" versus "we could not tell".
// Persistence (encryption, the install transaction, the routing-slot
// conflict handling) is a later subtask — see config.go for the stored
// shape (installConfig/credentials) this feeds.

var (
	// ErrInvalidBotToken is returned when the pasted string does not even
	// look like a Discord bot token (three dot-separated base64url
	// segments: "<app id b64>.<timestamp b64>.<hmac b64>"). Mapped to 400
	// by the handler.
	ErrInvalidBotToken = errors.New("discord: bot token must look like <base64>.<base64>.<base64>")
	// ErrCredentialsRejected means Discord itself rejected the token (or
	// the authenticated identity is not a bot). Keep it distinct from an
	// unreachable API so users are never told to rotate a valid credential
	// because the deployment's network or Discord's API is having an
	// outage.
	ErrCredentialsRejected = errors.New("discord: Discord rejected this bot token")
	// ErrCredentialsUnverifiable means Multica could not complete the live
	// Discord check (network failure, 5xx, malformed response, rate
	// limiting, or any other non-authoritative failure). The token has not
	// been persisted or changed, and the user should retry rather than
	// rotate.
	ErrCredentialsUnverifiable = errors.New("discord: could not reach Discord to verify this bot")
)

// discordTokenShape is deliberately loose: Discord has changed its bot
// token format before (the middle segment used to encode a plain base64
// user id, then a differently-shaped timestamp), so this only rejects
// obviously-wrong input (empty, whitespace, missing dots, empty segments,
// or characters outside the base64url alphabet) rather than pinning exact
// segment lengths or contents.
var discordTokenShape = regexp.MustCompile(`^[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{20,}$`)

// validateTokenShape rejects obviously malformed input before making a live
// API call. It intentionally does not validate segment semantics (base64
// decodability of each part, embedded timestamps, etc.) — Discord's exact
// encoding is not a stable contract to pin a regex to.
func validateTokenShape(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("%w: token is empty", ErrInvalidBotToken)
	}
	if !discordTokenShape.MatchString(token) {
		return fmt.Errorf("%w: token is not three dot-separated segments", ErrInvalidBotToken)
	}
	return nil
}

const credentialVerificationTimeout = 15 * time.Second

// newCredentialVerificationClient is the bounded-timeout HTTP client used
// for the live install-time check, mirroring
// telegram.newCredentialVerificationClient. A dedicated (short) timeout
// keeps a hung Discord API call from blocking the install request
// indefinitely.
func newCredentialVerificationClient() *http.Client {
	return &http.Client{Timeout: credentialVerificationTimeout}
}

// defaultAPIBase is the production Discord REST API host (v10). Tests point
// discordAPI.base at an httptest server.
const defaultAPIBase = "https://discord.com/api/v10"

// requestError deliberately omits the request URL from Error(): the URL
// itself never contains the token (Discord auth goes in a header, not the
// path), but keeping the same shape as telegram.requestError lets Unwrap
// preserve the underlying transport error for cancellation checks.
type requestError struct {
	cause error
}

func (e *requestError) Error() string { return "discord: request to Discord API failed" }

func (e *requestError) Unwrap() error { return e.cause }

// apiError is a non-2xx Discord REST API response.
type apiError struct {
	Code int // HTTP status code
	// RetryAfter is Discord's rate-limit backoff (seconds) on a 429, when
	// present. Not currently retried by this package — the caller decides.
	RetryAfter float64
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("discord api: http %d: %s", e.Code, e.Body)
}

// discordAPI is a minimal REST client scoped to what install-time
// validation needs: confirming identity via GET /users/@me.
type discordAPI struct {
	base   string // API host, no trailing slash
	token  string
	client *http.Client
}

func newDiscordAPI(base, token string, client *http.Client) *discordAPI {
	if base == "" {
		base = defaultAPIBase
	}
	if client == nil {
		client = newCredentialVerificationClient()
	}
	return &discordAPI{base: base, token: token, client: client}
}

// discordUser is the subset of Discord's User object the adapter reads.
type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

// rateLimitBody is the JSON shape Discord returns on a 429.
type rateLimitBody struct {
	RetryAfter float64 `json:"retry_after"`
}

// CurrentUser calls GET /users/@me with "Authorization: Bot <token>" and
// returns the authenticated identity. Any non-2xx response is returned as
// *apiError; a transport failure (DNS, connection refused, timeout,
// context cancellation) is returned as *requestError. classifyErr is the
// single place that turns either into ErrCredentialsRejected or
// ErrCredentialsUnverifiable.
func (a *discordAPI) CurrentUser(ctx context.Context) (discordUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/users/@me", nil)
	if err != nil {
		return discordUser{}, fmt.Errorf("discord: build users/@me request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return discordUser{}, &requestError{cause: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return discordUser{}, fmt.Errorf("discord: read users/@me response (http %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiError{Code: resp.StatusCode, Body: strings.TrimSpace(string(body))}
		if resp.StatusCode == http.StatusTooManyRequests {
			ae.RetryAfter = parseRetryAfter(resp, body)
		}
		return discordUser{}, ae
	}

	var u discordUser
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&u); err != nil {
		return discordUser{}, fmt.Errorf("discord: decode users/@me response: %w", err)
	}
	return u, nil
}

// parseRetryAfter prefers Discord's Retry-After header (seconds, may be
// fractional) and falls back to the JSON body's retry_after field.
func parseRetryAfter(resp *http.Response, body []byte) float64 {
	if h := resp.Header.Get("Retry-After"); h != "" {
		if secs, err := strconv.ParseFloat(h, 64); err == nil {
			return secs
		}
	}
	var rl rateLimitBody
	if err := json.Unmarshal(body, &rl); err == nil {
		return rl.RetryAfter
	}
	return 0
}

// classifyCredentialVerificationError separates Discord's authoritative
// credential rejection from failures where no verdict was obtained,
// mirroring telegram.classifyCredentialVerificationError.
//
// Classification:
//   - HTTP 401: the token itself was rejected (invalid or revoked) ->
//     ErrCredentialsRejected.
//   - HTTP 403: NOT treated as a bad token. On /users/@me a 403 in
//     practice means the request was otherwise blocked (e.g. a
//     Cloudflare/WAF response, or an account-level restriction) rather
//     than "this bot token is wrong" — Discord uses 401 for that. Treating
//     403 as rejected risks telling a user to rotate a token that was
//     actually fine. Callers that later observe a Discord-shaped 403 JSON
//     error body indicating an invalid token can special-case it, but by
//     default it falls through to Unverifiable.
//   - HTTP 429: treated as Unverifiable, not Rejected. A rate limit says
//     nothing about whether the token is valid. This package does not
//     retry on the caller's behalf; retry_after is captured on apiError
//     for a caller that wants to honor a short, trivial backoff (e.g. <=
//     a couple of seconds) before giving up and surfacing
//     ErrCredentialsUnverifiable to the user.
//   - Anything else (5xx, malformed JSON, transport/network failure,
//     timeout, unexpected status): ErrCredentialsUnverifiable. A good
//     token must never be reported as bad because infrastructure hiccuped.
func classifyCredentialVerificationError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == http.StatusUnauthorized {
		return fmt.Errorf("%w: %v", ErrCredentialsRejected, err)
	}
	return fmt.Errorf("%w: %v", ErrCredentialsUnverifiable, err)
}

// verifiedBot is the outcome of a successful live check: the caller has
// confirmed the token belongs to a real bot user, along with the fields
// later subtasks persist into installConfig (AppID / BotUsername).
type verifiedBot struct {
	AppID    string // Discord application/bot user id, for the app_id routing slot.
	Username string
}

// VerifyBotToken validates a pasted Discord bot token: shape-checks it,
// then confirms it live against GET /users/@me, and rejects a token whose
// authenticated identity is not a bot account. It does not encrypt or
// persist anything — that is owned by the install service built in a
// later subtask.
//
// base and client are both optional: base defaults to the production
// Discord API host, client defaults to a bounded-timeout http.Client.
// Tests supply an httptest server URL as base.
func VerifyBotToken(ctx context.Context, token string, base string, client *http.Client) (verifiedBot, error) {
	token = strings.TrimSpace(token)
	if err := validateTokenShape(token); err != nil {
		return verifiedBot{}, err
	}
	api := newDiscordAPI(base, token, client)
	me, err := api.CurrentUser(ctx)
	if err != nil {
		return verifiedBot{}, fmt.Errorf("discord users/@me: %w", classifyCredentialVerificationError(err))
	}
	if !me.Bot {
		return verifiedBot{}, fmt.Errorf("%w: response is not a bot account", ErrCredentialsRejected)
	}
	return verifiedBot{AppID: me.ID, Username: me.Username}, nil
}
