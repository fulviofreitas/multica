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
// conversation) even though no real content was lost.
//
// A carried-over (open==true entering the chunk) fence is "undecided"
// until one of exactly three things resolves it, and each must be handled
// deliberately — silently falling through any of them either manufactures
// a content-free fence or drops real source lines:
//
//  1. Real (non-blank, non-fence) content arrives: the reopen was needed,
//     so commit it — write the ```lang marker, then any blank lines that
//     were buffered ahead of it, then the content itself.
//  2. The fence's OWN closing marker arrives first: if nothing at all
//     (not even a blank line) was buffered, the reopen would have wrapped
//     zero content — discard it and the marker together, since the
//     PREVIOUS chunk's synthetic close already terminated the fence. But
//     if blank lines WERE buffered first, they are real source content
//     (e.g. a blank-line gap inside a code block) and must not be
//     dropped just because no non-blank line preceded the close — commit
//     the reopen and those buffered lines before writing the close.
//  3. The chunk simply ends with the fence still open (continuing into
//     the next chunk) and nothing was ever committed: this happens when
//     an entire chunk is blank lines carved out of the middle of a
//     fenced block by chunkMessage's split. Those blank lines are real
//     content too — commit the reopen and the buffered lines before
//     appending the trailing synthetic close.
//
// In short: "pending" (buffered blank lines awaiting a decision) must
// never be discarded while non-empty. It is only ever safe to drop
// pending, and skip writing a reopen at all, when it is completely empty
// — meaning the inherited fence's own closing marker was the very first
// thing seen, with truly nothing (not even a blank line) in between.
func balanceFences(chunks []string) []string {
	open := false
	lang := ""
	out := make([]string, 0, len(chunks))

	for _, chunk := range chunks {
		var outLines []string
		// pending buffers lines seen while a fence carried over from the
		// previous chunk (or opened earlier in this chunk) hasn't yet been
		// committed. Only blank lines can end up here: the moment a fence
		// marker or a non-blank line is seen, commit (below) resolves the
		// segment one way or the other.
		var pending []string
		committed := !open // no reopen is owed if nothing was open coming in

		// commit writes the buffered ```lang reopen marker plus any
		// buffered blank lines, if a reopen is still owed. It is a no-op
		// once committed is already true, so every exit path below can
		// call it unconditionally right before it needs pending resolved,
		// without needing to know which of the three cases applies.
		commit := func() {
			if committed {
				return
			}
			outLines = append(outLines, "```"+lang)
			outLines = append(outLines, pending...)
			pending = nil
			committed = true
		}

		lines := strings.Split(chunk, "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			isFence := strings.HasPrefix(trimmed, "```")

			switch {
			case isFence && open:
				if !committed && len(pending) == 0 {
					// Exit path 2, nothing-at-all case: the inherited
					// fence's own close is the very first thing in this
					// chunk. Reopening it would have wrapped zero
					// content, so discard the (empty) reopen and this
					// now-redundant marker together — the PREVIOUS
					// chunk's synthetic close already terminated the
					// fence.
					open = false
					lang = ""
					committed = true
					continue
				}
				// Exit path 2, blank-lines-buffered case (commit flushes
				// them), or an ordinary close of a fence that already
				// had real content (commit is a no-op here).
				commit()
				open = false
				lang = ""
				outLines = append(outLines, line)
			case isFence && !open:
				// Opening a fence. Any carried-over segment is always
				// fully resolved (committed==true) by the time open can
				// be false here, since every path above that sets
				// open=false also leaves committed==true.
				open = true
				lang = strings.TrimPrefix(trimmed, "```")
				outLines = append(outLines, line)
			case committed:
				outLines = append(outLines, line)
			case trimmed == "":
				pending = append(pending, line)
			default:
				// Exit path 1: real content resolves the reopen.
				commit()
				outLines = append(outLines, line)
			}
		}

		if open {
			// Exit path 3: the fence is still open at the end of this
			// chunk (continuing into the next one). If it was never
			// committed, the whole chunk was blank lines buffered in
			// pending — flush them before appending this chunk's own
			// trailing close, instead of letting them vanish.
			commit()
			outLines = append(outLines, "```")
		}

		if len(outLines) == 0 {
			// Everything in this chunk was the discarded (truly empty)
			// tail of a fence that turned out to need no reopening — skip
			// it instead of emitting an empty message.
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
