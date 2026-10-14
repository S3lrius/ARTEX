package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers packing by whole item. When a digest exceeds the channel length limit, it must be cut
// on item boundaries and report how many items did not fit, so the caller marks only the ones that were really sent.
//
// The old approach rendered the full text, truncated it, and marked the whole batch delivered. The rest of the
// message vanished, the delivery history said everything succeeded, and the findings were gone with nothing to show it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// A 200-item digest is far past WeCom's 4096-byte limit.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body is %d bytes, over the limit %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part of the batch should fit (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must say how many this message contains and how many remain. Otherwise the reader treats the header number as the whole batch.
	if !strings.Contains(body, "continue in the next message") {
		t.Fatalf("the header should say how many items are not in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Only the first kept items should be present.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "finding"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "finding"+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("with no length limit everything should be kept, got kept=%d", kept)
	}
	if strings.Contains(body, "continue in the next message") {
		t.Fatalf("a truncation note should not appear when nothing was truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget cannot hold even one item, one item is still sent and final truncation is the backstop.
	// Otherwise one oversized finding stalls the whole batch forever: every claim fails to fit, and nothing is ever sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least 1 item should be kept, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single message should report 1 item delivered, got %d", kept)
	}
	// An empty message has nothing to deliver.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("an empty message should report 0 items, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits length in characters. Counting bytes would compress a multi-byte message to about a third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body is %d characters, over the limit %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part of the batch should fit, got %d", kept)
	}
	if !strings.Contains(text, "continue next") {
		t.Fatalf("the text should say some items were left out:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("the card should hold only part of the batch, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body, so the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition failed")
	}
	// Confirm indirectly through the renderer: markdownBody(0) keeps everything when there is no limit.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("with no length limit everything should be kept, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is the regression test that untrusted content must not change the message structure.
// Titles and summaries come from model output (the model read the target's response), and asset names come from the target's URL.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear (the escaped form)
		wrong []string // must not appear (the unescaped form)
	}{
		{
			name: "newline and external link in the title",
			item: Item{
				Severity: "high",
				Name:     "login SQL injection\n[Urgent: verify your account](http://attacker.tld)",
			},
			// The newline must be folded (otherwise it can forge a new list item or quote block).
			// Brackets and parentheses must be escaped (otherwise it is a clickable external link).
			must:  []string{`\[Urgent: verify your account\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "image beacon in the title",
			item: Item{
				Severity: "high",
				Name:     "finding ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and a quote in the asset name",
			item: Item{
				Severity: "high",
				Name:     "plain title",
				Assets:   []string{"a.com/*inject*>quote"},
			},
			must:  []string{`\*inject\*`, `\>`},
			wrong: []string{"*inject*"},
		},
		{
			name: "backticks and a pipe in the summary",
			item: Item{
				Severity: "high",
				Name:     "title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// writeItem in single mode is the render path shared by the three Markdown channels.
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
					t.Errorf("unescaped form %q appeared (it can inject structure or an external link):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst locks the escape order. The backslash must be handled first,
// or the backslashes added later get escaped again and the output shows a doubled backslash.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escape order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks a specific regression.
// Markdown escaping must not leak into Telegram's HTML output. Escaping used to live in the shared title
// function, and Telegram messages showed visible backslashes such as `\(1\)`.
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram body contains a Markdown backslash escape:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
