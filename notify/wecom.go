package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard cap on a WeCom group bot's markdown content
// (bytes, not characters). This is the tightest limit among all six channels,
// and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom (WeChat Work) group bot.
//
// Platform specifics:
//   - Auth is solely via the key in the URL; signing isn't supported — so the
//     webhook URL itself is the entire credential.
//   - markdown content is capped at 4096 **bytes**; exceeding it gets the
//     whole message rejected (not truncated). At 3 bytes/char for Chinese,
//     that's only a little over a thousand characters of body text, so
//     client-side truncation is required.
//   - Rate limit is 20 messages/min, also backstopped by client-side rate
//     limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has exactly one credential (the key in the webhook URL), and signing
// isn't supported — the whole address is the entire credential, with no
// other field needing masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has exactly one field, the webhook, which is both the destination
// and the credential, so there's no such thing as "a leftover credential
// after changing the address" here.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest batch can be quite long (50 items × one line each + prefix),
	// and easily exceeds 4096 bytes. Truncation happens here rather than
	// relying on the platform's rejection: a rejection means the whole batch
	// is lost, while truncation at least delivers the first several items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 means the API call rate was exceeded — the platform's rate
		// window rolls over, so retrying with backoff is effective, hence
		// it's explicitly classified as retryable. Reaching this means the
		// client's rate_per_min is configured too aggressively; retrying is
		// just a backstop, the real fix is lowering that channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means the webhook key is invalid — a permanent failure that
		// retrying won't self-heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
