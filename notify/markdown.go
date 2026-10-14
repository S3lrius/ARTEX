package notify

import (
	"fmt"
	"strings"
)

// This file renders messages shared by the Markdown channels (DingTalk and WeCom).
// Feishu uses card JSON, Telegram uses HTML, and email uses HTML, each rendered in its own adapter.

// maxAssetsShown is how many assets a message lists. One finding can be anchored to dozens of assets.
// Listing all of them blows up the message and adds nothing: nobody reads the fourth domain in an IM client.
const maxAssetsShown = 3

// maxSummaryRunes is how far the summary is compressed. An IM message is a prompt to open the details.
// It is not the report. The full text lives in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the header (digest line, severity breakdown, and a possible truncation note)
// and the footer (the platform link). Packing by whole item subtracts this from the budget so the header and footer
// are not cut off. If they are, the reader cannot tell which batch this is or how many items were left out.
const markdownReservedBytes = 320

// markdownEscape escapes Markdown metacharacters.
//
// This is required because titles, summaries, classes, and asset display names all come from untrusted sources.
// Titles and summaries come from model output (the model read the target's response), and an asset URL is the
// full URL the scan saw, including a query string the target controls. Without escaping, a title of
//
//	login SQL injection\n[Urgent: verify your account](http://attacker.tld)
//
// would render as a clickable external link in a security engineer's DingTalk or Feishu client, and
// `![](http://attacker.tld/beacon)` would be fetched by the client, which reports that the finding was viewed and leaks the reader's IP.
// Even without malice, injected bold or a quote block can push a critical finding below the fold.
//
// The escaped set covers characters that change structure or create a clickable element: headings, links, emphasis, lists, quotes, and strikethrough.
// `\` must be handled first, or the backslashes added later get escaped again.
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

// markdownText collapses untrusted text to one line and escapes it for a Markdown body.
// Collapsing newlines is the other half of escaping: a newline alone can forge a new list item or quote block, and escaped characters do not stop that.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the IM title bar or card title). The text is the raw, unescaped original.
//
// Escaping is deliberately left out. Four renderers share this title: Markdown bodies, Telegram HTML,
// Feishu card plain_text, and generic webhook JSON plus the email subject. Each context escapes differently
// (Markdown escapes left visible backslashes in HTML, and they pollute JSON), so each output path owns its
// escaping. See writeItem, feishuItemLines, and telegramEscape. Markdown escaping used to live in this shared
// function, and Telegram messages showed visible backslashes such as `\(1\)`.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability digest · %d items", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body and returns the body plus how many items were actually written.
//
// kept is how many items this delivery really sent. The caller marks only the first kept items delivered.
// Items that do not fit the channel length limit wait for the next batch instead of being marked successful with the rest.
// That silent loss is the failure this avoids: the message was truncated, the delivery record said everything arrived, and nothing showed that the rest was never sent.
//
// maxBytes <= 0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even when it is too long. Final truncation is the backstop: part of one finding is better than sending nothing.
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

// markdownBatchIntro renders the start of a digest: the time window, the count, and the severity breakdown.
// With that, the recipient can tell whether the batch needs attention without opening the platform.
//
// items are the items that actually fit. total is how many this batch should contain. When they differ, the text
// must say how many continue in the next message. Otherwise the reader treats the header number as the whole batch, and the items that were never sent do not exist anywhere in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new vulnerabilities in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new vulnerabilities**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, " (this message shows the first %d; the other %d continue in the next message)", len(items), extra)
	}
	// Break the items down by severity so a critical one is obvious. Count only the items this message actually contains, so "Critical 3" matches the lines below.
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

// writeItem renders one finding.
//
// prefix is the digest list number. single=true renders the full form, with the summary and the back-link.
// A digest renders one summary line. Otherwise a 50-item digest becomes a long document.
//
// Every external field (title, class, assets, summary) goes through markdownText: one line, then escaped.
// The back-link is built from the administrator's public_base_url. It is not untrusted content, and it has to stay a clickable link, so it is written as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: one line, with the assets and summary compressed after it.
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
