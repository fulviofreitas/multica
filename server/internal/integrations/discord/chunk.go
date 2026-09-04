package discord

import "strings"

// chunkMessage splits text into pieces no longer than maxChars UTF-16 code
// units, on rune boundaries only, then re-balances fenced code blocks across
// the resulting pieces so a split that falls inside a ``` fence never leaves
// an unterminated block in one chunk (which would render the REST of the
// conversation as code in Discord's client — see balanceFences).
//
// The unit-counting and newline-preference logic mirrors
// telegram/sender.go's chunkMessage: Discord's stated 2000-character limit,
// like Telegram's, is counted in UTF-16 code units, so a message heavy with
// astral-plane runes (many emoji, some symbol blocks) can hit the wire limit
// well before a naive `len([]rune(s))` count would. maxMessageChars (1900)
// already leaves headroom under Discord's real 2000 cap for whatever
// fence-balancing markers this function adds after the initial cut.
func chunkMessage(text string, maxChars int) []string {
	if maxChars <= 0 || utf16Units(text) <= maxChars {
		return []string{text}
	}

	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		n := 0
		end := 0
		for i, r := range runes {
			units := 1
			if r > 0xFFFF {
				units = 2
			}
			if n+units > maxChars {
				break
			}
			n += units
			end = i + 1
		}
		if end == 0 {
			// A single rune's UTF-16 width alone exceeds maxChars — cannot
			// happen for any real Unicode code point (max 2 units), but
			// guards against an infinite loop if maxChars is absurdly
			// small.
			end = 1
		}
		// Prefer the last newline in the window, but only when it leaves a
		// substantial first chunk rather than producing tiny fragments —
		// same heuristic as telegram.chunkMessage.
		if i := lastIndexRune(runes[:end], '\n'); i >= 0 && utf16Units(string(runes[:i])) > maxChars/2 {
			end = i + 1
		} else if i := lastIndexSpace(runes[:end]); i >= 0 && utf16Units(string(runes[:i])) > maxChars/2 {
			// No usable newline: fall back to the last whitespace run in the
			// window before resorting to a hard mid-word split, so prose
			// without paragraph breaks still splits on word boundaries.
			end = i + 1
		}
		chunks = append(chunks, strings.TrimRight(string(runes[:end]), "\n"))
		runes = runes[end:]
	}
	return balanceFences(chunks)
}

// balanceFences walks chunks in order, tracking whether a ``` fence is open
// at each chunk boundary. When a chunk's fence is still open at its end
// (the split fell inside a fenced code block), it closes the fence in THAT
// chunk and reopens an identical fence (same language tag) at the start of
// the next chunk — so every chunk Discord receives has its own complete,
// balanced set of fences, and the code styling never leaks into the
// surrounding prose of later chunks.
//
// The reopen is deferred rather than written unconditionally: chunkMessage
// picks split points purely by UTF-16 budget, with no awareness of fence
// state, so a split can land immediately BEFORE the fence's own original
// closing marker with no code between the two. Reopening eagerly in that
// case manufactured a content-free ```lang\n``` chunk that the model never
// wrote — cosmetically wrong (an empty code block in the middle of the
// conversation) even though no real content was lost. Buffering the reopen
// (and any blank lines) until the first real content line lets us detect
// that situation: if the very next non-blank line turns out to be the
// fence's own close, the whole segment is discarded — both the never-needed
// reopen and the now-redundant closing marker — since the PREVIOUS chunk
// already terminated the fence with its own synthetic close. If discarding
// leaves a chunk with nothing else in it, the chunk itself is dropped
// instead of being emitted as an empty message.
func balanceFences(chunks []string) []string {
	open := false
	lang := ""
	out := make([]string, 0, len(chunks))

	for _, chunk := range chunks {
		var outLines []string
		// pending buffers lines seen while a fence carried over from the
		// previous chunk hasn't yet been committed (i.e. no real code line
		// has justified writing its ```lang reopen marker). Only blank
		// lines can end up here: the moment a fence marker or a non-blank
		// line is seen, the segment resolves one way or the other and
		// pending is drained or dropped.
		var pending []string
		committed := !open // no reopen is owed if nothing was open coming in

		lines := strings.Split(chunk, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			isFence := strings.HasPrefix(trimmed, "```")

			switch {
			case isFence && open:
				open = false
				if !committed {
					// The reopened fence closes with nothing in between —
					// drop the buffered blank lines and this marker rather
					// than emitting an empty fenced block.
					pending = nil
					lang = ""
					continue
				}
				lang = ""
				outLines = append(outLines, line)
			case isFence && !open:
				open = true
				lang = strings.TrimPrefix(trimmed, "```")
				outLines = append(outLines, line)
			case committed:
				outLines = append(outLines, line)
			case trimmed == "":
				pending = append(pending, line)
			default:
				// First real content since the fence reopened: commit the
				// reopen marker and any buffered blank lines ahead of it.
				outLines = append(outLines, "```"+lang)
				outLines = append(outLines, pending...)
				pending = nil
				committed = true
				outLines = append(outLines, line)
			}
		}

		if open {
			outLines = append(outLines, "```")
		}

		if len(outLines) == 0 {
			// Everything in this chunk was the discarded tail of a fence
			// that turned out to need no reopening — skip it instead of
			// emitting an empty message.
			continue
		}
		out = append(out, strings.Join(outLines, "\n"))
	}
	return out
}

// utf16Units counts s's length the way Discord (and Telegram) count a
// message body: UTF-16 code units, so an astral-plane rune (most emoji)
// counts as 2, not 1. Mirrors telegram.utf16Units.
func utf16Units(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func lastIndexRune(rs []rune, r rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// lastIndexSpace returns the index of the last whitespace rune in rs, or -1
// if none. Used as chunkMessage's second-choice split point when no newline
// falls usefully inside the window.
func lastIndexSpace(rs []rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		switch rs[i] {
		case ' ', '\t':
			return i
		}
	}
	return -1
}
