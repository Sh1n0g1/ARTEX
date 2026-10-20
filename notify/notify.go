// Package notify implements the IM / email delivery channel adapters for
// vulnerability findings.
//
// Layering: this package is a **leaf package**, depending only on the
// standard library. It knows nothing about the database or the server. A
// channel's config is passed in as map[string]any (mirroring the
// notification_channels.config JSONB column), and the content to deliver is
// passed as a Message. The benefit of this separation: the parts that are
// genuinely easy to get wrong — signature computation, UTF-8 truncation,
// filter matching — can be unit tested without PostgreSQL; the host only has
// to orchestrate things on the server side.
//
// Concurrency contract: Channel implementations must be **stateless**. The
// same Channel instance is shared concurrently across multiple channel
// configs (even multiple bot instances of the same channel), so all
// credentials are always passed in via the cfg parameter — nothing like a
// webhook URL may be cached into the implementation's own fields.
package notify

// Channel type identifiers. These values also form the valid set for
// notification_channels.kind, allow-listed on the server side (same
// treatment as findings.status — no DB CHECK, to keep adding channels easy).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (incl. Lark) custom bot
	KindWeCom    = "wecom"    // WeCom (WeChat Work) group bot
	KindWebhook  = "webhook"  // Generic webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback kind used when config has none set.
const InitKind = KindDingTalk

// severityRank maps a vulnerability severity to a comparable rank. An unknown
// severity returns 0, so any min_severity setting blocks unknown severities —
// when in doubt, don't notify, to avoid flooding on false positives.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the severity's rank; unknown severities return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the severity name with an emoji, used in message
// titles and card colors. An unknown severity is echoed back as-is rather
// than guessed at.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel translates a disposition status into a display label, used in
// status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In Progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False Positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk Accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min
// means no threshold, so everything passes. Note an unknown severity has
// rank 0 and is rejected by any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
