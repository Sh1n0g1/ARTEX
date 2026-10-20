package notify

import (
	"fmt"
	"strings"
)

// This file contains the shared message rendering for the "markdown-family"
// channels (DingTalk, WeCom). Feishu uses a card JSON, Telegram uses HTML,
// and email uses HTML — each renders in its own adapter.

// maxAssetsShown is the max number of assets listed in a message. A finding
// can be anchored to dozens of assets; listing them all would overflow the
// message with no information value — nobody reads the 5th domain onward in
// an IM message.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. An IM
// message is a "go look at the details" prompt, not the report body itself —
// the full content lives in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message header (digest line +
// severity breakdown + a possible truncation notice) and footer (platform
// link). This is subtracted from the budget when packing whole items, so the
// header/footer never get truncated — if they were, the reader couldn't even
// tell which batch this is or how many items are missing.
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
//
// Why this is necessary: the finding title, summary, type and asset display
// names all come from **untrusted sources** — the title and summary come
// from model output (the model reads the target's responses), and an asset's
// URL is the full URL obtained from scanning (including a query string the
// target controls). Without escaping, a title like
//
//	Login SQL injection\n[Urgent: click to verify your account](http://attacker.tld)
//
// would render as a **clickable external link** in a security engineer's
// DingTalk/Feishu; and `![](http://attacker.tld/beacon)` would be fetched by
// the client when rendered, effectively signaling "this finding has been
// viewed" and leaking the reader's IP. Even non-malicious injected bold text
// or a blockquote could push a serious finding below the fold.
//
// The escape set covers the characters for headings/links/emphasis/lists/
// quotes/strikethrough — the classes that change structure or produce
// clickable elements. `\` must be handled first, or it would re-escape the
// backslashes added for the other characters.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText flattens untrusted text to a single line and escapes it, for
// use in markdown body text. Flattening is the other half of escaping:
// newlines alone can forge a new list item or blockquote, which escaping
// characters alone can't stop.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the IM platform's title bar / card
// title); its content is the **unescaped raw text**.
//
// Escaping is deliberately skipped here: this title is shared by renderers
// across four different contexts — markdown body text, Telegram's HTML,
// Feishu card's plain_text, and the generic webhook's JSON and email
// subject. Each context's escaping rules differ (markdown escaping stuffed
// into HTML leaves visible backslashes, stuffed into JSON corrupts the data),
// so escaping must be the responsibility of each output side — see
// writeItem / feishuItemLines / telegramEscape. Markdown escaping was once
// added to this shared function, and Telegram messages ended up showing
// visible backslashes like `\(1\)`.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability digest · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and the
// **number of items actually written**.
//
// The returned kept is the number of items truly delivered in this send; the
// caller uses it to mark only the first kept items as delivered — items
// blocked by the channel's length limit must be left for the next batch, not
// marked successful along with the rest. This is exactly how "silent loss"
// happens: the message gets truncated, but the delivery record shows
// everything delivered, with nowhere to notice that the second half was
// never sent.
//
// maxBytes<=0 means unlimited.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// Send a single message even if it's too long (the final truncation
		// backstops this): partial info about one finding beats sending nothing.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the opening of a digest message: time window,
// item count, and severity breakdown. With these, a recipient can judge
// whether this batch needs immediate attention without clicking into the
// platform.
//
// items is what was **actually packed in**, total is how many this batch
// should have had. When they differ, it must say explicitly "how many more
// are in the next message" — otherwise the reader would assume the number in
// the header is everything, while the items that were never sent are
// completely invisible in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**New in the last %d minutes: %d vulnerabilities**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**%d new vulnerabilities**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (showing the first %d; the remaining %d will continue in the next message)", len(items), extra)
	}
	// Give a breakdown by severity so the reader can see at a glance whether
	// there's anything serious. Only counts the items **actually included**
	// in this message, so "Critical 3" matches what can actually be counted
	// below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single finding item.
//
// prefix is the digest list's item number; single=true renders the full
// version (with summary and detail link); the digest list only renders one
// summary line per item — otherwise a 50-item digest would turn into a long
// document.
//
// All content from outside (title/type/assets/summary) goes through
// markdownText: flattened to one line + escaped. The detail link is built
// from the admin-configured public_base_url, which is not untrusted content,
// and it must be a clickable link, so it's output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: single-line presentation, with assets and summary
		// compressed and appended after.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
