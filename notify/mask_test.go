package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("the masked value leaked the full credential: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("the masked value should not expose the address body: %q", got)
	}
	// The last 6 characters stay so the user can tell which robot it is.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("the last 6 characters should be kept as a hint: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("a masked value must be recognized by IsMasked: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// If a short credential also exposed the last 6 characters, that would be the whole credential.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("a credential of length %d should not give a tail hint, got %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("the masked value contains the original: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s should be masked, got %q", k, s)
		}
	}
	// Non-credential fields must be kept as-is, or the UI cannot display them.
	if masked["port"] != float64(587) {
		t.Errorf("the non-credential field port should not change: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// When the channel kind cannot be recognized, an empty config in the UI is better than echoing raw content that may contain credentials.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("an unknown channel kind should return an empty config, got %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// Masking is a display behavior. It must not overwrite the real value stored in the database.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig changed its argument, which would overwrite the real credential with a masked value")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// The user only changed method. The browser submits the masked values plus the new method.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("a masked field should keep the stored original, got %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("the edited field should take effect, got %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("an empty string should clear the field, got %v", got)
	}
	// A field that was not mentioned is kept (partial update).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("a field that was not mentioned should be kept, got %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("a field that was not mentioned should be kept, got %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("a field that was mentioned should be updated, got %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap is the most important security invariant in this package:
// changing the destination must not carry the old credential along.
//
// These cases use the attack-shaped input (change only the address, say nothing about the credential),
// not the "correct input for the defense". Testing only the latter stays green even when the defense does nothing.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing is the credential key that should be named.
		wantMissing string
	}{
		{
			name: "generic webhook changes the address and tries to keep the Authorization header",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram changes base_url and tries to send the bot token to its own endpoint",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "email changes the SMTP host and tries to hand over the password",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "turning email TLS off also requires stating the password again",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// A masked value means "keep the old credential", and it must also be refused when the address changes.
			name:        "masked credential sent back with a new address",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk changes the webhook and tries to keep the signing secret",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("changing the address without stating the credential again should be refused; got config %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("a dedicated error type should be returned so the API can give an actionable hint, got %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("the missing credential key %q should be named, got %v", tc.wantMissing, target.Missing)
			}
			// The error should tell the operator how to fix it.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("the error should mention %q: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits is the reverse case. A normal edit must not be blocked by mistake, or this protection gets bypassed or deleted because it is too annoying.
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "only the name changes (config sent back as-is)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "only the request method changes; address and credentials stay",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "the address changes and a new credential is provided at the same time",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "the address changes and the caller explicitly says a credential is no longer needed",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram changes chat_id (not a destination)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "email changes the recipient (not a destination)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("a legitimate edit was blocked by mistake: %v", err)
			}
			if merged == nil {
				t.Fatal("a merged result should be returned")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance covers an easy misjudgment.
// The frontend submits a port as a JSON number (float64), and the database reads it back as float64,
// but the two values can have different types (int versus float64). Comparing with == treats "unchanged" as "changed"
// and asks a user who only renamed the channel to re-enter the password. A false alarm makes people stop trusting the protection.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// The same port, submitted as an int.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("the same port value (only the type differs) should not be judged as an address change: %v", err)
	}
	// A real port change must be blocked.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("a port change should be blocked")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination covers an optional destination field.
// An empty Telegram base_url means the official API address.
//
// This used to make the channel permanently fail to save from the second save on:
//
//	a create stores base_url:"" (the create path stores the frontend config directly and does not go through MergeConfig)
//	→ the first save treats the empty string as an explicit clear and MergeConfig deletes the key
//	→ the second save still has incoming "", stored no longer has the key, and that is judged as "the address changed"
//	→ bot_token is a masked echo → 400 "the destination changed; re-enter the credential fields"
//
// The user changed nothing, and from then on the save fails unless they paste the bot token again.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// The frontend buildConfig() submits a value for every field of this channel: credentials come back masked,
	// and an empty text box submits an empty string. This reproduces that output, rather than submitting only the changed keys.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// First save: only the channel name changed, and the config is sent back as-is.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("the first save was blocked by mistake: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("the premise changed: an empty string should be deleted by MergeConfig. This case covers the step after the key disappears")
	}

	// Second save: the submission is identical to the last one. The user changed nothing.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("the second save was blocked by mistake (the user changed nothing): %v", err)
	}
	// A third save confirms it is stably saveable, not a one-off miss.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("the third save was blocked by mistake: %v", err)
	}
	// The credential must survive the whole way and not be cleared by the empty-string logic.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("the bot token should keep the original value, got %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges is the paired assertion for the case above.
// Treating an empty string and a missing key as equivalent must not also let a real address change through.
// Both directions are real credential-exfiltration paths. Telegram's bot token travels in the URL path,
// and changing base_url hands the token to the new address.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// Direction one: from empty (the official address) to a self-hosted address.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("switching from the official address to a self-hosted address must require the token to be entered again")
	}

	// Direction two: clearing a self-hosted address (switching back to the official API) is also an address change.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("clearing a self-hosted address (switching back to the official API) is also an address change and must require the token to be entered again")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// Same idea as SecretKeys: if a channel forgets to declare its destination keys, PrepareConfigUpdate cannot protect it.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("channel %s declares no destination keys, so the protection against carrying a credential to a new address does not apply to it", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s declares no credential keys", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// The compiler already forces every channel to implement SecretKeys. This confirms again that no channel
	// turns in a blank on masking. A channel that returns an empty slice would echo its credentials to the browser in the clear.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("channel %s has no masking expectation recorded in the test", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s declares no credential fields, so its config would be echoed in the clear", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer covers a hole an audit pointed out.
// Nesting the mask sentinel inside a non-string structure (webhook.headers is an object) is not treated as a mask,
// because MergeConfig only treats a string with the prefix as a mask. The literal "__masked__" would then be stored
// as a real header value, later auth would fail silently, and nothing would report an error.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// A mask sentinel nested inside the object.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("a mask sentinel nested inside a structure should be refused (otherwise the literal is stored)")
	}
	// Submitting the object as a whole (a real new value) is still accepted.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("submitting a new header normally should not be blocked: %v", err)
	}
}
