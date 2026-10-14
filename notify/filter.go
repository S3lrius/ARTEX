package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column:
// the filter for one channel instance.
// Every field is optional. Missing fields mean "do not filter" — that is the
// fallback for a malformed config, see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity (low/medium/high/critical). Empty means no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs: an empty slice means no restriction. A non-empty slice requires the event to intersect it.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude: empty means accept every class. Otherwise vulnclass must contain one of the keywords.
	// VulnClassExclude: a hit on any keyword drops the event (exclude wins over include).
	// Matching is a case-insensitive substring, which is safer than a regex: a bad regex cannot silently disable the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether this channel receives vulnerability status-change events (realtime mode only).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses a channel filter config.
//
// It never returns an error. That is deliberate: a malformed filter degrades to
// the zero Filter (no filtering, everything matches). For a vulnerability notifier,
// sending one extra message is far better than silently dropping a critical one.
// Turning a parse failure into "send nothing" gives the user a channel that looks
// configured and never notifies — the worst failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On a parse failure f stays at the zero value, which means no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a legal severity threshold (an empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the fields whose values are constrained, for use when a channel is saved.
//
// Why this has to happen on write: Match treats an unknown threshold as `rank >= 0`,
// which is always true. A one-letter typo in min_severity ("hgih") would silently
// disable the filter and send everything. That matches this package's preference
// for sending too much rather than too little (nothing is dropped), but the user
// would believe they had severity routing while every finding landed in the group,
// with no sign that the config was wrong. That silent downgrade belongs at the entry point.
//
// Validate is only for the write path. The read path still uses ParseFilter's lenient
// semantics, so a bad value already stored in history does not make the whole channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("minimum severity %q is invalid; choose low, medium, high, or critical, or leave it empty for no limit", f.MinSeverity)
	}
	return nil
}

// Match reports whether an event should be delivered to a channel with this filter.
//
// It never returns an error, for the same reason as ParseFilter: any internal failure is treated as a match.
// Order: event kind, then severity, then task/asset scope, then vulnerability-class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are delivered only when the channel opts in. The default is off,
	// because most people mean "notify me about new findings", not a running log of every status transition.
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
	// Exclude wins: any excluded keyword drops the event, even if an include keyword also matches.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan is enough for these small sets. Both sides are a few dozen ids a person checked by hand.
	// Building a map would cost more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any keyword, case-insensitively.
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
