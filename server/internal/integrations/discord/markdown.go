package discord

import (
	"regexp"
	"strings"
)

// This file converts Multica's standard agent-authored Markdown into
// Discord's own markdown flavor for outbound replies (Task Master subtask
// 6.1). Unlike telegram/markdown.go, which must translate into an entirely
// different target syntax (Telegram's parse_mode=HTML), Discord's message
// content already speaks a markdown dialect close enough to the source that
// bold (**), italic (*/_), strikethrough (~~), inline code (`), fenced code
// blocks (```), block quotes (>), bullet/numbered lists, masked links
// ([text](url) — allowed for bot/webhook authored messages), and spoilers
// (||text||) all pass through UNCHANGED. So the only real conversion work is
// degrading the handful of constructs Discord's client does not render:
//
//	Construct                 | Degradation
//	--------------------------|------------------------------------------
//	GFM pipe tables           | Rendered as a fenced code block (```),
//	                          | verbatim, so the tabular content stays
//	                          | legible in Discord's monospace font
//	                          | instead of rendering as a wall of pipes
//	                          | and dashes with no alignment.
//	Headings level 4-6        | Discord's client only renders #, ## and
//	(####, #####, ######)     | ### as headings; anything deeper is
//	                          | degraded to bold text (**text**) so the
//	                          | emphasis survives instead of showing up
//	                          | as literal "#### " characters.
//	Everything else           | Passed through unchanged — see the list
//	                          | above; Discord's own syntax already
//	                          | matches Multica's source markdown.
//
// Fenced code blocks are protected first and pass through byte-for-byte:
// their content is never scanned for tables or headings, so a code sample
// that happens to contain a "|" table-shaped line or a "####" comment is
// left completely untouched. Inline code spans are protected the same way
// (Telegram's placeholder technique, see formatInline there) before a line
// is tested for "is this a table row", so a pipe character inside inline
// code (e.g. “ `a|b` “) can never be misread as a table cell separator.
var (
	// reHeading matches an ATX heading line ("#" through "######" followed
	// by whitespace). Only level 4+ is degraded — see the table above.
	reHeading = regexp.MustCompile(`^(#{1,6})(\s+)(.*)$`)

	// reInlineCode matches one backtick-delimited inline code span. Mirrors
	// telegram.reInlineCode.
	reInlineCode = regexp.MustCompile("`[^`\n]+`")

	// reTableSeparator matches a GFM table's header/body divider row, e.g.
	// "|---|---|", "--- | ---", or "| :-- | --: |". It requires at least
	// two cells (one pipe joining two dash groups) so a bare "---"
	// horizontal-rule-shaped line is never mistaken for a table divider.
	reTableSeparator = regexp.MustCompile(`^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)+\|?\s*$`)
)

// formatDiscordMarkdown converts md (Multica's standard markdown) into the
// text Discord should render. Line-oriented, mirroring
// telegram.formatHTML's structure: fenced code blocks are recognized first
// and copied through verbatim; everything else is scanned line by line for
// the two degradations documented above.
func formatDiscordMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	var out []string
	inCode := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			// Fence delimiter: toggle code-block state and copy the
			// delimiter itself through unchanged. Discord's own fence
			// syntax is identical to Multica's source syntax, so no
			// rewriting is needed here (unlike Telegram's HTML <pre>).
			inCode = !inCode
			out = append(out, line)
			continue
		}
		if inCode {
			// Verbatim: code content is never scanned for tables or
			// headings, no matter what it looks like.
			out = append(out, line)
			continue
		}

		if tableLines, consumed := tryConsumeTable(lines[i:]); consumed > 0 {
			out = append(out, "```")
			out = append(out, tableLines...)
			out = append(out, "```")
			i += consumed - 1
			continue
		}

		out = append(out, degradeHeadingLine(line))
	}

	return strings.Join(out, "\n")
}

// tryConsumeTable reports whether lines begins a GFM pipe table (a row
// immediately followed by a valid separator row) and, if so, returns every
// contiguous row belonging to that table (header + separator + body) and how
// many input lines it consumed. consumed is 0 when lines does not begin a
// table.
func tryConsumeTable(lines []string) (table []string, consumed int) {
	if len(lines) < 2 || !isTableRow(lines[0]) {
		return nil, 0
	}
	protectedSep, _ := protectInlineCode(lines[1])
	if !reTableSeparator.MatchString(strings.TrimSpace(protectedSep)) {
		return nil, 0
	}

	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "```") || !isTableRow(lines[i]) {
			break
		}
		table = append(table, lines[i])
		i++
	}
	return table, i
}

// isTableRow reports whether line looks like one cell row of a table: not
// blank, and contains at least one "|" that is NOT inside an inline code
// span (protectPipes blanks those out first, so a pipe living only inside
// “ `a|b` “ never counts).
func isTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	return strings.Contains(protectPipes(line), "|")
}

// protectPipes returns line with every inline code span's content replaced
// by a placeholder that contains no "|", so pipe characters that only exist
// inside inline code are invisible to the table-row heuristic. The
// placeholder technique mirrors telegram.formatInline's inline-code
// protection.
func protectPipes(line string) string {
	protected, _ := protectInlineCode(line)
	return protected
}

// protectInlineCode extracts every inline code span in line and replaces it
// with a NUL-delimited placeholder, returning the protected text and a
// restore function that puts the original spans back in order. Mirrors
// telegram.formatInline's span-extraction step, generalized into its own
// helper because this file needs to protect code from two different scans
// (table-row detection and — should a future degradation need it — any
// other content-based check) rather than one fixed styling pass.
func protectInlineCode(line string) (protected string, restore func(string) string) {
	var spans []string
	protected = reInlineCode.ReplaceAllStringFunc(line, func(m string) string {
		spans = append(spans, m)
		return "\x00CODE\x00"
	})
	next := 0
	restore = func(s string) string {
		for next < len(spans) && strings.Contains(s, "\x00CODE\x00") {
			s = strings.Replace(s, "\x00CODE\x00", spans[next], 1)
			next++
		}
		return s
	}
	return protected, restore
}

// degradeHeadingLine passes non-heading lines and level 1-3 headings through
// unchanged (Discord renders #, ## and ### natively) and degrades a level
// 4-6 heading to bold text, since Discord's client does not render "####"
// or deeper as a heading at all — it would otherwise show up as literal
// hash characters in the message.
func degradeHeadingLine(line string) string {
	m := reHeading.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	level := len(m[1])
	if level <= 3 {
		return line
	}
	return "**" + strings.TrimSpace(m[3]) + "**"
}
