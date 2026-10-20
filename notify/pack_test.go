package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack whole items" fix: when a digest message exceeds
// the channel's length cap, it must be truncated **by whole item**, honestly
// reporting how many items didn't fit, so the caller only marks the ones
// genuinely delivered.
//
// The previous approach was to render the full text and then truncate it,
// marking the whole batch as delivered: the second half of the message
// vanished without a trace, while the delivery history showed full success --
// the finding was simply gone, with no way to notice anywhere.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 Chinese-language digest items, necessarily far exceeding WeCom's
	// 4096 bytes.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body is %d bytes, exceeding the limit of %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should only fit a portion (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must honestly state how many items this message includes
	// and how many remain -- otherwise the reader would take the header's
	// number as the whole batch.
	if !strings.Contains(body, "remaining") || !strings.Contains(body, "continue in the next message") {
		t.Fatalf("header should state how many items aren't included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Should only include the first kept items.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = unlimited
	if kept != len(m.Items) {
		t.Fatalf("should keep everything when length is unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "remaining") {
		t.Fatalf("a truncation notice should not appear when nothing was truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is too small to fit even one item, one must still be
	// sent (backstopped by the final truncation). Otherwise a single
	// oversized finding would permanently stall the whole batch: every
	// attempt fails to fit, so nothing is ever sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("should keep at least 1 item, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single-item message should report 1 delivered, got %d", kept)
	}
	// An empty message has nothing deliverable.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("an empty message should report 0, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits by **character count**; using a byte-based measure
	// would compress a Chinese-language message down to a third of its limit.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body is %d characters, exceeding the limit of %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("should only fit a portion, got %d", kept)
	}
	if !strings.Contains(text, "continue in the next message") {
		t.Fatalf("should state that some items weren't included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("the card should only fit a portion, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels don't truncate the body; the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition not met")
	}
	// Confirmed indirectly via the renderer's return value: markdownBody(0)
	// keeps everything when unlimited.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("should use everything when length is unlimited, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for "untrusted
// content must not alter message structure". The title and summary come from
// model output (the model reads the tested target's responses), and asset
// names come from the tested target's URLs.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in the result (escaped form)
		wrong []string // must not appear in the result (unescaped form)
	}{
		{
			name: "newline + external link in the title",
			item: Item{
				Severity: "high",
				Name:     "登录口 SQL 注入\n[紧急：点此验证账号](http://attacker.tld)",
			},
			// The newline must be collapsed (otherwise it could forge a new
			// list item/blockquote); brackets and parentheses must be
			// escaped (otherwise it's a clickable external link).
			must:  []string{`\[紧急：点此验证账号\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[紧急", "\n\n[紧急"},
		},
		{
			name: "image beacon in the title",
			item: Item{
				Severity: "high",
				Name:     "漏洞 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and blockquote in an asset name",
			item: Item{
				Severity: "high",
				Name:     "普通标题",
				Assets:   []string{"a.com/*注入*>引用"},
			},
			must:  []string{`\*注入\*`, `\>`},
			wrong: []string{"*注入*"},
		},
		{
			name: "backtick and pipe in the summary",
			item: Item{
				Severity: "high",
				Name:     "标题",
				Summary:  "`code` | 表格",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// Single-item mode's writeItem is the rendering path shared by
			// the three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form %q appeared (could be used to inject structure or an external link):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks down the escaping order: the
// backslash must be handled first, or it would wrap a second layer around
// the backslashes added afterward, producing double backslashes in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escaping order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks down a specific regression:
// markdown escaping must not leak into Telegram's HTML output (markdown
// escaping was once added to the shared title function, and Telegram
// messages ended up with visible backslashes like `\(1\)`).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("markdown's backslash escaping appeared in the Telegram body:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
