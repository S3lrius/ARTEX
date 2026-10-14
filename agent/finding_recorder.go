package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**Vulnerability traffic evidence (optional)**: when calling report_finding to report a vulnerability, if you have already viewed HTTP requests/responses and confirmed they support the conclusion, you may bind the real IDs in traffic_refs in reproduction order. Hostnames and timestamps only filter candidates; they do not establish that the traffic belongs to the finding. For TCP and other non-HTTP vulnerabilities, when traffic was not captured, or when there is no exact match, omit traffic_refs or pass []. Keep command output, logs, and other verifiable evidence in evidence, and say why nothing was bound. Do not guess IDs, and do not probe again only to fill in a packet."

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
