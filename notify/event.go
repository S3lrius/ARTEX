package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. The
// writer is the db-layer transaction that stores a vulnerability. The readers are
// the server delivery engine and filter matching. It lives in this package because
// it is the notification-domain payload: db only serializes it and does not interpret the fields.
//
// The vulnerability fields are stored redundantly instead of being looked up again
// at render time because a finding can later be renamed, re-rated, or have its status
// changed. The notification should reflect the conclusion at the time of the event.
// A later lookup would dangerously show "later changed to low". Fan-out and rendering
// also avoid joining findings, tasks, and assets.
type Snapshot struct {
	// Event kind: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Set only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is one vulnerability waiting to be sent, for a channel to render.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are resolved display names (for example a domain or IP). The server layer
	// fills them. This package does not touch the database, so it cannot look the names up.
	Assets []string
	// DetailURL links back to the vulnerability detail. Empty means public_base_url is unset, and rendering omits the link.
	DetailURL string
	// Used only for status-change events. When both are set, they render as "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the display title: the human-given name first, then the vulnerability
// class, then a placeholder when both are empty. It never returns an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed vulnerability)"
}

// Message is the full content of one channel send.
type Message struct {
	// Length 1 for a single notification; a whole batch for a digest.
	// An empty slice is invalid. The caller must provide at least one item.
	Items []Item
	// Batch=true renders a digest (different title, plus the time window and count).
	Batch bool
	// WindowMinutes is the digest period in minutes. It is used only when Batch=true,
	// for the "in the last N minutes" copy. It is passed in from config rather than
	// computed with time.Since at render time so rendering stays deterministic and testable.
	WindowMinutes int
	// HomeURL is the platform console address (the global public_base_url). Empty means no console link.
	HomeURL string
}
