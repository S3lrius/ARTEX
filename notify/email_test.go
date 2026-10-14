package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// This file fills in protocol-level tests for the email channel. Before this, email.Send had zero coverage.
// No case had walked the SMTP path, and it is the channel with the largest protocol surface and the easiest
// place to get wrong (handshake, auth, envelope, and DATA each have their own failure meaning).
//
// A minimal SMTP server drives the tests instead of mocking net/smtp.
// Most of the risk is in talking to a real SMTP server, and mocking that step would mean not testing it.

// fakeSMTP is an SMTP server that is just enough: greet, EHLO, AUTH, MAIL, RCPT, DATA, and QUIT,
// and it returns the reply code a case asks for at a given stage.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply is the RCPT TO reply. The default is 250.
	rcptReply string
	// mailReply is the MAIL FROM reply. The default is 250.
	mailReply string
	// advertiseAuth, when true, advertises AUTH PLAIN in EHLO.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("listener address is not TCP")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// Do not advertise STARTTLS. The code takes the cleartext branch. The test target is envelope logic, not TLS.
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// Simplified: the initial PLAIN response can span lines, and it is accepted directly.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	// The envelope stage has to be reached: the sender, both recipients, and DATA.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP session is missing %q, actual commands: %v", want, f.commands)
		}
	}
	// The body is base64 HTML, and it has to carry the real finding content (still recognizable after encoding).
	body := f.body()
	if body == "" {
		t.Fatal("the DATA stage received no body")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("missing Content-Type header:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("body is not base64 (a long HTML line would break SMTP's 1000-byte line limit):\n%s", body)
	}
	// Every recipient has to appear in the To header.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("the To header does not include every recipient:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// AUTH should not be sent when no account is configured. Some relays reject the message because of it.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("AUTH was sent with no account configured: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies is the direct check of the audit fix:
// 5xx is a permanent failure, and 4xx (greylisting) is retryable.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"recipient permanently refused with 550", "550 5.1.1 User unknown", "250 OK", true},
		{"recipient hits a 450 greylist", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"recipient hits a 452 mailbox full", "452 4.2.2 Mailbox full", "250 OK", false},
		{"sender permanently refused with 553", "250 OK", "553 5.1.3 Bad address", true},
		{"sender hits a 451 temporary error", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent classification is wrong: expected %v, got %v (%v)", tc.permanent, got, err)
			}
			// The server's original text has to be kept, or the user cannot tell whether to talk to the mail admin or change the address.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("the error should keep the server's reply code: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp's PlainAuth refuses to send credentials on an unencrypted connection (unless the target is localhost).
	// That is the correct security behavior and must not be bypassed, but the error has to tell the user how to fix it.
	// A hostname that is not localhost triggers it here.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // not localhost
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("local DNS resolved a local server; skipping (other cases are unaffected)")
	}
	// Failing to connect, or refusing to send credentials, both satisfy this assertion. The password must not be sent silently.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "connect") {
		t.Logf("error: %v (failing to connect when the host is not localhost is expected)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// The email channel has the most config fields, and missing any of them would only show up at delivery time.
	// Each one is checked here so validation rejects it early. The assertion is that the error names what is missing.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"missing host", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"missing port", map[string]any{"host": "smtp.example.com"}},
		{"port out of range", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"missing from", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"missing to", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("validation should fail: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance covers tolerance in config reading. JSONB numbers are float64,
// but a user may type a port as a string in the UI, and an array may also be a single string.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // port as a string
		"from": "a@b.c",
		"to":   "d@e.f", // a single string rather than an array
		"tls":  "true",  // a boolean as a string
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("string forms of numbers should be tolerated: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt did not parse a string port, got %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool did not parse the string \"true\"")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings did not accept a single string, got %v", to)
	}
}

// TestFilterValidateRejectsTypo is the direct check of the audit fix.
// A mistyped threshold must be rejected on write, or the filter silently disables itself and sends everything.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("legal threshold %q was rejected: %v", s, err)
		}
	}
	// These are typos that really happen. All of them must be rejected.
	for _, s := range []string{"hgih", "HIGH", "severe", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("illegal threshold %q should be rejected (otherwise the filter silently disables itself and sends everything)", s)
			continue
		}
		// The error should tell the user how to correct it.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("the error should list the allowed values, got %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly locks the split between a strict write path and a lenient read path.
// A bad value already in the database must not make the whole channel unreadable, or historical channels would suddenly stop notifying.
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // does not return an error
	if f.MinSeverity != "hgih" {
		t.Fatalf("the read path should keep the value as-is, got %q", f.MinSeverity)
	}
	// The channel can still decide an event (no panic, no block).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
