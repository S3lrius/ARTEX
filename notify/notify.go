// Package notify is the IM and email channel adapter for vulnerability findings.
//
// Layering: this package is a leaf. It depends only on the standard library. It does
// not know about the database or the server. Channel config arrives as map[string]any
// (the notification_channels.config JSONB column), and the content to send arrives as
// a Message. Signing, UTF-8 truncation, and filter matching — the parts that are easy
// to get wrong — can then be unit-tested without PostgreSQL. The host only orchestrates them on the server side.
//
// Concurrency: a Channel implementation must be stateless. One Channel instance is
// reused concurrently by many channel configs, even several bot instances of the same
// channel. Every credential comes in through the cfg argument. A webhook URL must not be cached on the implementation.
package notify

// Channel kind identifiers. The values are also the allowed set of notification_channels.kind.
// The server checks them with an allowlist (same approach as findings.status: no DB CHECK, so adding a channel stays easy).
const (
	KindDingTalk = "dingtalk" // DingTalk custom robot
	KindFeishu   = "feishu"   // Feishu (including Lark) custom robot
	KindWeCom    = "wecom"    // WeCom group robot
	KindWebhook  = "webhook"  // Generic webhook: custom method, headers, and JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event kinds, matching notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback when kind is empty in config.
const InitKind = KindDingTalk

// severityRank maps a vulnerability severity to a comparable rank. An unknown
// severity returns 0, so any min_severity setting excludes it. When in doubt, do not notify, to avoid a flood of false alarms.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the rank of a severity. An unknown severity returns 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns an emoji severity name for message titles and card colors.
// An unknown severity is echoed as-is and not invented.
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

// StatusLabel turns a workflow status into a readable label for status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min means no threshold, so everything passes.
// An unknown severity has rank 0 and is rejected by any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
