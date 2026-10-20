package notify

// Snapshot is the contract for the notification_events.snapshot JSONB
// column. The writer is the db-layer finding-persistence transaction; the
// reader is the server-layer delivery engine and filter matching. It's
// defined in this package because it's a payload belonging to the
// "notification" domain — db only serializes it and doesn't interpret the
// fields.
//
// Why duplicate the finding's fields here instead of looking them up again
// at render time: a finding's name, severity and status can all change
// later, while the notification content should reflect the conclusion **at
// the time the event happened** — looking it up again would risk the
// misleading "since changed to low" result. It also means fan-out and
// rendering don't need to JOIN the findings/tasks/assets tables.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is one finding awaiting delivery, for a channel to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are the resolved asset display names (e.g. domains/IPs), filled
	// in by the server layer — this package doesn't touch the database, so it
	// has no way to get the names.
	Assets []string
	// DetailURL links back to the finding detail page; empty means
	// public_base_url isn't configured, and it's omitted when rendering.
	DetailURL string
	// Used only for status-change events; when both are non-empty, rendered
	// as "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: prefers the human-assigned name,
// falls back to the vulnclass, and uses a placeholder when both are
// empty — never outputs an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(Unnamed vulnerability)"
}

// Message is the full content for one channel send.
type Message struct {
	// Length 1 for a single-item push; a whole batch for a digest push.
	// An empty slice is invalid — the caller must guarantee at least one item.
	Items []Item
	// When Batch=true, renders as a digest message (different title, with the
	// time window and item count).
	Batch bool
	// WindowMinutes is the digest period (in minutes), used only when
	// Batch=true for wording like "in the last N minutes". Deliberately
	// passed in explicitly by config rather than computed from time.Since at
	// render time, so rendering stays deterministic and testable.
	WindowMinutes int
	// HomeURL is the platform dashboard address (the global
	// public_base_url); empty means no dashboard link is included.
	HomeURL string
}
