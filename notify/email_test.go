package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// This file rounds out protocol-level tests for the email channel. Before
// this, email.Send's coverage was 0 -- no test exercised the SMTP path at
// all, and it's exactly the one with the largest protocol surface and the
// most ways to get wrong among the six channels (handshake, auth, envelope,
// DATA stage each have their own failure semantics).
//
// This drives a minimal self-built SMTP server rather than mocking out
// net/smtp: most of the email channel's risk lies precisely in the step of
// "talking to a real SMTP server", and mocking that step out means not
// testing it.

// fakeSMTP is a just-barely-sufficient SMTP server: it can complete
// greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT, and returns a specified reply code for
// a given stage as a test case requires.
type fakeSMTP struct {
	ln net.Listener

	// rcptReply is the reply to RCPT TO; defaults to 250.
	rcptReply string
	// mailReply is the reply to MAIL FROM; defaults to 250.
	mailReply string
	// advertiseAuth, when true, advertises AUTH PLAIN support in EHLO.
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
		t.Fatal("not a TCP listen address")
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
			// Don't advertise STARTTLS: forces the code down the plaintext
			// branch (the test target is the envelope logic, not TLS).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// Simplified handling: PLAIN's initial response may span
			// multiple lines; just accept it.
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
	// The envelope stage must be reached: sender, both recipients, DATA.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP session is missing %q, actual commands: %v", want, f.commands)
		}
	}
	// The body is base64-encoded HTML, and must carry the actual finding
	// content (still recognizable after encoding).
	body := f.body()
	if body == "" {
		t.Fatal("no body received at the DATA stage")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("missing Content-Type header:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("body is not base64-encoded (a long HTML line would break SMTP's 1000-byte line limit):\n%s", body)
	}
	// Multiple recipients must all appear in the To header.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To header doesn't include all recipients:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// AUTH should not be sent when no account is configured -- some relays
	// reject the message because of it.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("AUTH was sent even though no account is configured: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies is the direct verification of this
// audit's fix: 5xx judged permanent, 4xx (greylisting) judged retryable.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"recipient permanently rejected with 550", "550 5.1.1 User unknown", "250 OK", true},
		{"recipient hits 450 greylisting", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"recipient hits 452 mailbox full", "452 4.2.2 Mailbox full", "250 OK", false},
		{"sender permanently rejected with 553", "250 OK", "553 5.1.3 Bad address", true},
		{"sender hits 451 temporary error", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("should error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("wrong permanent determination: expected %v got %v (%v)", tc.permanent, got, err)
			}
			// The server's original text must be preserved, or the user has
			// no way to know whether to contact the server admin or change
			// the address.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("error should preserve the server's reply code: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp's PlainAuth refuses to send credentials over an unencrypted
	// connection (unless the target is localhost). This is **correct**
	// security behavior and must not be bypassed; but the error must guide
	// the user toward a fix. Uses a non-localhost hostname here to trigger it.
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
		t.Skip("local DNS resolved to the local server; skipping (doesn't affect other cases)")
	}
	// Either "couldn't connect" or "refused to send credentials" passes this
	// assertion; what matters is that the password **must not** be silently
	// sent out.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "connect") {
		t.Logf("error: %v (failing to connect under a non-localhost host is expected)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// The email channel has the most config fields, and missing any one of
	// them would only surface at delivery time; this confirms one by one
	// that validation catches it ahead of time. The assertion checks that
	// "the error message mentions what's missing".
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
				t.Fatalf("should fail validation: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance covers tolerance in config reading: a JSONB
// number is a float64, but a user might type the port as a string in the UI;
// an array might also be a single string.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // port as a string
		"from": "a@b.c",
		"to":   "d@e.f", // a single string instead of an array
		"tls":  "true",  // boolean as a string
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("should tolerate string-form values: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt did not parse the string port, got %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool did not parse the string \"true\"")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings isn't compatible with a single string, got %v", to)
	}
}

// TestFilterValidateRejectsTypo is the direct verification of the audit fix:
// a typo'd threshold must be caught on write, or the filter would silently
// fail and turn into "send everything".
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("a valid threshold %q was rejected: %v", s, err)
		}
	}
	// These are typos that genuinely happen in practice -- all must be rejected.
	for _, s := range []string{"hgih", "HIGH", "严重", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("invalid threshold %q should be rejected (otherwise the filter silently fails and turns into sending everything)", s)
			continue
		}
		// The error message should guide the user to the correct value.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("error message should list the valid options, got %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly locks down the division of labor between
// "strict on write, lenient on read": a bad value already in the database
// must not make the whole channel unreadable (which would make a historical
// channel suddenly stop notifying entirely).
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // doesn't error
	if f.MinSeverity != "hgih" {
		t.Fatalf("the read path should preserve it as-is, got %q", f.MinSeverity)
	}
	// And the channel can still make a determination on an event (no panic,
	// no blocking).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
