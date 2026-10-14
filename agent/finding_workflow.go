package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Vulnerability ID convention**: finding_id is the standalone vulnerability record ID; finding_node_id is the exploration-node ID. The id returned by list_findings / list_task_findings / node_detail / get_task_node_detail remains the exploration-node ID; read the standalone ID from finding_id in the same response. get_finding_traffic / bind_finding_traffic use the standalone finding_id. The legacy update_finding_report finding_id parameter still takes finding_node_id. Do not use the number on the first line of report_finding with the evidence tools, and do not guess another number after an ID error."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nBy default the report agent checks and binds traffic before writing the report. The reporter keeps the verification commands, key output, existing real traffic IDs, and what each one is for in evidence, so the report agent can check them against the execution record. Do not search for extra packets just to bind. Explicit immediate binding is still supported: traffic_refs or evidence_hint_id may submit already-verified references; the latter reads the structured references of the named hint in this task. If either is invalid, the whole report fails. TCP and cases with no packet do not need these optional parameters. The returned finding_id and finding_node_id are the standalone record and the exploration node, respectively."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing off a confirmed vulnerability, keep the verified traffic ID, role, note, and order in that hint's traffic_refs (one item at the top level; a batch goes in the matching hints element), and say in text which specific vulnerability it proves. The caller must not hand off only text and drop existing traffic references. An unverified candidate must not be passed as evidence."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic evidence handoff (optional)**: automatic binding is done by default by the report agent after the vulnerability is stored and before the report is written. The reporter should keep verification commands, key output, existing real traffic IDs, and what each one is for in evidence, and include intent_id in the task, so the report agent can trace them. Do not search for extra packets just to bind. When Auto / Planner report on someone else's behalf, do not drop references the executor already has. add_hint / add_task_hint may hand off with traffic_refs; explicit immediate binding still accepts report_finding's traffic_refs / evidence_hint_id. For TCP or no packet, record the finding normally. Do not guess IDs, and do not probe again only to fill in a packet."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen a platform conversation has no task context, do not call report_finding directly. Hand off to the existing task with add_task_hint, let the task agent record it, and check the result with list_task_findings."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore judging a goal complete, finish reporting or handing off the evidence already in hand. Do not end the task or cancel a Worker only because a text vulnerability was recorded while the evidence handoff is still unfinished. No packet does not require waiting or forcing a capture."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**Automatic traffic association before the report (enabled)**: you check and bind traffic for the vulnerability that triggered this run, then write the report. First get a definite finding_id and finding_node_id from the report_finding JSON, or from get_task_node_detail / list_task_findings. Read the vulnerability detail, the execution record of the matching intent, and the existing evidence list, and prefer real IDs the reporter handed off. If this verification is HTTP and the traffic tools are available, use traffic_search to filter candidates, then traffic_get to check each request/response really supports the vulnerability. Hostnames and timestamps only filter; they do not prove the traffic belongs to the finding. Associate confirmed evidence in reproduction order with bind_finding_traffic(finding_id, traffic_refs), choosing baseline / proof / verification / supporting and saying what each one is for. Operate only on this vulnerability. Do not create it again or probe the target again. After a successful bind, call get_finding_traffic again for the latest version, read the body you need, then pass the version you actually read as evidence_version to update_finding_report (its finding_id parameter still uses finding_node_id). Do not append a binding that already exists. For TCP, uncaptured traffic, unavailable tools, or no exact match, skip automatic binding, write the report from the text/command evidence, and say why. Do not guess in order to fill out traffic. If binding fails, do not claim success; keep the existing evidence and say in the report why it was not bound."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references for the specific vulnerability in this hint, in order. After handoff, report_finding may pass evidence_hint_id to carry these references.", "items": obj(map[string]any{"traffic_id": str("real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("what conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must be a hint node of this task (an inherited hint cannot be used directly for binding)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
