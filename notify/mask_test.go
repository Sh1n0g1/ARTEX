package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("masked value leaked the full credential: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("masked value should not expose the address body: %q", got)
	}
	// The last 6 characters must be kept, or the user can't recognize which
	// bot it is.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("should keep the last 6 characters as an identifying hint: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("a masked value must be recognized by IsMasked: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// If a short credential also exposed its last 6 characters, that would
	// expose the entire credential.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("a credential of length %d should not get a tail hint, got %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("masked value contains the original value: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s should be masked, got %q", k, s)
		}
	}
	// A non-credential field must be kept as-is, or the UI can't display it.
	if masked["port"] != float64(587) {
		t.Errorf("non-credential field port should not change: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// When the channel type can't be recognized, better to have the UI show
	// an empty config than to spit back the original content, which might
	// contain credentials.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("an unknown channel type should return an empty config, got %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// Masking is a display-layer behavior and must not in turn overwrite the
	// real value in the database.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig modified its input, which would overwrite the real credential with the masked value")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// The user only changed method; the browser submits the masked values
	// plus the new method.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("masked fields should keep the stored original value, got %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("a changed field should take effect, got %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("an empty string should clear the field, got %v", got)
	}
	// A field not mentioned is kept (partial-update semantics).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("an unmentioned field should be kept, got %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("an unmentioned field should be kept, got %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("a mentioned field should be updated, got %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap is this package's most
// important security invariant: **changing the destination address must not
// carry the old credential along**.
//
// These cases use exactly the shape of an attacker's input (change only the
// address, say nothing about the credential), not "correct input for the
// defense logic" -- testing only the latter would stay all-green even if the
// defense never actually took effect.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing is the credential key expected to be named.
		wantMissing string
	}{
		{
			name: "generic webhook changes address wanting to keep the Authorization header",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram changes base_url wanting to send the Bot Token to its own endpoint",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "email changes the SMTP host wanting to hand over the password",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "email turning off TLS must also re-state the password",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// A masked value = "keep using the old credential", which must
			// also be rejected in the context of a destination change.
			name:        "echoing back a masked credential + a new address",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk changes the webhook wanting to keep the signing secret",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("the address changed without re-stating the credential and should have been rejected; got config %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("should return the dedicated error type so the endpoint can give an actionable message, got %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("should name the missing credential key %q, got %v", tc.wantMissing, target.Missing)
			}
			// The error message should guide the operator on how to fix it.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("error message should mention %q: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits is the reverse case: a normal
// edit must not be caught by mistake, or this protection would get bypassed
// or removed for being "too annoying".
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "only changing the name (config echoed back as-is)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "only changing the request method, address and credential untouched",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "changing the address and **also** giving a new credential",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "changing the address and explicitly declaring the credential is no longer needed",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram changes chat_id (not a destination)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "email changes the recipient (not a destination)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("a legitimate edit was caught by mistake: %v", err)
			}
			if merged == nil {
				t.Fatal("should return a merged result")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance covers a detail that's easy to
// misjudge: the port submitted by the frontend is a JSON number (float64),
// and reading it back from the database is also float64, but the two values'
// types can differ (e.g. int vs float64). Comparing with == would misjudge
// "unchanged" as "changed", popping up "please re-enter the password" for a
// user who only changed the name -- a false alarm would make people stop
// trusting this protection.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// The same port, submitted as an int.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("the same port value (only the type differs) should not be judged a destination change: %v", err)
	}
	// A genuine port change must still be caught.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("a port change should be caught")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination covers the
// "an optional destination field left blank" path: Telegram's base_url left
// blank means using the official API address.
//
// This used to make the channel permanently fail to save starting from the
// second save:
//
//	Created with base_url:"" stored in the database (the create path stores the
//	frontend-submitted config directly, bypassing MergeConfig)
//	→ first save: MergeConfig treats the empty string as an explicit clear and
//	  deletes the key
//	→ second save: incoming is still "", while stored no longer has the key,
//	  judged as "address changed"
//	→ bot_token is the masked echo-back value → 400 "destination address has
//	  changed, please re-enter the credential field"
//
// The user changed nothing, yet could never save again, short of re-pasting
// the Bot Token.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// The frontend's buildConfig() submits a value for every field defined
	// on this channel: credentials are filled back in as a masked value, and
	// an empty text box is submitted as an empty string. This fully
	// reproduces that output, rather than submitting only "the keys that
	// changed".
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// First save: only the channel name changed; config is echoed back as-is.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("the first save was caught by mistake: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("precondition changed: an empty string should have been deleted by MergeConfig -- this is exactly the 'after the key disappears' step this case is meant to cover")
	}

	// Second save: the submitted content is identical to last time; the user
	// changed nothing.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("the second save was caught by mistake (the user changed nothing): %v", err)
	}
	// A third time, to confirm it's not "wrong just once" but reliably savable.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("the third save was caught by mistake: %v", err)
	}
	// The credential must be carried through the whole way, not swept away
	// by the empty-string logic.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("the Bot Token should keep its original value, got %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges is the paired
// assertion to the previous case: treating an empty string and "the key
// doesn't exist" as equivalent must **not** also let a genuine address
// change through. Both directions are real credential-exfiltration
// paths -- Telegram's Bot Token travels in the URL path, so changing
// base_url is the same as sending the Token to the new address.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// Direction one: from "blank" (the official address) to a self-hosted address.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("switching from the official address to a self-hosted address must require re-entering the Token")
	}

	// Direction two: clearing a self-hosted address (= switching back to the
	// official API) is likewise an address change.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("clearing a self-hosted address (switching back to the official API) is likewise an address change and must require re-entering the Token")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// Same reasoning as SecretKeys: if a channel forgets to declare its
	// destination keys, PrepareConfigUpdate can't protect it.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("channel %s did not declare destination keys; the protection against changing the address to exfiltrate credentials doesn't apply to it", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s did not declare credential keys", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// The compiler already forces every channel to implement SecretKeys;
	// this double-checks that "no channel skipped masking" -- a channel
	// returning an empty slice means its credentials get echoed back to the
	// browser in plaintext.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("channel %s is not registered with a masking expectation in the test", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("channel %s did not declare any credential fields; its config would be echoed back in plaintext", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer covers a gap the audit
// pointed out: when the masked sentinel is stuffed into a **non-string**
// structure (e.g. webhook.headers is an object), MergeConfig only recognizes
// a masked value when it's "a string with the prefix", so the literal
// "__masked__" would get stored as a real header value in the database --
// silently breaking auth afterward, with no error at all.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// The masked sentinel stuffed inside the object.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("stuffing the masked sentinel inside a structure should be rejected (otherwise the literal would get stored in the database)")
	}
	// Submitting the object as a whole (a real new value) is accepted as usual.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("submitting a new header normally should not be blocked: %v", err)
	}
}
