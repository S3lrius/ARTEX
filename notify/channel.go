package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for one notification channel. Implementations must be stateless:
// one instance is reused concurrently by many channel configs, and every credential comes in through cfg.
type Channel interface {
	// Kind returns the channel type identifier. It must match the registry key.
	Kind() string
	// Validate runs when a config is saved. It checks required fields and format.
	// The error is shown to the person configuring the channel, so it should name the missing field.
	Validate(cfg map[string]any) error
	// Send delivers one message and returns how many items actually went out, plus an error.
	//
	// The count matters because every platform caps message length and a digest is truncated when the batch does not fit.
	// If the caller marked the whole batch delivered, the truncated items would vanish: they are not in the message,
	// and the delivery history would still say success, so nothing would show that the findings were never sent.
	// With kept, the caller marks only the first kept items and leaves the rest for the next batch.
	//
	// An error means delivery failed. *PermanentError means it should not be retried.
	// On failure kept is meaningless and the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the per-minute delivery cap the platform recommends, used as the default rate limit for a new channel instance.
	// Zero means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the config keys that hold credentials. API responses mask those values,
	// and an update that sends the masked value keeps the stored original.
	// Only the implementation knows which fields are secrets
	// (the whole WeCom webhook URL is a credential, while DingTalk's secret is only the secret field),
	// so the channel has to supply that knowledge. The caller must not guess.
	SecretKeys() []string
	// DestinationKeys returns the config keys that decide where the message is sent.
	//
	// Like SecretKeys, this is a security boundary: the destination and the credential are separate fields.
	// If someone could change only the address and keep the stored credential, anyone who can edit the channel
	// could send the real credential to a server they control, and masking the config would mean nothing.
	// See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. An explicit literal is used instead of init() self-registration
// so every channel is visible in one place, and a missing registration fails at compile time rather than as a runtime side effect.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get returns the channel implementation for a kind.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns every supported channel type, sorted, so a UI dropdown stays stable.
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that must not be retried: bad credentials, a refused target, an illegal body.
// Retry only helps transient faults (network jitter, rate limits, a remote 5xx). Retrying a permanent failure
// never succeeds, and it buries the real error in retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. A nil err returns nil, so callers can write `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the error chain carries a permanent-failure mark.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// Config readers.
//
// Channel config comes from a JSONB column. After encoding/json it is map[string]any:
// numbers are float64 and arrays are []any. These helpers normalize that, and they tolerate
// type drift from a blank form field (a port submitted as a string, for example).

// cfgString reads a string config value and trims surrounding space. A paste from a web form often includes it.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an integer config value, accepting both float64 (the JSON default) and a string.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a boolean config value, also accepting the strings "true" and "1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-array config value, trims each entry, and drops empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// A single string is also accepted, for a form that submits one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string map (custom HTTP headers, for example). Keys and values are trimmed, and empty keys are dropped.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
