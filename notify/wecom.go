package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard cap on a WeCom group-bot markdown content field (bytes, not characters).
// It is the tightest limit of the six channels, and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom group robot.
//
// Platform behavior:
//   - It authenticates only with the key on the URL and does not support a signature, so the webhook address is the entire credential.
//   - markdown content is capped at 4096 bytes. Over that, the whole message is rejected, not truncated. Multi-byte text uses 3 bytes per character, so the body holds only a little over a thousand characters and the client must truncate.
//   - The rate limit is 20 messages per minute, also enforced by the client.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only the webhook credential (the key on the URL) and does not support a signature.
// The whole address is the credential, so there is no other field to mask.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only the webhook field. It is both the destination and the credential, so there is no leftover credential after the address changes.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("webhook address is missing")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("webhook address is invalid: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest can be long (50 items times one line each, plus a prefix) and easily exceeds 4096 bytes.
	// Truncate here instead of waiting for the platform to reject it: a rejection drops the whole batch, while truncation at least delivers the first items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 means the API call limit was exceeded. The platform's rate window rolls forward, so a retry after backoff works, and this is explicitly retryable. Reaching here means the client's rate_per_min is too aggressive. Retry is only a backstop; the real fix is to lower that channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limit %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means the webhook key is invalid. That is permanent; a retry will not heal it.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
