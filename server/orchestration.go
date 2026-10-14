package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// This file is the P2 cross-task orchestration tool set (scoring-orchestration design notes, section 2 P2). These are host tools. They need the Manager (any task's Store), the Engine (pause), and the create-task flow, so they live in the server layer. Read tools redirect an existing per-task tool onto the target task's store (build a temporary ToolSet and Call the matching tool), reusing the same logic. Control tools (spawn and pause) call the Manager or Engine directly. Like the traffic tools, they are seeded into the tools table and bound per agent (visible only to the orchestration agent).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // platform tools (create and edit skills, tools, and MCP; for Auto)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] failed to load: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id is required"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task does not exist: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // general wake (writes without a dedicated callback use it; read tools are a no-op)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint records a "a person added N strategic hints: ..." trigger and wakes the planner
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"List every task (id, description, goal, status, runtime, parent task, LLM configuration). The orchestration agent uses it to see the whole picture, which tasks have been stuck too long, and which LLM each one uses. Runtime: running is created-to-now, and a terminal task is created-to-last-activity, in seconds. llm_profile is the configuration name the task planner and worker use. (active configuration) means it follows the globally active one.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(active configuration)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d (deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"List the available LLM configurations (profiles): id, name, model, format, and whether it is the active configuration. Pass an id as spawn_task's llm_profile_id to pin a child task to its own LLM (for example a cheap model for recon and a strong model for exploitation). API keys are not included.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Create a child task and start the exploration engine. Returns task_id. Use it to turn one piece of work (a challenge or a goal) into its own task. parent_ref is optional: set the parent task id this orchestration is tied to, to record the parent-child link.",
		objSchema(map[string]any{
			"description":            strParam("task description (a short title)"),
			"goal":                   strParam("task goal (what to achieve)"),
			"parent_ref":             strParam("optional: parent task id (records the parent-child link)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("optional: source task ids to inherit read-only (at most %d). The child can read the assets and conclusions those tasks already found as a starting point. Unlike parent_ref, which is only a parent pointer, this inherits content.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "optional: LLM configuration id for this child task's planner and worker (see list_llm_profiles). Empty inherits the parent, then falls back to the globally active configuration"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "optional: task-level timeout in seconds. When it is reached, a graceful wrap-up runs and the task enters the timeout terminal state. Empty or 0 means no limit"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "optional: planner heartbeat interval in seconds. When that long has passed since the last planning round ended or the task started, and nothing else triggered, one planning round runs (deadlock backstop, and a wake to supervise in-flight workers). Empty or 0 means the default 600 (10 min)."},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "optional: for a simple task, turn this on to issue one seed intent at creation (the description plus the goal) so the worker starts testing without waiting for the first planner round. Default false (plan first, then execute)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "untitled task"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal is required"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// Read-only inherited source tasks: a count cap, and each id must be valid, unique, and exist. The rules match HTTP task creation.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("at most %d related tasks can be selected", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("related task id is invalid or duplicated"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("related task #%d does not exist", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM configuration #%d does not exist or has no API key", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// Shared post-create flow. HTTP task creation (server.go createTask) uses the same launchTask: seed, then a visible goal breakdown in the background (round 0, the LLM step, and one goal at a time), then engine.Run. seed_first_intent defaults to false (plan first, then execute). A simple task can turn it on to issue one work item and start testing.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Pause the given task (stop its planner and worker loops).",
		objSchema(map[string]any{"task_id": strParam("task id to pause")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task does not exist: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Read the exploration-graph overview of a task (same as graph_overview: asset counts, frontier, findings, coverage, and so on). Pass task_id.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Read a task's confirmed findings (including flag and PoC; each row has id, task_id, intent_id, vulnclass, severity, summary, and status). Pass task_id.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Inject a strategic hint into a task. That task's planner reads it the next time it generates intents.\n"+
		"Prefer a batch: put several hints in the hints array and submit once (the ids array matches hints in length and order; a failed item has id=0). For one hint, omit hints and set the top-level text.",
		objSchema(map[string]any{
			"task_id":      strParam("task id"),
			"hints":        map[string]any{"type": "array", "description": "Prefer this. An array of hints. Each item has the same fields as the top level (text, asset_ids, traffic_refs).", "items": objSchema(map[string]any{"text": strParam("hint text"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[single] hint text"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "anchored asset ids (optional, zero, one, or many; asset ids inside this task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"Read how one work item (intent) ran inside a task. get_task_worker_trace(task_id, intent_id) returns step summaries. Pass step_ids=[...] to fetch the full content of those steps (at most 5; extra ids return only the first 5).",
		objSchema(map[string]any{
			"task_id":   strParam("task id"),
			"intent_id": map[string]any{"type": "integer", "description": "intent id (a work item in this task)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "optional: step ids whose full content to fetch (at most 5; extra ids return only the first 5, and the rest are listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "List which work items (intents) ran in a task and how many steps each has, so you can see which ones are worth opening with get_task_worker_trace.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search every work item's execution in a task by keyword. Returns matching step summaries and intent_id.",
		objSchema(map[string]any{"task_id": strParam("task id"), "q": strParam("search keyword")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Read the full content of one exploration-graph node in a task (finding, fact, intent, or goal: summary plus detail, evidence, and PoC). id is the exploration node id (what report_finding returns, or the id in list_task_findings). Use it to get the full evidence before writing a finding report.",
		objSchema(map[string]any{
			"task_id": strParam("task id"),
			"id":      map[string]any{"type": "integer", "description": "exploration-graph node id (not an asset id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write or update the detailed report for a recorded finding (full Markdown, replacing the previous text). finding_id is the id report_finding returned (the number in \"finding recorded: <id>\"). A report should cover the overview, impact, reproduction steps, evidence or PoC, and a fix.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "target finding id (the id report_finding returned)"},
			"report":           strParam("full detailed report, in Markdown"),
			"evidence_version": map[string]any{"type": "integer", "description": "evidence version returned by get_finding_traffic; prevents the report from overwriting a newer evidence change"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // reuse the "number or numeric string" parser
			if nodeID <= 0 {
				return actool.Errorf("invalid finding_id"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("no finding record for finding_id=%d (record it with report_finding first)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (it exists to operate the platform). SeedTool applies on first insert. Rows already seeded in an old database are bound by seedAutoDefaultBindings.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // unbind list_facts, list_companies, and list_worker_traces from the worker default (one time)
	s.seedWorkerReadbackRebind()  // fix an old migration that deleted them: bind search_all_worker_traces, get_worker_trace, and node_detail back onto the worker (one time)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals prompt adds an "extract operating constraints" step; old databases get a new default version (one time)
	s.reseedMainAgentPrompt()         // main-agent prompt asks, after the goal is met and add_intent submits an intent, whether to record a goal (one time)
	s.reseedPlannerPrompt()           // planner prompt: rewrite the legitimate reason for "0 intents" and add a quantitative acceptance check (one time)
	s.reseedWorkerPrompt()            // worker prompt: add an evidence bar for negative conclusions (one time)
	s.seedReporterAgent()             // preset the report-writer agent, its tool bindings, and the finding trigger (one time)
	s.upgradeReporterTriggerMessage() // migration for old databases: make the reporter send evidence_version back (one time)
	s.seedFindingTrafficTools()       // add the optional evidence parameter and the read-only evidence tools, keeping the user's configuration
	s.seedFindingWorkflowTools()
	// The pentest default tool bindings need no migration. BuiltinToolSeeds already seeds list_assets, insert_assets, report_finding, list_findings, and list_companies together with pentest on a fresh init. There is no old database to migrate.
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// Also refresh some built-in agent tools to the code default:
	//   - goal_met: the description seeded in old databases said "end this planning round", which made the planner treat it as a way to end an empty round and wrongly mark the whole task complete right after it started.
	//   - insert_assets: a new related parameter marks whether an asset belongs to the current task and whether it counts toward coverage. SeedTool is first-insert only, so a schema already seeded in an old database would not receive the new parameter.
	//   - list_facts: now paginated, with limit, before, and q. An empty schema already seeded in an old database would otherwise show "no parameters" on the tool page, and the model would not get those parameter descriptions.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] refreshed orchestration and platform tool schemas to the code default (one time)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] failed to unbind goal_met from the planner: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt refreshes the goals breakdown prompt to the current code default. The default text added a step that extracts operating constraints (set_constraints) before splitting goals, and SeedPromptIfEmpty is first-insert only, so an old database's version 1 never receives it. Version history appends a new version and switches to it (ResetPromptToDefault). The old version stays in history, so a user who customized it can restore it from the version record. A settings flag guards it, so it runs once. Bump the flag when the default changes again. A fresh database needs nothing (SeedPromptIfEmpty already seeded the latest default).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // try only once, whether it succeeds or not
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // a fresh database has no agent row yet; seedPrompts seeds the latest default, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// A fresh database's seedPrompts already seeded the latest default, so the current version already matches the code default and a duplicate version is not appended.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the goals prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default goals prompt (adds the extract-constraints step, one time)")
}

// reseedMainAgentPrompt refreshes the main-agent prompt to the current code default. The default text now asks the person, when add_intent submits an intent after every goal is met, whether to record it as a formal goal. SeedPromptIfEmpty is first-insert only, so an existing version in an old database never receives it. Version history appends a new version and switches to it (ResetPromptToDefault). The old version stays in history, so a customization can be restored from the version record. A settings flag guards it, so it runs once. A fresh database needs nothing (SeedPromptIfEmpty already seeded the latest default). The shape matches reseedGoalsPrompt.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // try only once, whether it succeeds or not
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // a fresh database has no agent row yet; seedPrompts seeds the latest default, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// A fresh database's seedPrompts already seeded the latest default, so the current version already matches the code default and a duplicate version is not appended.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the main-agent prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default main-agent prompt (asks about creating a goal after the goal is met, one time)")
}

// reseedPlannerPrompt refreshes the planner prompt to the current code default. The default was rewritten to be shorter, restraint was reduced to deduplication only, "depth before coverage" and a hard floor were added (if the goal is not met and nothing is running, it must produce work), and negative-conclusion review got a cap. Whenever the default changes in substance, bump the flag below (currently v2) so existing databases refresh once more. SeedPromptIfEmpty is first-insert only, so an old database's existing version does not receive it. Versioning appends a new version and switches to it (ResetPromptToDefault). The old version stays in history, so a customized prompt can be recovered from the version record. The settings flag guards it, so it runs once. A fresh database needs nothing (SeedPromptIfEmpty already seeded the latest default). It has the same shape as reseedGoalsPrompt.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // try only once, whether it succeeds or not
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // a fresh database has no agent row yet; seedPrompts seeds the latest default, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// A fresh database's seedPrompts already seeded the latest default, so the current version already matches the code default and a duplicate version is not appended.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the planner prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default planner prompt (slimmer rewrite, restraint reduced to dedup, depth first, and a cap on negative-conclusion review, one time)")
}

// reseedWorkerPrompt refreshes the worker prompt to the current code default. The record_fact section dropped the sentence about writing a negative conclusion as an observation plus a tentative reading, and confidence (observed or inferred) was decoupled from "whether this intent's means were exhausted", which was easy for the planner to misread. Splitting the facts array was tightened to the rare case where items are fully independent and cannot be merged. The flag is bumped to v3 so existing databases refresh once more. SeedPromptIfEmpty is first-insert only, so an old version is not updated. Versioning appends a new version and switches to it, and the old version stays in history. The settings flag guards it, so it runs once. A fresh database needs nothing. It has the same shape as reseedGoalsPrompt.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // try only once, whether it succeeds or not
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // a fresh database has no agent row yet; seedPrompts seeds the latest default, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// A fresh database's seedPrompts already seeded the latest default, so the current version already matches the code default and a duplicate version is not appended.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the worker prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default worker prompt (context lookup narrowed to list_assets and list_findings, dropping list_facts, node_detail, and asset_neighbors, one time)")
}

// reporterToolCallMessage must unconditionally require one get_finding_traffic read before the report is written. The tool is read-only and does not depend on the capture switch, so it can read manually bound evidence whether automatic binding is on or off. If this said "read only when automatic binding is on", a reporter on the default-off configuration would not send evidence_version. SetFindingReportVersionByNodeID would then write -1 under the legacy meaning, and the finding detail and Markdown export would stay on "evidence changed, report needs an update" with no UI control that can clear it.
const reporterToolCallMessage = "A finding was just recorded by report_finding. Read finding_id (the standalone finding record ID) and finding_node_id (the exploration node ID) from the returned JSON. " +
	"First call get_finding_traffic(finding_id) and read the current evidence list and its version (an empty list is normal; write the report anyway). " +
	"If the run guidance has automatic binding on, verify and associate this finding's traffic before that read. Use finding_node_id for the node detail. " +
	"Then call update_finding_report(finding_id=finding_node_id, report, evidence_version=the version you actually read) to save. " +
	"evidence_version is required, or the report is permanently marked as needing an update. Do not mix up the two ids."

// upgradeReporterTriggerMessage records that the reporter trigger wording migration
// has been considered. The previous one-time upgrade matched a stored message
// verbatim. That historical text is not kept in this tree, so the migration now
// only records the flag. New databases are seeded with reporterToolCallMessage.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedReporterAgent presets a report-writer custom agent (builtin=false, so it can be edited or deleted in the UI). It binds update_finding_report and the task query tools, and attaches a trigger that fires when report_finding is called, so each recorded finding wakes it to write a detailed report. It runs once (settings flag). If the user deletes it, it is not recreated. The orchestration tools are already SeedTool'd above this function, so the bindings succeed.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // try only once, whether it succeeds or not

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // the key is already taken (the user created one); do not overwrite it
	}
	a, err := s.m.pg.CreateAgent("reporter", "Report writer",
		"Writes detailed finding reports. Fires when a finding is discovered, reads the evidence and the execution, then writes a Markdown report back.")
	if err != nil {
		log.Printf("[reporter] failed to create the agent: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] failed to seed the prompt: %v", err)
	}
	// Trigger run policy: parallel + none. One finding, one report, and several findings write concurrently. merge must be none. Otherwise the default all would fold a wave of findings into one run and parallelism would not matter. maxParallel=5 caps concurrent report sessions at 5 so a burst does not start too many LLM calls.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] failed to set the trigger run policy: %v", err)
	}
	// Bind the tools it needs: write the report, and read evidence, execution, and the situation.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] failed to bind tools: %v", err)
	}
	// Trigger: fire when report_finding is called. The tool returns "finding recorded: <id>" with the finding_id, and the task id is in the trigger message too.
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] failed to create the trigger: %v", err)
	}
	log.Printf("[reporter] seeded the report-writer agent and the finding trigger")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] failed to bind report_finding by default: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] failed to bind report_finding by default: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] failed to bind list_assets by default: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // switch the default binding from worker to planner
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] failed to bind add_company_scope by default: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] failed to unbind add_company_scope: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] failed to unbind %s from the worker: %v", k, err)
			return // on error do not set the flag, so the next start retries
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: also bind node_detail
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] failed to bind the read-back and detail tools: %v", err)
		return // on error do not set the flag, so the next start retries
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: replace the old asset tool names and add insert_assets and add_company_scope
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Asset tools: operating the platform as Auto often means viewing and recording assets and managing company scope.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] failed to apply the default bindings: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
