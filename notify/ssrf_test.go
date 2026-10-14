package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardenings:
//   - A delivery address must not use the server as a pivot into an internal network or cloud metadata (SSRF).
//   - An address-validation error must not carry a credential that was in the address.
//
// About the test environment: many cases in this package use an httptest receiver on 127.0.0.1, and the guard
// would block them by default. TestMain turns AllowLocalTargetsEnv on for the package, and each SSRF case
// clears it so the assertion is the default refusal.

func TestMain(m *testing.M) {
	// Let ordinary cases reach a local fake receiver. SSRF cases clear the flag themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core SSRF assertion:
// with the default config, delivery to a loopback address must be refused at the connection layer.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // closing the escape hatch is the default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("delivery to a loopback address should be refused by default")
	}
	if hit {
		t.Fatal("the request reached a local service, so the guard did not take effect")
	}
	// The error should tell the user how to opt in. A local SMTP relay is a legitimate config.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal should explain how to opt in explicitly: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case. After an explicit opt-in it must work.
// Otherwise a local postfix or an internal relay, both legitimate deployments, would be blocked outright.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work after an explicit opt-in: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // cloud metadata endpoint, the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // an IPv4-mapped form must be reduced before the check, or it is a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be refused", s)
		}
	}
	// RFC1918 private networks are deliberately allowed. A self-hosted Mattermost or SMTP relay on an internal network is a common legitimate use.
	// This assertion pins that choice down. If someone later adds a private-network check, this fails and forces a conscious decision.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (a private network is a common legitimate delivery target)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the hint at config time:
// a literal IP should be refused when the config is saved, not on the first failed delivery.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at config time", raw)
		}
	}
	// A public address and a private address still pass. Private networks are left to dial time, which does not block them.
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials covers a branch an audit said the previous pass missed.
//
// When url.Parse fails it returns a *url.Error whose Error() contains the full original address. The previous
// pass only redacted the error returned by http.Client.Do and missed this one. The "permanent failure" cases
// added then (file://, gopher://, ftp://) all parse successfully and take the scheme branch, so a green run
// did not prove this path was safe.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // illegal percent-encoding
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // port is not a number
		"http://[::1?access_token=" + leakProbeToken,                  // unmatched bracket
	}
	for _, raw := range cases {
		// First confirm this input really makes url.Parse fail. Without that, the case can
		// take another branch without anyone noticing, which is how the earlier false assurance happened.
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should fail to parse, or this case does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// The channel wrapper must not carry the address out either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("an illegal address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the SMTP channel's dial guard.
//
// The email channel used to dial with a bare net.Dialer, which was the only hole in the SSRF protection:
// a host of 169.254.169.254 or 127.0.0.1 connected directly, and a failed smtp.NewClient handshake wrapped
// the peer's reply into the error, which last_error then showed through the delivery-history API. That is
// the half-blind read the other channels already closed. The timing difference between "connection refused"
// and "timeout" can also probe ports.
//
// TestMain turns AllowLocalTargetsEnv on for the package (many cases use a fake receiver on 127.0.0.1),
// so this case has to clear it. Otherwise it would pass whether or not the guard exists, which is why the hole was not caught by any test.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // closing the escape hatch is the default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("email delivery to a loopback address should be refused by default")
	}
	// The connection should never be established. The guard stops it in the Control hook, so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("an SMTP session was established, so the guard did not take effect")
	}
	// The error should tell the user how to opt in. A local postfix relay is a legitimate config.
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal should explain how to opt in explicitly: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse case. After an explicit opt-in,
// delivery must work. A self-hosted SMTP server or a local relay is a very common deployment, and the guard must not block it outright.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP should be deliverable after an explicit opt-in: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO was not seen, so the session was never established")
	}
}
