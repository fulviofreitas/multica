package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// This file is Task Master subtask 6.1/6.2: the Discord outbound REST
// sender — markdown conversion (markdown.go), message chunking and
// rate-limit handling for POST /channels/{channel_id}/messages.
//
// # Outbound MUST be stateless REST, never the Gateway
//
// This is not a style preference — it is the reason this file exists as
// REST rather than a message pushed down the same Gateway WebSocket
// install.go/typing.go/connect.go already hold open for inbound. WeCom's
// only outbound path is its in-process WebSocket
// (wecom/outbound.go:15-23): EventChatDone is dispatched on the in-process
// event bus, but on a multi-replica deployment the replica that publishes
// that event is not necessarily the replica holding the bot's WS lease, so
// the reply is silently dropped there — this is open bug
// multica-ai/multica#7215. Slack and Lark are immune because their outbound
// is stateless HTTP any replica can perform.
//
// Discord is on the immune side BY CONSTRUCTION: everything in this file —
// discordAPI.CreateMessage, sender.Send, chunkMessage, retryDelay — reaches
// Discord over a plain bounded-timeout *http.Client and reads/decrypts
// nothing but the installation's bot token. None of it references
// discordChannel's Gateway connection, ResumeCache or Reconnector (see
// discord_channel.go), and none of it may ever be changed to do so: any
// replica that can decrypt the bot token can deliver a reply, exactly like
// Slack/Lark and exactly UNLIKE WeCom. TestSend_NoGatewayConnectionRequired
// in sender_test.go guards this by construction.

// maxMessageChars bounds one outbound POST body's content field. Discord's
// documented hard cap is 2000 characters, counted the same UTF-16-code-unit
// way Telegram counts its 4096 limit (see telegram/sender.go's
// maxMessageUnits and utf16Units) — a message built mostly of astral
// characters (many emoji, some CJK-adjacent symbols) can hit the wire limit
// well before naive rune-counting would suggest. 1900 leaves headroom for:
//   - any fence-balancing markers chunkMessage/balanceFences adds when a
//     split falls inside a fenced code block (see chunkFences.go doc below),
//     which are added AFTER the 1900 cut and must still land under 2000;
//   - counting error against Discord's own UTF-16 accounting, which this
//     package has no way to verify byte-for-byte without Discord's source.
const maxMessageChars = 1900

// maxSendAttempts bounds how many times sendWithRetry will call Discord for
// ONE chunk before giving up and returning an error. A 429 or 5xx must never
// cause a message to be silently dropped, but it also must never retry
// forever — 5 attempts, combined with retryDelay's backoff (which honors
// Discord's own Retry-After on a 429, or an exponential 500ms/1s/2s/4s
// schedule capped at 10s otherwise), bounds one chunk's worst case to well
// under a minute while still surviving a single transient blip or a short,
// well-behaved rate-limit window.
const maxSendAttempts = 5

// maxBackoffDelay caps the exponential backoff used for 5xx/transport
// retries (not the Discord-supplied Retry-After on a 429, which is honored
// as-is) so a buggy or malicious backoff exponent can never stall a send for
// an unreasonable amount of wall-clock time.
const maxBackoffDelay = 10 * time.Second

// discordMessagePayload is the POST /channels/{channel_id}/messages request
// body this sender uses. Only the fields Multica's outbound reply needs are
// modeled — richer message features (embeds, components, attachments) are
// out of scope for the cross-platform OutboundMessage envelope (see
// channel/message.go's own doc comment on that boundary).
type discordMessagePayload struct {
	Content string `json:"content"`
	// MessageReference quote-replies to an existing message. Only ever set
	// on the FIRST chunk of a multi-chunk reply — see sender.Send.
	MessageReference *discordMessageReference `json:"message_reference,omitempty"`
}

type discordMessageReference struct {
	MessageID string `json:"message_id"`
}

// discordMessageResponse is the subset of Discord's created Message object
// this sender reads.
type discordMessageResponse struct {
	ID string `json:"id"`
}

