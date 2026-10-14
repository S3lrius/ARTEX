package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// This file is an invariant test: any error text that comes out of a channel implementation must not carry a credential.
//
// It is its own file because the first channel cases only covered the success path and platform business errors,
// and never looked at transport failures. A transport error (connection refused, DNS failure, timeout) is the
// dangerous one. The *url.Error returned by http.Client.Do puts the full URL into the error text, and these
// credentials live in the URL. The credential then flows to four exits:
//
//	notification_deliveries.last_error  → stored in the clear
//	GET /api/notify/deliveries response → bypasses channel-config masking and is echoed to the browser
//	server logs                         → often shipped off the host
//	the test-send endpoint's 502        → shown directly in the frontend
//
// So this does not test one function. It really sends one request that must fail on each channel, and asserts the error text does not contain the credential.

// credentialCases covers every channel shape whose credential is in the URL:
// DingTalk and WeCom put it in the query, Feishu in the last path part, Telegram in the middle of the path.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk access_token in the query",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom key in the query",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu hook id in the last path part",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token in the middle of the path",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk signing secret",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken is a sentinel that cannot be a real credential, used to search error text.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials is the core invariant.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// A peer that must fail: nothing listens on 127.0.0.1:1, so this is the connection-refused path.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "leak probe"}},
			})
			if err == nil {
				t.Fatal("an unreachable address should be an error")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath covers the permanent-failure branch.
// A failed URL check and a platform business error also leave the process, and they must not carry a credential either.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// A credential in an illegal address hits the validateHTTPURL / url.Parse branch.
		{"DingTalk address is illegal", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"WeCom address is illegal", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Feishu address is illegal", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API address is illegal", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"generic webhook address is illegal", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("an illegal config should be an error")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("the error text leaked credential %q:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q, expected %q", in, got, want)
		}
		// The redacted result must not still contain any path or query fragment of the original address.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("the redacted result still contains path or query fragment %q: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// An unparseable input must never echo the original string.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("unparseable input %q was echoed as %q", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL watches the concrete *url.Error type.
// It is what http.Client.Do returns, and it is where a leak starts.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("the host should be kept for debugging, got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("the underlying cause should be kept for debugging, got %q", got)
	}
	// Op is kept too. POST versus GET matters when debugging.
	if !strings.Contains(got, "Post") {
		t.Errorf("the operation name should be kept, got %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback is the fallback path. A custom error that is not a *url.Error
// (a redirect-policy error, for example) still has its addresses removed.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("refusing a cross-host redirect (a.example → http://b.example/bot%s/send)", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("the address should be replaced with the redacted form, got %q", got)
	}
	// Text with no address is kept as-is.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("text with no address should not be changed")
	}
}

// TestCrossHostRedirectRefused covers "credential in the URL + following a cross-host redirect = handing over the credential".
// Two httptest servers listen on different ports of 127.0.0.1. A different port is a different Host, which is a cross-host redirect.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("a cross-host redirect should be refused")
	}
	if hit {
		t.Fatal("the redirect target was visited, so the credential left with the redirect")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed is the reverse case. A same-host redirect (a trailing slash, for example) must still work, or a normal workflow is blocked with it.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// A redirect on the same host and the same port.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("a same-host redirect should not be refused: %v", err)
	}
}
