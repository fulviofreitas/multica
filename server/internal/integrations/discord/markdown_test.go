package discord

import (
	"strings"
	"testing"
)

// See markdown.go's package doc for the full degradation table this test
// file exercises. The canonical matrix lives here — sender_test.go's
// chunking tests must not re-derive markdown conversion behavior.

func TestFormatDiscordMarkdown_PassThroughConstructs(t *testing.T) {
	// Every construct Discord natively renders must survive byte-for-byte
	// (aside from the surrounding line, which formatDiscordMarkdown never
	// touches for these cases).
	cases := []struct {
		name string
		in   string
	}{
		{"bold", "this is **bold** text"},
		{"italic-star", "this is *italic* text"},
		{"italic-underscore", "this is _italic_ text"},
		{"strikethrough", "this is ~~struck~~ text"},
		{"inline-code", "run `go test ./...` now"},
		{"blockquote", "> quoted line"},
		{"bullet-list", "- item one\n- item two"},
		{"ordered-list", "1. first\n2. second"},
		{"masked-link", "see [the docs](https://multica.ai/docs)"},
		{"spoiler", "the ending is ||he dies||"},
		{"heading-h1", "# Title"},
		{"heading-h2", "## Subtitle"},
		{"heading-h3", "### Section"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatDiscordMarkdown(tc.in)
			if got != tc.in {
				t.Fatalf("formatDiscordMarkdown(%q) = %q, want unchanged", tc.in, got)
			}
		})
	}
}

func TestFormatDiscordMarkdown_HeadingDegradation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"h4", "#### Deep heading", "**Deep heading**"},
		{"h5", "##### Deeper heading", "**Deeper heading**"},
		{"h6", "###### Deepest heading", "**Deepest heading**"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatDiscordMarkdown(tc.in)
			if got != tc.want {
				t.Fatalf("formatDiscordMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatDiscordMarkdown_TableDegradesToCodeBlock(t *testing.T) {
	in := "| Name | Age |\n| --- | --- |\n| Alice | 30 |\n| Bob | 25 |"
	got := formatDiscordMarkdown(in)

	if !strings.HasPrefix(got, "```\n") || !strings.HasSuffix(got, "\n```") {
		t.Fatalf("table was not wrapped in a fenced code block: %q", got)
	}
	// The raw table text must survive verbatim inside the fence so it stays
	// legible, even without column alignment.
	inner := strings.TrimSuffix(strings.TrimPrefix(got, "```\n"), "\n```")
	if inner != in {
		t.Fatalf("table body was altered: got %q, want %q", inner, in)
	}
}

func TestFormatDiscordMarkdown_TableSurroundedByProse(t *testing.T) {
	in := "Before the table.\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\nAfter the table."
	got := formatDiscordMarkdown(in)

	if !strings.Contains(got, "Before the table.") || !strings.Contains(got, "After the table.") {
		t.Fatalf("surrounding prose was lost: %q", got)
	}
	if !strings.Contains(got, "```\n| A | B |\n| --- | --- |\n| 1 | 2 |\n```") {
		t.Fatalf("table was not degraded to a fenced block in place: %q", got)
	}
}

func TestFormatDiscordMarkdown_BareDashesNotMistakenForTable(t *testing.T) {
	// A horizontal-rule-shaped line ("---") alone is not a two-column table
	// separator and must not trigger degradation on the line above it.
	in := "Some prose\n---\nMore prose"
	got := formatDiscordMarkdown(in)
	if got != in {
		t.Fatalf("non-table content was altered: got %q, want %q", got, in)
	}
}

func TestFormatDiscordMarkdown_InlineCodeProtectedFromTableDetection(t *testing.T) {
	// A pipe character living only inside inline code must never make an
	// ordinary line look like a table row.
	in := "call `a|b` please\nMore prose"
	got := formatDiscordMarkdown(in)
	if got != in {
		t.Fatalf("inline code containing a pipe was misdetected as a table: got %q, want %q", got, in)
	}
}

func TestFormatDiscordMarkdown_CodeBlockLeftUntouched(t *testing.T) {
	// A fenced code block containing markdown-looking text (a heading, a
	// table-shaped set of lines) must pass through completely unmodified —
	// none of the line-level rules apply inside a fence.
	in := "```\n#### not a real heading\n| still | not | a | table |\n| --- | --- | --- | --- |\n```"
	got := formatDiscordMarkdown(in)
	if got != in {
		t.Fatalf("fenced code block content was altered: got %q, want %q", got, in)
	}
}

func TestFormatDiscordMarkdown_CodeBlockWithLanguageTag(t *testing.T) {
	in := "```go\nfunc main() {}\n```"
	got := formatDiscordMarkdown(in)
	if got != in {
		t.Fatalf("fenced code block with language tag was altered: got %q, want %q", got, in)
	}
}
