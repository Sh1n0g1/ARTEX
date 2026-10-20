package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, guaranteeing the result is
// valid UTF-8 and doesn't cut a character in half.
//
// Why it must cut on a character boundary: a WeCom group bot's markdown has
// a hard limit of 4096 **bytes** (not characters), and a Chinese character is
// 3 bytes. A raw byte-slice truncation would cut a Chinese character in half,
// producing invalid UTF-8 — the platform would then either reject the whole
// message or display garbled boxes. The approach here is to back up from the
// budget position to the nearest rune-start byte (utf8.RuneStart tells
// continuation bytes 0b10xxxxxx apart from lead bytes).
//
// max<=0 means unlimited. An ellipsis is appended after truncation, unless
// max is too small to even fit the ellipsis.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis itself: drop the ellipsis and
		// truncate plainly, to avoid the result exceeding max after all.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine flattens multi-line text to a single line: collapses all
// whitespace, then truncates by character count. Used for an IM message's
// title line — summaries often contain newlines, and stuffing one straight
// into a table/title would break the layout. max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending
// an ellipsis when it's exceeded. max<=0 means unlimited.
//
// The difference from TruncateBytes is the platform's unit: WeCom limits by
// byte count, Telegram limits by character count. Using the wrong unit won't
// error, it'll just truncate the message far shorter than intended (1
// Chinese character = 3 bytes, so truncating at 4096 bytes leaves only about
// 1365 characters), so both functions must be kept and chosen per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count, guaranteeing it
// doesn't produce a half-cut tag.
//
// Truncating HTML by character count directly can cut out a mangled tag like
// `<a href="htt`, and the platform's parser would either error out and
// reject the whole message, or swallow the rest of the body as an attribute
// value. The approach here: truncate by character count first, then check
// whether the tail has an unclosed `<`, and if so back up to before it.
//
// Doesn't balance tags (e.g. adding a closing `</b>`): Telegram's HTML parser
// auto-closes unclosed tags, and implementing balancing ourselves would need
// to handle quotes inside attributes, comments, and self-closing tags — the
// complexity isn't worth the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (the last `<` has no `>`
	// after it), back up to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut-off HTML entity (e.g. `&amp;` cut down to `&amp`),
	// back up the same way. An entity fragment, in a parser that only
	// understands entities, might get the **whole message** rejected — and a
	// digest message exceeding the length cap is already common, so it's not
	// worth losing the whole notification over this.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items can be **fully** fit within the
// budget, for packing a digest message whole-item by whole-item.
//
// Why pack whole items instead of rendering everything and then truncating:
// truncation would make the second half of the items vanish without a trace,
// while their delivery records would still get marked as delivered — nothing
// in the message or the delivery history shows it, and the finding is simply
// gone. By packing whole items, the ones that don't fit stay in the database
// as the next batch, and the kept value the caller gets back is exactly how
// many items this message actually delivers.
//
// Parameters: maxSize<=0 means unlimited; reserve is the amount set aside for
// the message header/footer; size does the measuring (platforms differ:
// WeCom/DingTalk count bytes, Telegram counts characters — using the wrong
// unit won't error, it'll just compress Chinese-language messages to far
// below the actual limit); render turns item idx into its actual
// text — length varies by content and can't be estimated.
//
// Always returns at least 1 (as long as there are items). Even when a single
// item is extremely long, it's still sent, backstopped by the caller's final
// truncation — otherwise one oversized finding would permanently stall the
// whole batch.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are packItemCount's two measurement units, named
// explicitly to avoid a bare func(s string) int closure at the call site,
// which would make it hard to tell at a glance which unit is in use.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list as one display line, omitting the rest
// and noting the total count when it exceeds limit. A finding can be
// anchored to dozens of assets; listing them all would overflow the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " (" + itoa(len(assets)) + " total)"
}

// itoa is a short alias for strconv.Itoa, used only for building display
// text, to avoid importing strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