// CreateMessage posts one message to channelID via POST
// /channels/{channel_id}/messages, mirroring discordAPI.CurrentUser's
// request/error shape exactly (install.go): a non-2xx response comes back as
// *apiError (with RetryAfter populated on a 429), and a transport failure as
// *requestError.
func (a *discordAPI) CreateMessage(ctx context.Context, channelID string, payload discordMessagePayload) (discordMessageResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return discordMessageResponse{}, fmt.Errorf("discord: encode message payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/channels/"+channelID+"/messages", bytes.NewReader(body))
	if err != nil {
		return discordMessageResponse{}, fmt.Errorf("discord: build create-message request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+a.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return discordMessageResponse{}, &requestError{cause: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return discordMessageResponse{}, fmt.Errorf("discord: read create-message response (http %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &apiError{Code: resp.StatusCode, Body: strings.TrimSpace(string(respBody))}
		if resp.StatusCode == http.StatusTooManyRequests {
			ae.RetryAfter = parseRetryAfter(resp, respBody)
		}
		return discordMessageResponse{}, ae
	}

	var m discordMessageResponse
	if err := json.Unmarshal(respBody, &m); err != nil {
		return discordMessageResponse{}, fmt.Errorf("discord: decode create-message response: %w", err)
	}
	return m, nil
}

// sender posts agent replies to Discord over the REST API. It holds nothing
// but an *discordAPI (bounded-timeout HTTP client + bot token) and a
// logger — see this file's package doc for why that is a hard requirement,
// not an implementation detail.
type sender struct {
	api    *discordAPI
	logger *slog.Logger
}

func newSender(api *discordAPI, logger *slog.Logger) *sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &sender{api: api, logger: logger}
}

// Send delivers out as one or more Discord messages: converts Multica
// markdown to Discord's flavor, splits it into <=maxMessageChars chunks with
// balanced code fences, and posts each chunk in order with bounded 429/5xx
// retry. Only the first chunk carries out.ReplyTo as a quote reference. The
// returned SendResult.MessageIDs holds every posted chunk's id in order;
// MessageID holds the LAST one, mirroring telegram.sender.Send.
func (s *sender) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	if s.api == nil {
		return channel.SendResult{}, errors.New("discord: api client not configured")
	}
	channelID := out.ChatID
	if out.ThreadID != "" {
		// A Discord thread is itself a channel with its own channel_id (see
		// resolvers.go's package doc); when a caller supplies ThreadID
		// explicitly, post into it rather than the parent.
		channelID = out.ThreadID
	}
	if channelID == "" {
		return channel.SendResult{}, errors.New("discord: missing destination channel id")
	}

	chunks := chunkMessage(formatDiscordMarkdown(out.Text), maxMessageChars)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	var ids []string
	replyTo := out.ReplyTo
	for _, chunk := range chunks {
		payload := discordMessagePayload{Content: chunk}
		if replyTo != "" {
			payload.MessageReference = &discordMessageReference{MessageID: replyTo}
		}
		m, err := s.sendWithRetry(ctx, channelID, payload)
		if err != nil {
			return channel.SendResult{MessageIDs: ids}, fmt.Errorf("discord: create message: %w", err)
		}
		ids = append(ids, m.ID)
		replyTo = "" // only the first chunk quotes
	}

	result := channel.SendResult{MessageIDs: ids}
	if len(ids) > 0 {
		result.MessageID = ids[len(ids)-1]
	}
	return result, nil
}

// sendWithRetry posts one chunk, retrying a retryable failure
// (rate-limited, 5xx, or transport) up to maxSendAttempts times. It never
// retries a fatal error (400/401/403/404/anything else 4xx) — those are
// returned to the caller on the first attempt, since a bad request or bad
// permission will never succeed by trying again. A 429 that keeps
// recurring past maxSendAttempts returns an error rather than looping
// forever, so a message can never be silently dropped on the caller's
// behalf: it either eventually sends or the caller learns it did not.
func (s *sender) sendWithRetry(ctx context.Context, channelID string, payload discordMessagePayload) (discordMessageResponse, error) {
	var lastErr error
	for attempt := 0; attempt < maxSendAttempts; attempt++ {
		m, err := s.api.CreateMessage(ctx, channelID, payload)
		if err == nil {
			return m, nil
		}
		lastErr = err

		wait, retryable := retryDelay(err, attempt)
		if !retryable {
			return discordMessageResponse{}, err
		}
		if attempt == maxSendAttempts-1 {
			break
		}
		s.logger.WarnContext(ctx, "discord: send retrying after transient error",
			"attempt", attempt+1, "wait", wait, "error", err)
		if !sleepCtx(ctx, wait) {
			return discordMessageResponse{}, ctx.Err()
		}
	}
	return discordMessageResponse{}, fmt.Errorf("discord: exceeded %d send attempts: %w", maxSendAttempts, lastErr)
}

// retryDelay classifies err into retryable vs fatal and, for a retryable
// error, how long to wait before the next attempt:
//   - HTTP 429: retryable. Waits Discord's own Retry-After (RetryAfter on
//     apiError, populated from the header or JSON body by parseRetryAfter),
//     defaulting to 1 second when Discord did not supply one.
//   - HTTP 5xx: retryable, exponential backoff (backoffDelay).
//   - Any other apiError (400, 401, 403, 404, ...): FATAL, not retried — a
//     malformed request or a permissions/credential problem will not
//     resolve itself by trying again.
//   - A transport failure (*requestError: DNS, connection refused, timeout,
//     context cancellation): retryable, exponential backoff. A lost
//     response can mean Discord already received and processed the
//     request, but unlike Telegram's sendMessage (see
//     telegram/api.go:267's comment on why it does NOT retry transport
//     errors), Discord's message-create is not naturally idempotent from
//     this client's side either way — the product requirement here is that
//     a 429 (or a flaky network) must never silently eat a reply, so this
//     sender retries transport failures too and accepts the small
//     duplicate-message risk over the larger silently-dropped-reply risk.
func retryDelay(err error, attempt int) (time.Duration, bool) {
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.Code == http.StatusTooManyRequests:
			secs := ae.RetryAfter
			if secs <= 0 {
				secs = 1
			}
			return time.Duration(secs * float64(time.Second)), true
		case ae.Code >= 500:
			return backoffDelay(attempt), true
		default:
			return 0, false
		}
	}
	var re *requestError
	if errors.As(err, &re) {
		return backoffDelay(attempt), true
	}
	return 0, false
}

// backoffDelay is the exponential schedule used for 5xx/transport retries:
// 500ms, 1s, 2s, 4s, ..., capped at maxBackoffDelay.
func backoffDelay(attempt int) time.Duration {
	d := 500 * time.Millisecond * time.Duration(1<<uint(attempt))
	if d > maxBackoffDelay {
		d = maxBackoffDelay
	}
	return d
}

// sleepCtx blocks for d or until ctx is done, whichever comes first,
// reporting whether the full wait elapsed. Mirrors
// telegram/telegram_channel.go's sleepCtx (package-local copy: the two
// adapters do not share an internal package for this one helper).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
