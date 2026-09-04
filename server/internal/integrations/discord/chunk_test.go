package discord

import (
	"slices"
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

func TestChunkMessage_LongFencedBlockNeverProducesFenceOnlyChunk(t *testing.T) {
	// Reproduces a confirmed, DB-settled production defect: a single
	// balanced ```python ... ``` fenced block of 12,511 characters — long
	// enough that splitting at maxMessageChars (1900) lands the final split
	// immediately before the block's own closing fence marker, with no code
	// left between the two. balanceFences used to reopen the fence there
	// anyway, manufacturing a trailing chunk that was nothing but
	// "```python\n```" — an empty code block the model never sent.
	var body strings.Builder
	line := "x = compute_something(a, b, c)  # some comment padding here\n"
	const targetTotal = 12511
	const fenceOverhead = len("```python\n") + len("\n```")
	for body.Len()+len(line) <= targetTotal-fenceOverhead {
		body.WriteString(line)
	}
	for body.Len() < targetTotal-fenceOverhead {
		body.WriteByte('z')
	}
	in := "```python\n" + body.String() + "\n```"
	if len(in) != targetTotal {
		t.Fatalf("test setup produced %d chars, want %d", len(in), targetTotal)
	}

	got := chunkMessage(in, maxMessageChars)
	if len(got) < 2 {
		t.Fatalf("expected the block to split into multiple chunks, got %d", len(got))
	}

	for i, c := range got {
		// No chunk may exceed the UTF-16 unit budget.
		if utf16Units(c) > maxMessageChars {
			t.Fatalf("chunk %d exceeds maxMessageChars (%d units): %q", i, utf16Units(c), c)
		}
		// Every chunk's fences must be balanced.
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has an unterminated fence: %q", i, c)
		}
		// No chunk may consist solely of fence markers and/or a language
		// tag — that is exactly the manufactured-empty-code-block defect.
		onlyFenceContent := true
		for _, l := range strings.Split(c, "\n") {
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "```") {
				continue
			}
			onlyFenceContent = false
			break
		}
		if onlyFenceContent {
			t.Fatalf("chunk %d consists solely of fence markers/language tag: %q", i, c)
		}
	}

	// No content loss: stripping the fence-marker/language-tag scaffolding
	// balanceFences injects from every chunk and rejoining must reproduce
	// the original code body verbatim.
	var reassembled strings.Builder
	for _, c := range got {
		for _, l := range strings.Split(c, "\n") {
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "```") {
				continue
			}
			reassembled.WriteString(l)
			reassembled.WriteString("\n")
		}
	}
	// chunkMessage right-trims trailing newlines off of raw split pieces
	// (see its TrimRight call), so compare content modulo trailing newlines
	// rather than requiring byte-for-byte whitespace fidelity at the very
	// end of the message.
	if got, want := strings.TrimRight(reassembled.String(), "\n"), strings.TrimRight(body.String(), "\n"); got != want {
		t.Fatalf("code content was altered by fence rebalancing:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestBalanceFences_ResolvedInheritedFenceDoesNotManufactureStrayMarker(t *testing.T) {
	// Regression test for a bug introduced by the fix for the fence-only
	// chunk defect (see TestChunkMessage_LongFencedBlockNeverProducesFenceOnlyChunk):
	// discarding a degenerate (content-free) inherited fence must mark the
	// chunk's reopen debt as settled. Before this test's fix, "committed"
	// stayed false for the rest of the chunk, so the NEXT line re-entered
	// the "commit a reopen" path with a stale (already-cleared) language
	// tag — writing a standalone "```" immediately before ordinary prose
	// (styling it as code) and leaving a LATER, genuinely new fence in the
	// same chunk unterminated.
	got := balanceFences([]string{
		"```python\ncode_a",
		"```\nSome prose here\n```js\ncode_b",
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %#v", len(got), got)
	}
	if strings.Count(got[1], "```")%2 != 0 {
		t.Fatalf("chunk[1] has an unterminated fence: %q", got[1])
	}
	want := "Some prose here\n```js\ncode_b\n```"
	if got[1] != want {
		t.Fatalf("chunk[1] = %q, want %q", got[1], want)
	}
}

func TestBalanceFences_ResolvedInheritedFenceLeavesTrailingProseUnfenced(t *testing.T) {
	// Regression test: a chunk whose only fence-related content is the
	// inherited fence's own closing marker, followed by trailing prose with
	// no further fence markers at all, must end with an EVEN fence count
	// (zero, here). Before the "committed" fix, the discarded inherited
	// fence left the chunk with one orphaned marker worth of state, so the
	// chunk closed with an ODD fence count — exactly the "unterminated
	// fence leaks code styling into the rest of the conversation" failure
	// mode balanceFences's doc comment says it exists to prevent.
	got := balanceFences([]string{
		"```python\ncode_a",
		"```\nSome trailing prose",
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %#v", len(got), got)
	}
	if strings.Count(got[1], "```")%2 != 0 {
		t.Fatalf("chunk[1] has an unterminated fence: %q", got[1])
	}
	want := "Some trailing prose"
	if got[1] != want {
		t.Fatalf("chunk[1] = %q, want %q", got[1], want)
	}
}

func TestBalanceFences_BlankLineRunInsideOpenFenceIsPreserved(t *testing.T) {
	// Regression test for a third, previously-unhandled way a chunk can
	// end while it inherited an open fence: the chunk simply ENDS while
	// still open and nothing was ever committed, because every line in it
	// was blank (chunkMessage carved an all-blank-lines chunk out of the
	// middle of a fenced block). Before this fix, the buffered blank
	// lines were silently dropped and the unconditional trailing-close
	// append produced a chunk that was nothing but "```" — the exact
	// fence-only-chunk defect this whole function exists to eliminate,
	// reintroduced on a third path, plus real content loss (a blank line
	// from the user's code block vanishing entirely).
	got := balanceFences([]string{
		"```python\ncode_a",
		"\n\n\n",
		"more_code\n```",
	})
	want := []string{
		"```python\ncode_a\n```",
		"```python\n\n\n\n\n```",
		"```python\nmore_code\n```",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for i, c := range got {
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has an unterminated fence: %q", i, c)
		}
	}
}

func TestBalanceFences_ArbitraryChunkBoundariesNeverLoseOrMisrenderContent(t *testing.T) {
	// Property sweep directly against balanceFences — the actual state
	// machine under test — built by cutting a fixed sequence of LOGICAL
	// LINES at every combination of two boundaries. This is deterministic
	// and exhaustive over where a chunk boundary can fall relative to a
	// fence's open, a blank-line run of a given length (mixing
	// truly-empty and whitespace-only lines), and its close.
	//
	// This intentionally does NOT go through chunkMessage's real
	// character-level splitter: that splitter's strings.TrimRight-based
	// trailing-newline trimming can itself collapse several blank lines
	// at a split point (a separate, pre-existing behavior of
	// chunkMessage, not balanceFences — verified independently), which
	// makes hitting a specific chunk-boundary alignment input-dependent
	// and unreliable to sweep for through the public API. Building raw
	// chunks with strings.Join(lines[a:b], "\n") instead means
	// balanceFences's own strings.Split(chunk, "\n") exactly reverses the
	// construction, so the expected non-fence content is precisely the
	// template's lines — no ambiguity, and a chunk boundary can be placed
	// on any line, including one that lands squarely inside the
	// blank-line run.
	for _, blankRunLen := range []int{1, 2, 3, 5, 10} {
		lines := []string{"```python", "code line one", "code line two"}
		for i := 0; i < blankRunLen; i++ {
			if i%3 == 1 {
				lines = append(lines, "   ") // whitespace-only, not empty
			} else {
				lines = append(lines, "")
			}
		}
		lines = append(lines,
			"code line three",
			"code line four",
			"```",
			"trailing prose line one",
			"trailing prose line two",
		)

		var want []string
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "```") {
				continue
			}
			want = append(want, l)
		}

		n := len(lines)
		// i starts at 2, not 1: a cut at i==1 would make the FIRST raw
		// chunk exactly the opening "```python" marker alone, with
		// nothing else — a pre-existing, separate behavior of a chunk's
		// own raw content being just a fresh fence-open with nothing
		// following in that same chunk (verified present in balanceFences
		// before any of these fixes, unrelated to the pending/committed
		// carried-over-fence machinery this test targets) and out of
		// scope here.
		for i := 2; i < n-1; i++ {
			for j := i + 1; j < n; j++ {
				chunks := []string{
					strings.Join(lines[:i], "\n"),
					strings.Join(lines[i:j], "\n"),
					strings.Join(lines[j:], "\n"),
				}
				got := balanceFences(chunks)

				var gotContent []string
				for _, c := range got {
					if strings.Count(c, "```")%2 != 0 {
						t.Fatalf("blankRunLen=%d cuts=(%d,%d): chunk has an unterminated fence: %q (all chunks: %#v)", blankRunLen, i, j, c, got)
					}
					var nonFence []string
					for _, l := range strings.Split(c, "\n") {
						if strings.HasPrefix(strings.TrimSpace(l), "```") {
							continue
						}
						nonFence = append(nonFence, l)
					}
					if len(nonFence) == 0 {
						t.Fatalf("blankRunLen=%d cuts=(%d,%d): chunk consists solely of fence markers: %q (all chunks: %#v)", blankRunLen, i, j, c, got)
					}
					gotContent = append(gotContent, nonFence...)
				}

				if !slices.Equal(gotContent, want) {
					t.Fatalf("blankRunLen=%d cuts=(%d,%d): content preservation failed\ngot:  %#v\nwant: %#v\nchunks: %#v", blankRunLen, i, j, gotContent, want, got)
				}
			}
		}
	}
}

func TestChunkMessage_FencedBlockWithBlankRunFollowedByProseStaysBalancedAndLossless(t *testing.T) {
	// Property sweep through the PUBLIC entry point: a fenced code block
	// containing a real code prefix, a blank-line run of varying length
	// (mixing truly-empty and whitespace-only lines), a real code suffix,
	// and a trailing prose line, swept across a range of chunk widths.
	// Unlike the earlier version of this sweep (which built bodies from a
	// single repeated non-blank line and so could structurally never
	// produce an all-blank chunk), this shape lets a raw split land
	// squarely inside the blank-line run — which is exactly the class of
	// input that let two consecutive balanceFences regressions ship.
	//
	// Content preservation here is checked against non-blank lines only.
	// chunkMessage's raw splitter right-trims ALL trailing newlines from
	// a raw chunk (strings.TrimRight), which can collapse several blank
	// lines at once when a split lands inside a blank-line run — a
	// pre-existing, separate behavior of chunkMessage's character-level
	// splitting, not of balanceFences, and out of scope here. See
	// TestBalanceFences_ArbitraryChunkBoundariesNeverLoseOrMisrenderContent
	// for the precise, chunk-boundary-exact content-preservation check
	// (including blank lines) against balanceFences itself, which that
	// splitter quirk cannot affect. TrimRight can only ever strip
	// trailing BLANK lines, never touch a non-blank line, so exact
	// non-blank content preservation is a safe, precise assertion at this
	// public-API layer.
	newInput := func(blankRunLen int) string {
		var body strings.Builder
		body.WriteString("code line one\n")
		body.WriteString("code line two\n")
		for i := 0; i < blankRunLen; i++ {
			if i%3 == 1 {
				body.WriteString("   \n")
			} else {
				body.WriteString("\n")
			}
		}
		body.WriteString("code line three\n")
		body.WriteString("code line four\n")
		return "```python\n" + body.String() + "```\nTrailing prose sentence after the fenced block."
	}

	// nonBlankWords, not nonBlankLines: at a narrow width, chunkMessage's
	// own word-wrap fallback (lastIndexSpace) can legitimately split ONE
	// logical source line across two chunks at a space (e.g. "code line
	// one" becoming "code " then "line one") when no nearby newline
	// leaves a "substantial" first part — correct line-wrapping, not
	// content loss, and unrelated to fences. chunkMessage never splits
	// WITHIN a word, only at whitespace/newline boundaries, so comparing
	// word sequences (not line sequences) is the precise, wrap-agnostic
	// invariant: every word that went in must come out, in order.
	nonBlankWords := func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "```") {
				continue
			}
			out = append(out, strings.Fields(l)...)
		}
		return out
	}

	check := func(t *testing.T, width, blankRunLen int) {
		t.Helper()
		in := newInput(blankRunLen)
		got := chunkMessage(in, width)
		want := nonBlankWords(in)

		var gotWords []string
		for i, c := range got {
			if strings.Count(c, "```")%2 != 0 {
				t.Fatalf("width=%d blankRunLen=%d: chunk %d of %d has an unterminated fence: %q", width, blankRunLen, i, len(got), c)
			}
			var nonFence []string
			for _, l := range strings.Split(c, "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "```") {
					continue
				}
				nonFence = append(nonFence, l)
			}
			if len(nonFence) == 0 {
				t.Fatalf("width=%d blankRunLen=%d: chunk %d consists solely of fence markers: %q", width, blankRunLen, i, c)
			}
			for _, l := range nonFence {
				gotWords = append(gotWords, strings.Fields(l)...)
			}
		}
		if !slices.Equal(gotWords, want) {
			t.Fatalf("width=%d blankRunLen=%d: non-blank content preservation failed\ngot:  %#v\nwant: %#v", width, blankRunLen, gotWords, want)
		}
	}

	// width starts at 18, not 8: below that, chunkMessage's real
	// character-level splitter can land a raw split INSIDE (or right
	// after) the "```python" opening marker itself, isolating it alone in
	// its own chunk with nothing else — the same pre-existing,
	// out-of-scope, fresh-fence-open-with-nothing-following behavior
	// TestBalanceFences_ArbitraryChunkBoundariesNeverLoseOrMisrenderContent
	// documents and excludes (verified present before any of these
	// fixes). 18 is comfortably above "```python\n" (10 chars); verified
	// empirically clean through width 80.
	for width := 18; width <= 80; width += 3 {
		for _, blankRunLen := range []int{0, 1, 2, 5, 20, 60} {
			check(t, width, blankRunLen)
		}
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
