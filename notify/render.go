package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes cuts s so it is at most max bytes, and the result stays valid UTF-8 without splitting a character.
//
// The cut has to land on a character boundary. WeCom group-robot Markdown has a hard limit of 4096 bytes, not characters,
// and a CJK character is 3 bytes. Slicing by raw bytes splits a character in half and produces invalid UTF-8.
// The platform then either rejects the whole message or shows a replacement box. This walks back from the budget
// to the nearest rune start (utf8.RuneStart rejects continuation bytes 0b10xxxxxx).
//
// max <= 0 means no limit. An ellipsis is appended unless max is too small to hold it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis. Drop the ellipsis and cut, so the result does not exceed max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine collapses multi-line text to one line: fold all whitespace, then cut by character count.
// Titles in IM messages use this. Summaries often contain newlines, and dropping them straight into a table or title breaks the layout.
// max <= 0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes cuts s to at most max characters (not bytes) and appends an ellipsis when it overflows.
// max <= 0 means no limit.
//
// TruncateBytes and TruncateRunes exist because the platforms count differently: WeCom limits bytes, Telegram limits characters.
// Using the wrong unit does not return an error. It just cuts the message far shorter than intended
// (one CJK character is 3 bytes, so a 4096-byte cut leaves about 1365 characters). Both functions stay, and each channel picks one.
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

// TruncateHTML cuts an HTML fragment by character count and does not leave a half-open tag.
//
// Cutting HTML by characters can produce a broken tag such as `<a href="htt`. The platform parser then
// either rejects the whole message or swallows the rest of the body as an attribute value. This cuts by
// characters first, then if the tail has an unclosed `<`, it backs up to just before that `<`.
//
// It does not balance tags (it does not insert a missing `</b>`). Telegram's HTML parser closes unclosed tags itself.
// Balancing quotes inside attributes, comments, and self-closing tags would cost more than it returns.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment that starts with `<` (a `<` with no later `>`), back up to before that `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut HTML entity (`&amp;` cut to `&amp`), back up the same way.
	// A leftover entity can make a parser that only accepts entities reject the whole message.
	// A digest that exceeds the length limit is common, and it is not worth losing the whole notification for this.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount counts how many items fit completely inside the budget, so a digest is packed by whole item.
//
// Packing by whole item, rather than rendering the full text and then truncating, matters because truncation
// makes the later items disappear while their delivery records are still marked delivered. They are missing
// from the message and from the delivery history, so the findings are gone. After packing by whole item, the
// ones that do not fit stay in the store for the next batch, and kept is how many this message really sent.
//
// maxSize <= 0 means no limit. reserve is held back for the header and footer.
// size measures length. The platforms differ: WeCom and DingTalk count bytes, Telegram counts characters.
// The wrong unit does not return an error; it just compresses the message far below the limit.
// render turns item idx into its real text. Length depends on the content, so it cannot be estimated.
//
// The result is at least 1 when there is still an item. An extremely long single item is still sent, and the
// caller's final truncation is the backstop. Otherwise one oversized finding would stall the whole batch forever.
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

// byteSize and runeSize are the two measuring units for packItemCount. Naming them avoids a bare
// func(s string) int closure at the call site, where it is hard to see which unit is in use.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders the asset list as one display line. Past limit, the rest are omitted and the total is noted.
// One finding can be anchored to dozens of assets, and listing all of them blows up the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " (" + itoa(len(assets)) + " total)"
}

// itoa is a short alias for strconv.Itoa, used only when joining display text, so this file does not import strconv.
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
