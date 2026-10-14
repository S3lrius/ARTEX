package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most important invariant in the package. WeCom limits length in bytes, and a 3-byte character
	// is split in half by any implementation that cuts on raw bytes, producing invalid UTF-8 that the platform rejects.
	// Several inputs whose lengths are coprime, mixing ASCII and 3-byte characters, hit every possible cut point.
	// The euro sign is 3 bytes, so the rune-boundary walk-back still runs.
	inputs := []string{
		"€€€€€€",
		"mix € mixed € content",
		"a€b€c€d€e",
		"🔴🟠🟡🔵", // 4-byte emoji; a bad cut is even more obvious
		strings.Repeat("€x", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result is %d bytes, over the limit", in, max, len(got))
			}
			// Content that was not truncated must not be changed.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: under the limit but the content changed -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean no limit")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean no limit")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, appending the ellipsis must not push the result over the limit.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("at max=1 the result %q has length %d, over the limit", got, len(got))
	}
	// The normal case should carry the ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected an ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The difference from TruncateBytes has to stay. Telegram limits characters.
	// Counting bytes would cut a multi-byte message down to about a third.
	s := strings.Repeat("€", 10)
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string counted in bytes should be clearly shorter.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("the byte unit should not produce the same character count as the character unit")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("first line\n\nsecond line\twith a tab   and spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("all whitespace should be folded, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("consecutive spaces should not remain, got %q", got)
	}
	// After truncation it must still be readable and valid.
	got = OneLine(strings.Repeat("€", 10), 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Cutting HTML directly can produce a fragment such as `<a href="htt`, and the platform rejects the whole message.
	s := `<b>Title</b>body body body<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result is %d characters, over the limit", max, n)
		}
		// The tail must not have an unclosed `<` (a `<` in the last span with no `>`).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: a tag in the tail was cut -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("under the limit everything should be listed, got %q", got)
	}
	// Past the limit the total has to be noted, or the reader does not know how many assets were left out.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "(5 total)") {
		t.Fatalf("the total of 5 should be noted, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("an empty severity has rank 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("an empty threshold should allow the event")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("unexpected status mapping, got %q", got)
	}
	// An unknown status is echoed as-is. A label is not invented.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("an unknown status should be echoed as-is, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers a gap an audit pointed out: truncation has to avoid
// a half-open tag and also a cut HTML entity.
//
// After `&amp;` is cut to `&amp`, a parser that only accepts entities may reject the whole message.
// An over-long digest is common, and that cost is too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain an entity fragment that has `&` and no matching `;`.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: an entity fragment was left in the tail %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: a malformed entity appeared", max)
		}
	}
}
