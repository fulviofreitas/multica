package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// newTestSender builds a sender pointed at ts, mirroring how
// discordChannel.Send constructs one in discord_channel.go.
func newTestSender(ts *httptest.Server) *sender {
	api := newDiscordAPI(ts.URL, "test-bot-token", ts.Client())
	return newSender(api, nil)
}

func TestSender_Send_ReturnsPlatformMessageID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(discordMessageResponse{ID: "msg-abc-123"})
	}))
	defer ts.Close()

	s := newTestSender(ts)
	result, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID: "chan-1",
		Text:   "hello world",
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if result.MessageID != "msg-abc-123" {
		t.Fatalf("MessageID = %q, want %q", result.MessageID, "msg-abc-123")
	}
	if len(result.MessageIDs) != 1 || result.MessageIDs[0] != "msg-abc-123" {
		t.Fatalf("MessageIDs = %#v, want [msg-abc-123]", result.MessageIDs)
	}
}

func TestSender_Send_OnlyFirstChunkQuotes(t *testing.T) {
	var mu atomicPayloads
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p discordMessagePayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.add(p)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(discordMessageResponse{ID: fmt.Sprintf("msg-%d", mu.len())})
	}))
	defer ts.Close()

	s := newTestSender(ts)
	longText := strings.Repeat("word ", 1000) // forces multiple chunks
	_, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID:  "chan-1",
		Text:    longText,
		ReplyTo: "parent-msg-1",
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	posted := mu.snapshot()
	if len(posted) < 2 {
		t.Fatalf("expected multiple chunks to be posted, got %d", len(posted))
	}
	if posted[0].MessageReference == nil || posted[0].MessageReference.MessageID != "parent-msg-1" {
		t.Fatalf("first chunk did not carry the reply reference: %#v", posted[0])
	}
	for i, p := range posted[1:] {
		if p.MessageReference != nil {
			t.Fatalf("chunk %d unexpectedly carried a reply reference: %#v", i+1, p)
		}
	}
}

// atomicPayloads collects posted payloads across concurrent-safe access
// (the httptest handler runs on its own goroutine per request, though this
// package's sender posts sequentially — kept simple and safe regardless).
type atomicPayloads struct {
	items []discordMessagePayload
}

func (a *atomicPayloads) add(p discordMessagePayload)       { a.items = append(a.items, p) }
func (a *atomicPayloads) len() int                          { return len(a.items) }
func (a *atomicPayloads) snapshot() []discordMessagePayload { return a.items }

func TestSender_Send_429WithRetryAfterThenSucceeds(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"retry_after": 0.01})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(discordMessageResponse{ID: "msg-ok"})
	}))
	defer ts.Close()

	s := newTestSender(ts)
	start := time.Now()
	result, err := s.Send(context.Background(), channel.OutboundMessage{ChatID: "chan-1", Text: "hi"})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if result.MessageID != "msg-ok" {
		t.Fatalf("MessageID = %q, want msg-ok", result.MessageID)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts (1 rate-limited + 1 success), got %d", attempts)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("retry did not actually wait before succeeding: elapsed %v", elapsed)
	}
}

func TestSender_Send_Repeated429EventuallyReturnsError(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "0.001")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"retry_after": 0.001})
	}))
	defer ts.Close()

	s := newTestSender(ts)
	_, err := s.Send(context.Background(), channel.OutboundMessage{ChatID: "chan-1", Text: "hi"})
	if err == nil {
		t.Fatal("expected an error after repeated 429s, got nil (message must never silently drop OR loop forever)")
	}
	if got := atomic.LoadInt32(&attempts); got != maxSendAttempts {
		t.Fatalf("expected exactly %d bounded attempts, got %d", maxSendAttempts, got)
	}
}

func TestSender_Send_5xxRetried(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"internal error"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(discordMessageResponse{ID: "msg-after-5xx"})
	}))
	defer ts.Close()

	s := newTestSender(ts)
	result, err := s.Send(context.Background(), channel.OutboundMessage{ChatID: "chan-1", Text: "hi"})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if result.MessageID != "msg-after-5xx" {
		t.Fatalf("MessageID = %q, want msg-after-5xx", result.MessageID)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("expected 3 attempts (2 failures + 1 success), got %d", attempts)
	}
}

func TestSender_Send_400IsFatalNotRetried(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad request"}`))
	}))
	defer ts.Close()

	s := newTestSender(ts)
	_, err := s.Send(context.Background(), channel.OutboundMessage{ChatID: "chan-1", Text: "hi"})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected exactly 1 attempt for a fatal 400 (no retry), got %d", got)
	}
}

