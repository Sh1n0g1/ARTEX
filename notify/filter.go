package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB
// column: a channel instance's filter criteria. All fields are optional, and
// leaving them unset means "no filtering" — which is exactly the fallback
// semantics for malformed config, see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical);
	// empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// An empty TaskIDs / AssetIDs array means unrestricted; non-empty requires
	// the event to intersect with it.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// An empty VulnClassInclude means accept everything; non-empty requires
	// vulnclass to match any one of its keywords.
	// Matching any keyword in VulnClassExclude excludes the event (exclusion
	// takes priority over inclusion).
	// Matching is a case-insensitive substring match — safer than a regex:
	// a user's typo'd regex won't silently disable the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives status-change
	// events (meaningful only in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses a channel's filter config.
//
// **Never returns an error.** This is a deliberate design choice: malformed
// filter config always degrades to a zero-value Filter (= no filtering = everything
// matches), because for a vulnerability notification system, **sending one
// extra notification beats silently missing one critical finding**. Turning
// a parse failure into "don't notify" would hand the user a channel that
// looks configured but actually sends nothing — the worst possible failure
// mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure f stays at its zero value, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (an empty
// string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the filter config's fields that have a **restricted value
// set**, for use when saving a channel.
//
// Why this must be caught at write time: Match's handling of an unknown
// threshold is `rank >= 0`, which is always true — meaning a typo in
// min_severity ("hgih") would **silently disable** the filter, turning it
// into "send everything". This is consistent with this package's "better to
// over-send than under-send" bias (nothing gets missed), but the consequence
// is the user thinks they've set up tiered notifications while actually
// flooding the group with every finding, with no indication they misconfigured
// it. This kind of "silent degradation" is exactly what should be caught at
// the entry point.
//
// Note Validate is only used on the **write** path. The read path still goes
// through ParseFilter's lenient semantics, so a bad value already present in
// historical data doesn't make the channel unreadable altogether.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("invalid minimum severity %q; valid options: low / medium / high / critical, or leave blank for no threshold", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel with this
// filter.
//
// **Never returns an error**, for the same reason as ParseFilter: any
// internal anomaly is treated as "match". Evaluation order: event type →
// severity threshold → task/asset scope → vulnerability-type keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are only received by channels that explicitly
	// opt in. Off by default, because most people expect "notify" to mean
	// "a new finding was discovered", not a running log of every status
	// transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion takes priority: matching any exclude keyword is an immediate
	// out, even if the include list also matches.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan is fine for small sets; both sides are on the order of
	// "a few dozen, hand-picked by a person", so the overhead of building a
	// map exceeds the benefit.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
