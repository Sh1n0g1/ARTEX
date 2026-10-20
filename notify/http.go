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

// allowLocalTargets decides whether delivering messages to loopback /
// link-local addresses is allowed.
//
// Rejected by default. These address ranges aren't where IM bots or public
// email servers live, and what they can reach is sensitive: another
// service's admin port on the same machine, and the cloud metadata endpoint
// (169.254.169.254, which can leak instance credentials). The delivery
// address is set by an admin, but an admin session hijacked via XSS/CSRF, or
// a second person sharing the same JWT, could read response content back by
// editing the config — doJSON writes the first 200 bytes of a 4xx/5xx
// response body into last_error, and the delivery-history endpoint echoes it
// back, making this a semi-blind read primitive.
//
// But "a local SMTP relay" (postfix on 127.0.0.1:25) is a common setup for
// self-hosted mail, and a blanket block would get people stuck. So an
// explicit escape hatch is left instead of a hardcoded allowance: setting
// ARTEX_NOTIFY_ALLOW_LOCAL=1 permits it.
//
// Exported as AllowLocalTargetsEnv so tests can explicitly turn it on — both
// this package's and the server package's test cases heavily use httptest
// fake receivers on 127.0.0.1, and without turning it on they'd all be
// blocked by the guard.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP belongs to an address range
// that's "not allowed to deliver to" by default.
//
// Only rejects loopback, link-local (including the cloud metadata
// 169.254.169.254), unspecified, and multicast. **Does not** reject RFC1918
// private networks: a self-hosted internal Mattermost / SMTP relay is a very
// common legitimate use, and blocking those too would make this feature
// unusable in real deployments. This trade-off is deliberate — the guard
// should stop genuinely sensitive targets without taking out normal
// deployments along with them.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) needs to be converted
	// back to IPv4 before judging it, or it bypasses the check.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook for the http.Transport dialer; it
// checks the target address at **connection-establishment time**.
//
// Why put this at the dial stage instead of only validating when the config
// is saved: this is where it ultimately takes effect. It also covers two
// ways of bypassing config-time validation — DNS rebinding (resolving to a
// public IP at validation time, then to an internal address at actual
// connection time) and redirects (we already reject cross-host redirects,
// but a same-host redirect could still point the path somewhere else).
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
		return fmt.Errorf("could not resolve target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to a loopback/link-local address %s (set %s=1 if delivery to a local service is genuinely needed)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport only adds a dial guard on top of the default Transport.
// Uses Clone to preserve all of the default tuning (connection pooling,
// HTTP/2, timeouts, proxy, etc.), avoiding changing other behavior just to
// add one check.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by every channel's delivery.
//
// Deliberately **not** reusing the project's global egress proxy (the server
// side's GlobalProxy): that proxy is meant for traffic to pentest targets and
// is often an unstable tunnel, while notification availability shouldn't be
// held hostage to a target network's instability. IM pushes connect
// directly. Timeout is set to 15 seconds — anything slower than that is
// effectively already a failure.
//
// Rejects cross-host redirects: this feature's delivery addresses are all a
// "fixed endpoint", and normally wouldn't redirect to another host; and
// these platforms' credentials (DingTalk's access_token, WeCom's key,
// Telegram's bot token) **are in the URL**, so following a cross-host
// redirect would hand the credential to the redirect target. A same-host
// redirect (e.g. appending a trailing slash) is still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps how much of the response body is read. The other end
// might return an oversized body on error, and we only need the status code
// and a short error description to show in the delivery history.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns the (length-limited) response body.
//
// Sends an empty body when payload is nil (for GET or platforms that don't
// require a body). Keys and values in headers are attached as-is, used for
// the generic webhook's custom headers.
//
// Error classification is this function's core responsibility: network-layer
// failures and 5xx/408/429 are classified as "retryable", the rest of the
// 4xx range as "permanent" — retrying a 403 would just log the same error
// three more times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// A serialization failure is a local bug (a config field has the
			// wrong type); retrying won't make it better.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// An invalid URL is most likely a user typo, and is a permanent
		// failure. Also can't pass err through as-is here: url.Parse's error
		// text includes the full address.
		return nil, Permanent(fmt.Errorf("invalid request address: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout — mostly transient
		// failures, handed off to retry with backoff.
		//
		// The error text must be redacted before propagating it. Reason:
		// http.Client.Do returns a *url.Error, whose Error() is
		// `Op "full URL": underlying error`, and for these platforms the
		// credential **is in the URL** (DingTalk's access_token, WeCom's
		// key, Feishu's hook id, Telegram's /bot<token>/). Without
		// redaction, the credential would flow out through this error string
		// to four places: notification_deliveries' last_error (stored in
		// plaintext), the delivery-history endpoint's response (**bypassing
		// the channel config's masking**), server logs, and the 502 text the
		// test-send endpoint returns to the frontend.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limited) and 408 (timeout) are worth retrying; the rest of
	// the 4xx range is a config or permission problem, where retrying is
	// pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("remote rate limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("remote service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("remote rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses the response body into a short single line, for use in
// error messages. The response may contain newlines and large amounts of
// whitespace, and stuffing it straight into last_error would break the
// delivery-history page's layout.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses a delivery address down to
// "scheme://host/…", for use in error messages.
//
// This is the package's only address-redaction approach, deliberately made
// **blunt**: everything except scheme and host is discarded. The reason is
// there's no "general and safe" way to tell which segment of a URL is the
// credential:
//
//	DingTalk  credential in query        /robot/send?access_token=xxx
//	WeCom     credential in query        /cgi-bin/webhook/send?key=xxx
//	Feishu    credential in **the last path segment** /open-apis/bot/v2/hook/<hook_id>
//	Telegram  credential in **a middle path segment** /bot<token>/sendMessage
//
// Trying to "keep only the useful part" would require a patch per channel,
// and missing any one of them means a credential leak. Keeping the host is
// already enough for troubleshooting (DNS failure, connection failure, bad
// certificate can all be pinpointed from it); which specific bot it is can
// be recognized from the masked tail hint in the channel config.
//
// Returns a fixed placeholder on a parse failure — never echoes the raw
// string back.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address could not be parsed)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address out of a transport-layer error,
// keeping only the underlying cause.
//
// *url.Error's structure is {Op, URL, Err}, and its Error() prints the URL
// along with everything else. This explicitly takes the Err field, bypassing
// its Error() — more reliable than a string replacement after the fact,
// since a replacement would have to correctly handle every URL-encoding/
// escaping variant, and it's easy to miss one.
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
	// A non-*url.Error (such as an error returned by the redirect policy) may
	// also carry an address; redact it the same way.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces any http(s) addresses appearing in a piece of
// text with their redacted form.
//
// Used as a fallback for errors that don't expose structured fields (a
// redirect-policy error, a third-party library's custom error). Only
// recognizes the http/https prefix, splitting on whitespace and quotes —
// an address won't contain either of those characters.
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