func TestSender_Send_403IsFatalNotRetried(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"missing permissions"}`))
	}))
	defer ts.Close()

	s := newTestSender(ts)
	_, err := s.Send(context.Background(), channel.OutboundMessage{ChatID: "chan-1", Text: "hi"})
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected exactly 1 attempt for a fatal 403 (no retry), got %d", got)
	}
}

// TestSend_NoGatewayConnectionRequired guards the WeCom bug class
// (multica-ai/multica#7215, see sender.go's package doc): a discordChannel
// built with NO Gateway connection, NO resume cache and NO reconnector
// wired (every one of those fields left at its Go zero value) must still be
// able to deliver a reply, because Send is REST-only and must never read
// process-local connection state. If a future change makes Send depend on
// c.resumeCache/c.reconnector/anything Gateway-shaped, this test panics on
// a nil-pointer dereference or otherwise fails, exactly the failure mode
// that made WeCom's outbound replica-dependent.
func TestSend_NoGatewayConnectionRequired(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(discordMessageResponse{ID: "msg-no-gateway"})
	}))
	defer ts.Close()

	// Deliberately construct discordChannel with ONLY the fields Send needs
	// (botToken, logger, restAPIBase) — resumeCache, reconnector,
	// gatewayCfg, gatewayURL and onMessageCreate are all left nil/zero,
	// simulating a replica that has never dialed this installation's
	// Gateway connection at all.
	c := &discordChannel{
		botToken:    "test-bot-token",
		logger:      nil,
		restAPIBase: ts.URL,
		restClient:  ts.Client(),
	}

	result, err := c.Send(context.Background(), channel.OutboundMessage{
		ChatID: "chan-1",
		Text:   "delivered with no gateway connection",
	})
	if err != nil {
		t.Fatalf("Send failed on a channel with no Gateway state: %v", err)
	}
	if result.MessageID != "msg-no-gateway" {
		t.Fatalf("MessageID = %q, want msg-no-gateway", result.MessageID)
	}
}

// TestSender_Send_WhitespaceOnlyUnderBudgetTextDeliversNothing covers Gap 2
// at the Send call site: a short whitespace-only body takes chunkMessage's
// early-return path (it fits under maxMessageChars in one piece). Before
// the chunk.go fix, that path returned the raw text unfiltered, and Send
// posted it to Discord verbatim — a guaranteed 400. Send must now treat
// "nothing deliverable" as a legitimate empty success, not synthesize an
// empty chunk to send (the len(chunks) == 0 fallback this pins the removal
// of).
func TestSender_Send_WhitespaceOnlyUnderBudgetTextDeliversNothing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not call Discord for a whitespace-only body")
	}))
	defer ts.Close()

	s := newTestSender(ts)
	result, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID: "chan-1",
		Text:   "\n\n\n",
	})
	if err != nil {
		t.Fatalf("Send returned an error for a legitimate empty reply: %v", err)
	}
	if result.MessageID != "" || len(result.MessageIDs) != 0 {
		t.Fatalf("result = %#v, want a zero-value SendResult (nothing was delivered)", result)
	}
}

// TestSender_Send_AllBlankOverBudgetTextDeliversNothing covers Gap 1 at the
// Send call site: text long enough to force chunkMessage's splitting loop,
// entirely blank, so every raw split piece is empty/whitespace-only and
// dropBlankChunks removes them all, driving chunkMessage's result to
// len == 0. Before the call-site fix, Send's own
// `if len(chunks) == 0 { chunks = []string{""} }` fallback resurrected a
// single empty chunk and posted it — the bug simply moved from chunkMessage
// into the caller that was supposed to consume its guarantee.
func TestSender_Send_AllBlankOverBudgetTextDeliversNothing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not call Discord for an all-blank over-budget body")
	}))
	defer ts.Close()

	s := newTestSender(ts)
	result, err := s.Send(context.Background(), channel.OutboundMessage{
		ChatID: "chan-1",
		Text:   strings.Repeat("\n", 3000),
	})
	if err != nil {
		t.Fatalf("Send returned an error for a legitimate empty reply: %v", err)
	}
	if result.MessageID != "" || len(result.MessageIDs) != 0 {
		t.Fatalf("result = %#v, want a zero-value SendResult (nothing was delivered)", result)
	}
}

func TestSender_Send_MissingChannelID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not call Discord when the destination channel id is missing")
	}))
	defer ts.Close()

	s := newTestSender(ts)
	_, err := s.Send(context.Background(), channel.OutboundMessage{Text: "hi"})
	if err == nil {
		t.Fatal("expected an error for a missing channel id")
	}
}
