package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit is the character limit of Telegram sendMessage's text field.
const telegramTextLimit = 4096

// telegramChannel implements the Telegram Bot API.
//
// Platform behavior:
//   - Authentication is entirely in the URL path (/bot<token>/sendMessage). There is no signature.
//   - HTML parse mode is used instead of MarkdownV2. MarkdownV2 requires escaping `_*[]()~`>#+-=|{}.!`,
//     18 characters, and missing one rejects the whole message. HTML only needs &, <, and > escaped.
//   - Business errors are also hidden in HTTP 200 and are distinguished by the ok field.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// A Telegram direct chat is about 1 message per second, and a group is 20 per minute. The conservative value is used.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// The bot token is the full credential. chat_id is only the recipient and is not a secret: without the token it cannot send a message.
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url decides which API endpoint the token is sent to (a self-hosted reverse proxy, for example). Changing it requires stating the token again.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("bot token is missing")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("chat id is missing")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API address is invalid: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse the Telegram response: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 is a rate limit, and a retry after backoff works. The rest (400 bad parameters, 401 bad token,
	// 403 blocked, 404 chat does not exist) are configuration problems, and a retry will not fix them.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram rate limit: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram returned error %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint builds the sendMessage address. An empty base_url uses the official API.
// A non-empty value is a self-hosted Bot API reverse proxy.
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// Do not pass err through: the address contains the bot token, and the address itself should not be echoed here.
		return "", fmt.Errorf("failed to build the API address (API address: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML renders the HTML body and returns the body plus how many items were written (see Channel.Send).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram's limit is a character count, so packing is measured in characters (runeSize).
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">View all in the platform</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>Status change</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>Type</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>Assets</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>Summary</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">View details</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes is held back for the message title and a possible truncation note, counted in characters.
const telegramReservedRunes = 160

// telegramBatchLine renders one digest line. It is not escaped; the caller escapes the whole line.
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle renders the digest title line. The count is how many this message actually contains,
// not the batch total. Otherwise the reader treats the header number as the whole batch.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("Vulnerability digest · %d items", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf(" (showing the first %d; the other %d continue next)", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("last %d minutes · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape escapes HTML text.
// Telegram only recognizes these three entities. An existing entity such as &amp; is escaped again, which is
// what we want: the raw characters should be shown, not HTML the user injected.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr escapes an HTML attribute value. Quotes are escaped in addition to the text rules.
// A quote in a URL would close the href early and turn the rest of the content into an injection point.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
