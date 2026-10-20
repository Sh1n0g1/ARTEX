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

// This file covers two related hardening measures:
//   ① a delivery address must not let the server be used as a pivot to hit
//      an internal network / cloud metadata (SSRF)
//   ② address validation errors must not carry a credential out of the address
//
// On the test environment: a large number of this package's cases use
// httptest fake receivers on 127.0.0.1, which the guard would block by
// default. So TestMain turns AllowLocalTargetsEnv on globally, while each
// SSRF case below explicitly clears it to assert the **reject by default**
// behavior.

func TestMain(m *testing.M) {
	// Lets the regular cases connect to local fake receivers; SSRF cases
	// clear it themselves temporarily.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the SSRF protection's core
// assertion: under the default config, delivering to a loopback address must
// be rejected at the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // turn off the escape hatch = default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("delivering to a loopback address should not be allowed by default")
	}
	if hit {
		t.Fatal("the request reached the local service -- the guard didn't take effect")
	}
	// The error message should guide the user on how to opt in (a local SMTP
	// relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the rejection message should explain how to explicitly opt in: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case: it must work
// once explicitly opted in, or legitimate deployments like a local postfix /
// internal relay would get blanket-blocked.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work once explicitly opted in: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // the cloud metadata endpoint -- this function's main reason for existing
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // an IPv4-mapped form must be converted back before judging, or it's a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: a self-hosted
	// internal Mattermost / SMTP relay is a common legitimate use. This
	// assertion locks down that trade-off -- if someone later casually adds a
	// private-network check, this will fail, forcing a deliberate decision
	// rather than silently breaking a batch of deployments.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (a private network is a common legitimate delivery target)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the upfront hint at
// the config stage: a literal IP should be rejected right at save time, not
// wait for the first delivery to fail.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at the config stage", raw)
		}
	}
	// A public address and a private address pass as usual (a private one is
	// left for the dial stage, which doesn't block it).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials covers a branch the audit
// pointed out as missed in the previous round.
//
// When url.Parse **fails**, it returns a *url.Error whose Error() includes
// the full original address. The previous round only redacted
// http.Client.Do's returned error and missed this one; and the "permanent
// failure path" cases added at the time (file://, gopher://, ftp://) could
// all actually be parsed successfully by url.Parse and took the scheme
// branch, so passing them didn't prove this path safe -- it was a false
// guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // invalid percent escape
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // non-numeric port
		"http://[::1?access_token=" + leakProbeToken,                  // unmatched bracket
	}
	for _, raw := range cases {
		// First confirm this input **actually** makes url.Parse fail.
		// Skipping this step risks the case silently taking a different
		// branch (which is exactly how the previous round's false guarantee
		// happened).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should have failed to parse, otherwise this case doesn't cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel-layer wrapper doesn't leak the address either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("an invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the SMTP channel's dial guard.
//
// The email channel used to use a bare net.Dialer, the one gap in the entire
// SSRF defense: setting host to 169.254.169.254 or 127.0.0.1 would connect
// directly, and when smtp.NewClient's handshake fails, it wraps the remote
// end's response line into the error, echoed back by the delivery-history
// endpoint via last_error -- exactly the semi-blind read primitive the other
// channels have already closed off; the timing difference between
// "connection refused" and "timeout" can also be used to probe ports.
//
// This package's TestMain turns on AllowLocalTargetsEnv globally (a large
// number of cases use fake receivers on 127.0.0.1), so this case must clear
// it itself -- otherwise it would pass whether or not the guard is in place,
// which is exactly how the gap went undetected by any test in the first place.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // turn off the escape hatch = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("delivering email to a loopback address should not be allowed by default")
	}
	// The connection should never even be established: the guard blocks it in
	// the Control hook, so EHLO never gets sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("an SMTP session was established -- the guard didn't take effect")
	}
	// The error message should guide the user on how to opt in (a local
	// postfix relay is a legitimate config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the rejection message should explain how to explicitly opt in: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse case:
// delivery must work normally once explicitly opted in. A self-hosted
// internal SMTP / local relay is a very common deployment, and the guard must
// not blanket-block it.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP should be deliverable once explicitly opted in: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("no EHLO seen -- the session wasn't actually established")
	}
}
