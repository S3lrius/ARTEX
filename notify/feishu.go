package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel implements a Feishu (including Lark) custom robot, using an interactive card.
//
// Platform behavior:
//   - The signing algorithm differs from DingTalk and is easy to get wrong. See the feishuSign comment.
//   - Like DingTalk, business errors are stuffed into an HTTP 200 body (code != 0).
//   - The card header supports a color template. Severity maps to that color so the message list shows how serious it is.
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// A Feishu custom robot is about 5 calls per second, which is 100 per minute.
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// The last segment of the webhook URL is the robot's unique id, and it is a credential.
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// Changing the webhook URL requires stating the signing secret again for the new address.
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("webhook address is missing")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("webhook address is invalid: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// Signing fields sit beside the message, and they appear only when a secret is configured.
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// Some Feishu hook versions use these field names. Both are accepted.
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse the Feishu response: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Feishu returned error %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Feishu returned error %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign computes the signature using Feishu's official rule.
//
// This is easy to get wrong. The official sample is
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// which means key = timestamp + "\n" + secret and the message is empty, not the intuitive
// key=secret, message=stringToSign. That second form is DingTalk's algorithm. The two are reversed,
// and copying the other implementation fails signature checks with error 19021.
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate maps a vulnerability severity to the card header color template.
// An unknown severity uses grey, not blue, so it is not confused with low.
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes is a conservative cap on card content. Feishu rejects a card that exceeds its size limit.
// The value is well below the official cap so JSON wrapping overhead is included.
const feishuMaxCardBytes = 24000

// feishuCard builds the interactive card and returns the card plus how many items were actually written.
// kept means the same thing as in markdownBody: only items that really made it into the card should be marked delivered.
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// Pack by whole item before building the header. The header says how many continue in the next message, and that number has to come from how many actually fit.
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("View all in the platform", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("View details", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines renders the lark_md body of one finding.
//
// lark_md is in the same family as Markdown and also parses links and emphasis, so every external
// field goes through markdownText (one line, then escaped). Otherwise a finding title becomes a clickable external link in Feishu.
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**Status change**: %s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**Type**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**Assets**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**Summary**: %s", s)
		}
	}
	return out
}

// feishuBatchLine renders one line of a digest card.
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
