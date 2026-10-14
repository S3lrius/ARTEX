package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg builds one message whose title and summary contain a quote and a newline.
// That is the input most likely to produce illegal JSON when a template interpolates it.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `login "SQL injection" risk`,
			VulnClass: "SQL injection",
			Severity:  "high",
			Summary:   "parameter id\nnot filtered, which causes injection",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg builds a digest.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "finding" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "reflected cross-site scripting",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost starts a fake receiver and hands the request body and headers to the assertion.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body is not valid JSON: %v\nraw: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("a back-link should send an actionCard, got %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("back-link was lost: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("a digest should send markdown, got %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "in the last 30 minutes") {
			t.Errorf("digest body is missing the time window: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent locks the "HTTP 200 but errcode is not 0" decision.
// Skipping the errcode check records a failed delivery as a success. That is a shared trap of these IM platforms.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("a non-zero errcode should be an error")
	}
	if !IsPermanent(err) {
		t.Fatalf("a keyword mismatch is a configuration error and should be permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("the error should include the platform error code, got %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("after truncation the text is not valid UTF-8, and WeCom would reject the whole message")
		}
	})
	// A long enough digest is past 4096 bytes.
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("body is %d bytes, over the WeCom limit %d", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("body is empty")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 is a rolling-window rate limit and should be retryable, got %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000 is an invalid key, a retry will not fix it, and it should be permanent, got %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("an interactive card should be sent, got %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high severity should use the orange color, got %v", header["template"])
		}
		// A configured secret must send signing fields, or Feishu rejects the message with 19021.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("signing fields are missing: %v", body)
		}
		// A card element should contain a button whose url points at the finding detail.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("the card has no button pointing at the detail page")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("signing fields should not be sent when secret is unset: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML parse mode should be used, got %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// The title and summary come from the target or from model output, and they are untrusted.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML was not escaped, so injection is possible: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("expected the escaped entity, got %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& was not escaped, got %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 should be retryable, got %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 is a configuration problem and should be permanent, got %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// This is why the default template exists. A quote or a newline in the title makes a naive
	// `"title": "{{.Title}}"` produce illegal JSON. {{json .}} does not.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 High] login "SQL injection" risk` {
			t.Errorf("title was not restored correctly: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items count should be 1, got %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "parameter id\nnot filtered, which causes injection" {
			t.Errorf("summary was not restored correctly: %v", it["summary"])
		}
		// A number must be a JSON number, not a string. A json:",string" tag falls into that trap.
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id should be a number, got %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("custom header was lost: %v", r.Header)
		}
		if body["msg"] != "3 items" {
			t.Errorf("custom template rendered incorrectly: %v", body["msg"])
		}
		if body["first"] != "finding1" {
			t.Errorf("range extraction is wrong: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d items" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a render that is not JSON should be a permanent failure (the template is wrong, and a retry does nothing), got %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("config set %d should be rejected: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("failed to assemble the message: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From header is wrong:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To header is wrong:\n%s", msg)
	}
	// A non-ASCII subject must be RFC 2047 encoded, or the client shows it as mojibake.
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("subject is not RFC 2047 encoded:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("subject could not be decoded: %v", err)
	} else if !strings.Contains(dec, "SQL injection") {
		t.Fatalf("decoded subject content is wrong: %q", dec)
	}

	// The body is base64, and decoding it should produce valid HTML.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("message is missing the header/body separator")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("body base64 decode failed: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("body is not HTML: %.80s", html)
	}
	// The title appears as text. A double quote is a legal character in HTML text and does not need escaping.
	// Asserting it is kept as-is stops a later change from escaping quotes and showing them as &quot;.
	if !strings.Contains(html, `"SQL injection"`) {
		t.Fatalf("quotes in the title should be kept as-is in text: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection covers the injection the email body actually has to stop.
// Finding titles and summaries come from the target and from model output, and they are untrusted.
// Text must escape & < > (or a tag can be injected), and attribute values must also escape quotes (or the href can be closed).
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("title was not escaped, so a tag can be injected: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("expected the escaped entity: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& and > were not escaped: %s", html)
	}
	// The back-link is built from the administrator's public_base_url and is relatively trusted, but quotes in
	// an attribute still have to be escaped. A quoted address would close the href and inject an event handler.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("the href attribute was not escaped correctly: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("a quote in an attribute should be escaped: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("header %s was not found", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// A validation error is shown to the person configuring the channel, so it has to say what is missing.
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "webhook"},
		{KindFeishu, map[string]any{}, "webhook"},
		{KindWeCom, map[string]any{}, "webhook"},
		{KindTelegram, map[string]any{}, "bot token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "chat id"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "port"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "recipient"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("channel %s is not registered", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s config %v should fail validation", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s error should mention %q, got %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification locks the difference between SMTP 4xx and 5xx.
// Treating 4xx as permanent would put every notification into failed after the first attempt on a greylisting server, which is exactly where automatic retry should help.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// When there is no reply code, treat it as retryable. One extra attempt is better than killing a possible transient fault.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("recipient was rejected", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("reply %q: expected permanent=%v, got %v", tc.reply, tc.permanent, got)
		}
		// Whatever the classification, the original text is kept for the user to debug.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("the original text of reply %q was dropped: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// All six channels are required. A missing one disappears from the UI dropdown with no warning.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("channel count should be %d, got %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("channel %s is not registered", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("channel %s Kind() does not match its registry key", k)
		}
	}
	if ValidKind("nope") {
		t.Error("an unregistered kind should not pass validation")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("should be recognized as a permanent failure")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("the error should pass the underlying message through: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must return nil")
	}
	if IsPermanent(nil) {
		t.Fatal("nil is not a permanent failure")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
