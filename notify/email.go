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

// emailDialTimeout / emailSessionTimeout bound connection setup and the
// whole SMTP session respectively. net/smtp itself has no timeout mechanism;
// without these two, a stuck remote end would leave the delivery goroutine
// hanging forever — and since the dispatcher processes things serially on a
// single goroutine, that means the entire notification system grinds to a halt.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP email delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Email has no platform rate limit, but shouldn't be used to flood either;
// use a lenient default.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Only the password is masked. The SMTP host, username and recipients aren't
// secrets; masking them would only make editing more cumbersome.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port determine which server the password is handed to; tls decides
// whether the transport is encrypted. Any change to these three requires
// re-stating the password — which incidentally makes "turning off TLS" a
// step that must explicitly carry a credential, rather than a casual edit.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("missing SMTP server address")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("invalid SMTP port (must be 1-65535)")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("missing sender address")
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

	// STARTTLS: upgrade if the remote end supports it. Credentials can't be
	// sent over a plaintext session (see the auth note below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS failed: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials over an unencrypted
			// connection (unless the target is localhost). This is
			// **correct** security behavior and must not be bypassed, but
			// the reason needs to be translated clearly — otherwise the user
			// just sees "unencrypted connection" with no idea what to do
			// about it.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("refusing to send credentials: the connection isn't encrypted. Enable TLS, switch to port 465 (implicit TLS), or check "+
					"\"Enable TLS\" (%w)", err))
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
		return 0, fmt.Errorf("failed to write email body: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("failed to submit email: %w", err)
	}
	// A Quit failure doesn't affect the fact that "the server already
	// accepted the email", so its error is ignored.
	_ = client.Quit()
	// Email has no length truncation (the full HTML body is sent), so the
	// whole batch counts as delivered.
	return len(m.Items), nil
}

// emailDial establishes the SMTP connection.
//
// implicitTLS=true takes the "TLS from the moment of connecting" path used
// by port 465; false connects in plaintext on 25/587 and then does STARTTLS.
// The two can't be mixed: sending a plaintext greeting to port 465 gets the
// connection dropped immediately.
//
// The session deadline is set **at connection time** (rather than added
// later), because net/smtp's Client hides the underlying connection in an
// unexported field, so it's unreachable from outside; once the connection is
// handed off, only a pre-set deadline can backstop it. This also happens to
// cover blocking during the handshake.
// Control is wired to blockInternalDial, sharing the same guard as the
// HTTP-based channels. Without it, SMTP would be the one gap in the whole
// SSRF defense: setting host to 169.254.169.254 or 127.0.0.1 would connect
// directly, and when smtp.NewClient's handshake fails, it wraps the remote
// end's response line into the error, echoed back by the delivery-history
// endpoint via last_error — a semi-blind read primitive; the timing
// difference between "connection refused" and "timeout" can also be used to
// probe ports. The dial stage is where this ultimately takes effect, and it
// also covers DNS rebinding.
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
		return nil, fmt.Errorf("failed to connect to SMTP server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP handshake failed: %w", err)
	}
	return client, nil
}

// smtpStageError classifies a stage's failure as "retryable" or "permanent"
// based on the SMTP reply code.
//
// Why this distinction is necessary: SMTP's 4xx and 5xx have completely
// different semantics —
//   - 4xx (450 greylisting, 451 local error, 452 insufficient storage) is a
//     **temporary** rejection, and the proper approach is to retry later;
//     greylisting especially is hit on almost every first delivery attempt.
//   - 5xx (550 user unknown, 553 bad address) is a permanent rejection,
//     where retrying is pointless.
//
// If everything were judged a permanent failure, a mail server with
// greylisting enabled would have **every single** push land in failed after
// the first attempt — and that's exactly the scenario where automatic
// retrying should be doing its job. The reply code is taken from the leading
// three digits of the error text; when no code can be extracted, it's
// treated as retryable (better to try once more than to declare a possibly
// transient failure dead just because it couldn't be parsed).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode extracts the leading three-digit reply code from an SMTP
// error text, returning 0 if none is found. net/smtp doesn't export an error
// code field, so it has to be pulled from the text; the format is
// "450 4.7.1 ...".
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

// buildEmailMessage assembles a complete RFC 5322 email.
//
// The body is base64-encoded for two reasons: first, SMTP requires each line
// to be no more than 1000 bytes, and the HTML body (especially for a digest
// email) easily produces overlong lines; second, base64 naturally never
// produces a line starting with ".", avoiding the trouble of SMTP dot
// escaping.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// A non-ASCII subject must use RFC 2047 encoding, or email clients will
	// display it as garbled text.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// Email has no hard length limit, so the body isn't truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 is line-wrapped at 76 characters, per RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
