package discord

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkMessage_UnderLimitSingleChunk(t *testing.T) {
	in := "short message"
	got := chunkMessage(in, maxMessageChars)
	if len(got) != 1 || got[0] != in {
		t.Fatalf("chunkMessage(%q) = %#v, want single unchanged chunk", in, got)
	}
}

func TestChunkMessage_OverLimitSplitsOnNewline(t *testing.T) {
	// Two paragraphs, each individually short, separated by a newline. The
	// combined text exceeds a small limit and must split AT the newline
	// rather than mid-paragraph.
	para1 := strings.Repeat("a", 50)
	para2 := strings.Repeat("b", 50)
	in := para1 + "\n" + para2

	got := chunkMessage(in, 60)
	if len(got) != 2 {
		t.Fatalf("chunkMessage produced %d chunks, want 2: %#v", len(got), got)
	}
	if got[0] != para1 {
		t.Fatalf("first chunk = %q, want %q", got[0], para1)
	}
	if got[1] != para2 {
		t.Fatalf("second chunk = %q, want %q", got[1], para2)
	}
}

func TestChunkMessage_NoNewlineLongText(t *testing.T) {
	// One long run of words with no newline: must still split (on
	// whitespace, per chunkMessage's documented fallback) rather than
	// exceeding the limit, and reassembling every chunk must reproduce the
	// original content losslessly modulo the split whitespace.
	words := make([]string, 40)
	for i := range words {
		words[i] = "word"
	}
	in := strings.Join(words, " ")

	got := chunkMessage(in, 30)
	if len(got) < 2 {
		t.Fatalf("expected the long text to split into multiple chunks, got %d", len(got))
	}
	for _, c := range got {
		if utf16Units(c) > 30 {
			t.Fatalf("chunk %q exceeds the 30-unit limit (%d units)", c, utf16Units(c))
		}
	}
	rejoined := strings.Join(got, "")
	if rejoined != in {
		t.Fatalf("chunking lost content: got %q, want %q", rejoined, in)
	}
}

func TestChunkMessage_NeverSplitsMidRune(t *testing.T) {
	// Build text out of a 4-byte, astral-plane emoji (2 UTF-16 units each)
	// so a byte-oriented or naive UTF-16-oblivious splitter would slice
	// through the middle of one.
	emoji := "😀" // U+1F600, 2 UTF-16 code units
	in := strings.Repeat(emoji, 30)

	got := chunkMessage(in, 10)
	for _, c := range got {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk is not valid UTF-8, a rune was split: %q", c)
		}
		// Every emoji in the reconstructed chunk must be the same code
		// point, i.e. no partial surrogate/rune survived.
		for _, r := range c {
			if r != '😀' {
				t.Fatalf("chunk contains a corrupted rune %q in %q", r, c)
			}
		}
	}
	if strings.Join(got, "") != in {
		t.Fatalf("rejoined chunks do not reproduce the original text")
	}
}

func TestChunkMessage_OnlyFirstChunkQuotesInSender(t *testing.T) {
	// chunkMessage itself has no notion of quoting — that behavior lives in
	// sender.Send (see sender_test.go's chunking/reply tests). This test
	// documents the pointer: chunk boundaries here must be stable so
	// sender.Send's "replyTo reset after first chunk" logic operates on a
	// predictable chunk count.
	para1 := strings.Repeat("a", 50)
	para2 := strings.Repeat("b", 50)
	got := chunkMessage(para1+"\n"+para2, 60)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(got))
	}
}

func TestChunkMessage_CodeFenceSpanningSplitProducesBalancedFences(t *testing.T) {
	// A single fenced code block whose body is long enough that the split
	// must land inside it. Every resulting chunk must have an EVEN number
	// of ``` fence markers (i.e. no unterminated fence), and every chunk
	// that contains code content must be wrapped in its own complete fence
	// pair.
	var body strings.Builder
	for i := 0; i < 40; i++ {
		body.WriteString("line of code number ")
		body.WriteString(strings.Repeat("x", 5))
		body.WriteString("\n")
	}
	in := "```go\n" + body.String() + "```"

	got := chunkMessage(in, 80)
	if len(got) < 2 {
		t.Fatalf("expected the code block to be split into multiple chunks, got %d", len(got))
	}
	for i, c := range got {
		fenceCount := strings.Count(c, "```")
		if fenceCount%2 != 0 {
			t.Fatalf("chunk %d has an unterminated fence (%d markers): %q", i, fenceCount, c)
		}
	}
	// Reassembling every chunk's non-fence-marker lines must reproduce the
	// original code content, i.e. no lines were dropped by the rebalancing.
	var reassembled strings.Builder
	for _, c := range got {
		for _, line := range strings.Split(c, "\n") {
			if strings.TrimSpace(line) == "```" || strings.HasPrefix(strings.TrimSpace(line), "```go") {
				continue
			}
			if line == "" {
				continue
			}
			reassembled.WriteString(line)
			reassembled.WriteString("\n")
		}
	}
	if reassembled.String() != body.String() {
		t.Fatalf("code content was altered by fence rebalancing:\ngot:  %q\nwant: %q", reassembled.String(), body.String())
	}
}

func TestChunkMessage_CodeFenceSpanningSplitReopensWithSameLanguage(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 40; i++ {
		body.WriteString("x = ")
		body.WriteString(strings.Repeat("y", 8))
		body.WriteString("\n")
	}
	in := "```python\n" + body.String() + "```"

	got := chunkMessage(in, 80)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	// The second (and any later) chunk that continues the code block must
	// reopen with the SAME language tag as the original fence.
	if !strings.HasPrefix(got[1], "```python\n") {
		t.Fatalf("second chunk did not reopen the fence with the original language tag: %q", got[1])
	}
}

func TestChunkMessage_MultipleCompleteFencesAcrossChunksStayBalanced(t *testing.T) {
	// Two SEPARATE, already-complete code blocks, each short enough on its
	// own, but combined they exceed the limit and must split between the
	// two blocks (on the blank line), leaving both intact and balanced.
	block1 := "```\nfirst block\n```"
	block2 := "```\nsecond block\n```"
	in := block1 + "\n\n" + block2

	got := chunkMessage(in, 20)
	for i, c := range got {
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has an unterminated fence: %q", i, c)
		}
	}
}
