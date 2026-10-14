package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout and emailSessionTimeout bound the connection and the whole SMTP session.
// net/smtp has no timeout of its own. Without these, a stuck peer leaves the delivery goroutine hung forever.
// The dispatcher is a single goroutine that works serially, so that stalls the whole notification system.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel delivers notifications over SMTP.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but it should not be used to flood a mailbox. The default is generous.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only the password is masked. The SMTP host, account, and recipients are not secrets, and masking them only makes editing harder.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host and port decide which server receives the password. tls decides whether the transport is encrypted.
// Changing any of the three requires stating the password again, so turning TLS off cannot be a casual edit.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP server address is missing")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP port is invalid (use 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("sender address is missing")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("at least one recipient address is required")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: upgrade when the peer supports it. Credentials must not be sent on a cleartext session (see the auth note below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials on an unencrypted connection (unless the target is localhost).
			// That is the correct security behavior and must not be bypassed, but the reason has to be clear.
			// Otherwise the user only sees "unencrypted connection" and does not know what to change.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials: the connection is not encrypted. Enable TLS, use port 465 (implicit TLS), or turn on TLS (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP authentication failed: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("sender %s was rejected", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("recipient %s was rejected", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("failed to write the message body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("failed to submit the message: %w", err)
	}
	// A failed Quit does not change the fact that the server already accepted the message, so the error is ignored.
	_ = client.Quit()
	// Email does not truncate (the whole HTML body is sent), so the whole batch counts as delivered.
	return len(m.Items), nil
}

// emailDial opens an SMTP connection.
//
// implicitTLS=true is the "TLS from the first byte" style used by port 465. false connects in the clear on 25/587 and then uses STARTTLS.
// The two must not be mixed: a cleartext greeting to port 465 is dropped immediately.
//
// The session deadline is set at dial time, not patched on later, because net/smtp's Client hides the underlying
// connection in an unexported field and the caller cannot reach it. Once the connection is handed over, only a
// deadline set in advance can bound it. That also covers a handshake that blocks.
// Control installs blockInternalDial, the same guard the HTTP channels use. Without it, SMTP is the hole in the
// SSRF protection: a host of 169.254.169.254 or 127.0.0.1 connects directly, and a failed smtp.NewClient handshake
// wraps the peer's reply into the error, which last_error then shows through the delivery-history API. That is a
// half-blind read. The timing difference between "connection refused" and "timeout" can also probe ports.
// Dial time is the enforcement point, and it also covers DNS rebinding.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError splits a stage failure into retryable and permanent using the SMTP reply code.
//
// The split is required because SMTP 4xx and 5xx mean different things:
//   - 4xx (450 greylist, 451 local error, 452 out of storage) is a temporary refusal.
//     The normal response is to retry later. Greylisting in particular shows up on almost every first delivery.
//   - 5xx (550 user does not exist, 553 illegal address) is a permanent refusal. Retrying does nothing.
//
// Treating every failure as permanent would put every notification into failed after the first attempt on a
// greylisting server, which is exactly where automatic retry should help.
// The reply code is the first three digits of the error text. If there is no code, the failure is retryable:
// one extra attempt is better than killing a possible transient fault because the text could not be parsed.
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode reads the leading three-digit reply code from SMTP error text. It returns 0 when there is none.
// net/smtp does not export the code, so it has to come from the text. The form is "450 4.7.1 ...".
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage assembles a complete RFC 5322 message.
//
// The body is base64 for two reasons. SMTP limits a line to 1000 bytes, and an HTML body (especially a digest)
// easily produces a longer line. Base64 also never produces a line that starts with ".", so SMTP dot-stuffing is unnecessary.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// A non-ASCII subject must be RFC 2047 encoded, or the client shows it as mojibake.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length limit, so the body is not truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// Base64 is wrapped at 76 characters, as RFC 2045 requires.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
