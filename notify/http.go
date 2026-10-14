package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether messages may be delivered to loopback or link-local addresses.
//
// The default is to refuse. Those ranges are not where an IM robot or a public mail server lives, and what they
// can reach is sensitive: another service's admin port on the same machine, and a cloud metadata endpoint
// (169.254.169.254, which can read instance credentials). An administrator sets the delivery address, but a
// management session borrowed through XSS or CSRF, or a second person sharing the same JWT, can change the
// config and read the response back. doJSON writes the first 200 bytes of a 4xx/5xx body into last_error, and
// the delivery-history API echoes it. That is a half-blind read.
//
// A local SMTP relay (postfix on 127.0.0.1:25) is a common self-hosted mail setup, and a hard block would
// stop those users. The escape hatch is explicit rather than a hardcoded allow: set ARTEX_NOTIFY_ALLOW_LOCAL=1.
//
// AllowLocalTargetsEnv is exported so tests can turn it on deliberately. This package and the server package
// use httptest receivers on 127.0.0.1, and without the flag the guard would block all of them.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP is in a range that delivery refuses by default.
//
// Only loopback, link-local (including the cloud metadata address 169.254.169.254), unspecified, and multicast are refused.
// RFC1918 private networks are not refused. A self-hosted Mattermost or SMTP relay on an internal network is a common legitimate use, and blocking it would make the feature unusable in a real deployment. The guard blocks the sensitive targets and leaves a normal deployment working.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) is reduced to IPv4 before the check, or it would bypass it.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the http.Transport dialer's Control hook. It checks the target address when the connection is opened.
//
// The check is at dial time, not only when the config is saved, because dial time is the enforcement point.
// It covers two ways around a save-time check: DNS rebinding (the check resolved a public IP, the real connection resolves an internal one) and a redirect (cross-host redirects are already refused, but a same-host redirect can still point the path somewhere else).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot resolve target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to the loopback or link-local address %s (set %s=1 if delivery to a local service is required)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport is the default Transport plus a dial guard.
// Clone keeps the default tuning (connection pool, HTTP/2, timeouts, proxy, and the rest) so adding one check does not change the other behavior.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by every channel delivery.
//
// It deliberately does not reuse the project's global egress proxy (GlobalProxy on the server side). That proxy
// is for traffic aimed at a penetration-test target and is often an unstable tunnel. Notification availability
// should not depend on jitter in the target network. IM delivery connects directly. The timeout is 15 seconds.
// A peer slower than that is already in a failed state.
//
// Cross-host redirects are refused. These delivery addresses are a single fixed endpoint, and a normal one does
// not redirect to another host. The credentials for these services (DingTalk access_token, WeCom key, Telegram
// bot token) are in the URL, so following a cross-host redirect would hand the credential to the redirect target.
// A same-host redirect, such as a trailing slash, is still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing a cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps how much of the response body is read. A failing peer can return a huge body, and only the error code plus a short description are needed for the delivery history.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns the response body, already length-limited.
//
// A nil payload sends an empty body (GET, or a platform that does not require a body).
// Header keys and values are attached as given, for a generic webhook's custom headers.
//
// Classifying errors is the point of this function. A network failure and 5xx/408/429 are retryable.
// Other 4xx responses are permanent. Retrying a 403 only writes the same error into the log three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// A marshal failure is a local bug (a config field has the wrong type), and a retry will not fix it.
			return nil, Permanent(fmt.Errorf("failed to build the request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// An illegal URL is usually a mistyped address, and it is a permanent failure.
		// Do not pass err through: url.Parse's error text contains the full address.
		return nil, Permanent(fmt.Errorf("request address is invalid: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, and timeout are usually transient and go to backoff retry.
		//
		// The error text has to be redacted before it leaves. http.Client.Do returns a *url.Error whose
		// Error() is `Op "full URL": underlying error`, and these credentials live in the URL
		// (DingTalk access_token, WeCom key, Feishu hook id, Telegram /bot<token>/).
		// Without redaction the credential flows to four places: last_error on notification_deliveries
		// (stored in the clear), the delivery-history API response (which bypasses channel-config masking),
		// server logs, and the 502 text the test-send endpoint returns to the frontend.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read the response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limit) and 408 (timeout) are worth retrying. Other 4xx responses are configuration or permission problems, and a retry does nothing.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("the peer rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("the peer failed (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("the peer rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet collapses a response body to one short line for an error message. The body may contain newlines and lots of whitespace, and dropping it straight into last_error breaks the delivery-history layout.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget reduces a delivery address to "scheme://host/…" for error messages.
//
// This is the only address-redaction rule in the package, and it is deliberately coarse: everything except the
// scheme and host is dropped. There is no general and safe way to decide which part of a URL is the credential:
//
//	DingTalk  credential in the query          /robot/send?access_token=xxx
//	WeCom     credential in the query          /cgi-bin/webhook/send?key=xxx
//	Feishu    credential in the last path part /open-apis/bot/v2/hook/<hook_id>
//	Telegram  credential in the middle of the path /bot<token>/sendMessage
//
// Keeping "only the useful part" would mean a patch per channel, and missing one channel is a credential leak.
// The host is enough to debug (DNS failed, connection failed, certificate mismatch). Which robot it was is identified by the masked tail in the channel config.
//
// A parse failure returns a fixed placeholder. The original string is never echoed.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address could not be parsed)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address out of a transport error and keeps the underlying cause.
//
// A *url.Error is {Op, URL, Err}, and Error() prints the URL with it.
// This reads the Err field and avoids Error(). That is more reliable than a later string replace, which has to handle every URL-encoded and escaped variant and is easy to miss.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Something that is not a *url.Error (a redirect-policy error, for example) can still carry an address, so it goes through the same redaction.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces http(s) addresses in a piece of text with the redacted form.
//
// It is the fallback for errors that have no structured fields (a redirect-policy error, or a custom error from a third-party library).
// Only an http or https prefix is recognized, and the address is split on whitespace and quotes. An address does not contain those characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
