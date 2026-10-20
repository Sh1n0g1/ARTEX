package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations must be
// **stateless**: the same instance is shared concurrently across multiple
// channel configs, so credentials are always passed in via the cfg parameter.
type Channel interface {
	// Kind returns the channel type identifier; must match the registry key.
	Kind() string
	// Validate is called when saving the config; it checks required fields and
	// formats. The returned error is shown directly to the person configuring
	// it, so the message should say "which field is missing" rather than a
	// generic "invalid config".
	Validate(cfg map[string]any) error
	// Send delivers one message, returning the **number of items actually
	// delivered** and an error.
	//
	// Why return a count: every platform has a message length limit, and a
	// digest message that doesn't fit the whole batch gets truncated. If the
	// caller unconditionally marks the whole batch as delivered, the items
	// that got truncated simply vanish — they're absent from the message and
	// the delivery history shows success, so there's no way to tell that a
	// finding was never actually sent. With kept returned, the caller only
	// marks the first kept items, leaving the rest for the next batch.
	//
	// A returned error means delivery failed; *PermanentError among those
	// means it should not be retried. When it fails, kept is meaningless and
	// the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the platform's officially recommended max
	// deliveries per minute, used as the default rate limit for new channel
	// instances. 0 means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the config keys that hold credentials for this
	// channel. These keys are masked when echoed back via the API, and a
	// masked value received on update means "keep the stored value". Only the
	// implementation itself knows which fields count as credentials (for
	// WeCom the entire webhook URL is the credential, while for DingTalk it's
	// just the secret), so this knowledge must come from the channel, not be
	// guessed by the caller.
	SecretKeys() []string
	// DestinationKeys returns the config keys that determine "where the
	// message is sent" for this channel.
	//
	// Like SecretKeys, this is security-relevant: the destination address and
	// the credentials are two independent sets of fields. If "change only the
	// address, keep the credentials as-is" were allowed, anyone who can edit a
	// channel's config could redirect the real stored credentials to a server
	// they control, defeating the whole point of masking the channel config.
	// See PrepareConfigUpdate for details.
	DestinationKeys() []string
}

// registry is the channel registry. Deliberately an explicit literal instead
// of self-registration via init(): this way "which channels exist" is visible
// in one place, and a missing channel is caught at compile time rather than
// relying on a runtime side effect.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get looks up a channel implementation by type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (for a
// stable UI dropdown).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried: bad
// credentials, a rejection by the destination, an invalid request body, etc.
// Retrying only makes sense for transient failures (network blips, rate
// limiting, a 5xx from the other side); retrying a permanent failure with
// backoff will never succeed and just buries the real error in retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil,
// so it can be written as `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err's chain carries the permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- config-reading helpers ----
//
// Channel config comes from a JSONB column in the database; once deserialized
// via encoding/json it's a map[string]any, with numbers always as float64 and
// arrays as []any. The helpers below unify that conversion and tolerate the
// type mismatches caused by a user leaving a field blank in the UI (e.g.
// typing a port as a string).

// cfgString reads a string config value, always trimming surrounding
// whitespace — easy to pick up when copy-pasting from a web form.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an integer config value, accepting both float64 (JSON's
// default) and string as the source.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a boolean config value, accepting the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-array config value, trimming whitespace and
// dropping empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accept a single string, for form submissions with only one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map config value (such as custom HTTP headers),
// trimming whitespace from keys and values and dropping empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
