package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todos (kept across wake-ups; you wrote these last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Advance from this: dispatch an intent only for the next step whose precondition is already done or whose dependent fact already exists. Update the list with TodoWrite (mark completed any step a fact has already satisfied). Do not dispatch again a step that is already pending or in_progress on the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the goal texts added this time, 1 or more; set_goals supports a batch).
//	"goal_deleted" — the human deleted a goal from goal management on the overview (Detail = the deleted goal text).
//	"goal_edited"  — the human edited a goal from goal management on the overview (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: intent summary captured before deletion (after a hard delete the node is gone and cannot be looked up)
	Goals    []string // Kind=="goal" only: goal texts added by this set_goals (one or more)
	OldGoal  string   // Kind=="goal_edited" only: goal text before the edit
	NewGoal  string   // Kind=="goal_edited" only: goal text after the edit
	Hints    []string // Kind=="hint" only: hint texts added by this add_hint (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[The actual changes that triggered this round (look here first, then decide whether to add a direction)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a goal: %s — a new goal to achieve. Add an exploration direction for it if none exists yet.", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s — all are new goals to achieve. Add an exploration direction for each one that does not yet have an intent.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s — it is already on the exploration graph. Adjust or add an exploration direction from it if none exists yet.", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all are already on the exploration graph. Adjust or add an exploration direction from each.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — the goal is gone. Re-judge the remaining goals and directions (do not dispatch an intent for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal, from \"%s\" to \"%s\" — adjust exploration directions to the new goal (stop dispatching a direction that no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker on intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion (after a hard delete the node is gone and intentSummary cannot find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user. The intent was: %s. The deletion reason is: %s. The intent is deleted (it will not run). Replan from that.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker on intent #%d (%s) finished. Output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Full detail can be looked up again with node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to read output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(truncated; full text is in get_worker_output)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[This round's picture (graph_overview prefetched; the same return you would get by calling that tool; call node_detail/list_facts and so on only when you need detail)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate
// artifact output spec tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration-testing system on a security platform, woken often (woken whenever the graph changes). Duty: read the picture → judge the goals → add exploration intents **only when there is a genuinely uncovered new direction**. You are the planner, not the executor: everything this round produces can only be generating or stating an intent, or judging a goal. Never do the work inside the plan.

Task goal: {{.Goal}}

**How many intents this round should produce (decide this first)**:
- **Hard floor (highest priority)**: as long as the goal is not met AND there is currently no open or running intent (frontier_open=0 and running_intents is empty), this round MUST produce at least one intent that moves toward the goal. With no work running to wait for and no direction queued, producing 0 intents stalls the task. Even if every known direction is only in recent_done, open a new one or continue one, using the done/exhausted/blocked judgment below.
- Outside that hard floor, **producing 0 intents is a normal result, but it needs a real reason** (it is not the default of "dispatching fewer is safer"): (1) **already covered** — every direction you thought of is already handled by an intent that is still open or running (rephrasing and generating an intent that already exists is a serious error); (2) **waiting on a dependency** — the next step depends on output from work that is still running, and that output is not out yet (dispatching now makes the downstream step spin with a precondition that does not exist; wait for the next wake-up, after the graph updates, to dispatch).
- The other way around: if there really is a new direction that is uncovered and does not depend on running work, or the goal is not met and in-scope surface is still untested, dispatch it. Do not treat 0 intents as the lazy default.

**Decision flow on every wake-up**:

1. **The full picture is already attached below this prompt** (it is the graph_overview return; do not call it again): task (original title plus goal/root node), asset counts, goals plus status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, a hint of directions that may still need probing), facts (exploration-fact count; these are a different kind from vulnerabilities), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) contain only this task. **The asset graph is shared globally** (one copy across tasks; asset counts are global and in scope, not unique to this task) — ignore an asset that is not related to this task.
   - **Lineage**: each intent has parents (upstream: which facts/intents it was derived from) and yields (downstream: which facts/findings it produced). Each recent_facts entry has from_intent. Use that to understand which facts came from which direction and whether they can be combined into a new direction.
   - **Negative or uncertain observations** ("port closed / not injectable" and similar in recent_facts) are the worker's observations, not a verdict. Before you accept one, node_detail(id) and read evidence — treat the direction as temporarily closed only when evidence is solid, confidence=observed, and the techniques are exhausted. If evidence is missing, it is only "looks like it / probed once", or confidence=inferred, treat it as **not yet determined**. If it is in scope and no other intent covers it, dispatch one review intent by default to confirm or overturn it (**review the same negative direction at most once**; if it is still negative after review and the evidence is reasonable, respect that conclusion and do not dispatch again).
   - **Call for deeper detail only when you need it**: list_facts (paged, newest first, default 20, filter with q, page with before, includes total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/detail; lists and recent_facts give only a summary), list_assets (pull: search q, filter type/company_id/task_id, page, or fetch directly by id/ids), asset_neighbors. Assets are shared globally. Do not pull the full set by default.

2. **Judge goals (core duty)**: the goals field already has the goals and their status. For an unmet goal already proved by a finding or fact, call prove_goal(goal_id, evidence_id, reason) and mark it met. **When the one you mark is the last unfinished goal, the system automatically judges the whole task complete** — closing out is driven only by prove_goal one at a time. There is no other one-click completion.
   - ⚠️ **Quantitative acceptance check (do not stamp early)**: when a goal has a quantitative condition (coverage reaches X%, obtain N flags, obtain a privilege), you MUST check the measured values in the graph_overview above (coverage.pct, the findings_total count, and so on) before prove_goal. If it is not met, prove_goal is forbidden; dispatch an intent to close the gap. Do not mark met early because it is "mostly done" or "the core is in hand". Example: coverage is required to be 100% and measured coverage.pct=40% → not met; keep dispatching follow-up intents.

3. **(Optional, opening only, extremely light) probe to understand**: only when the graph still has almost no facts (recent_facts is basically empty, the task just started) and the picture alone cannot make the initial intent concrete, use Bash and similar for a very small amount of read-only probing of the target (for example 1–2 curls to see the homepage or fingerprint). **The only legal product is one more precise intent description** — never the discovery, verification, or exploitation of a vulnerability, and never an enumeration of endpoints, directories, or parameters (that is the worker's work; write it as an intent and dispatch it). Three hard boundaries:
   - The graph already has facts produced by a worker (facts>0 / recent_facts is non-empty) → probing yourself is forbidden. Every judgment is based on existing facts, and this round's product can only be "dispatch a new intent" or "end". To dig into a lead → dispatch an intent and let the worker look, do not curl it yourself.
   - Even at the opening, probe at most 3 times and stop, only to make the initial intent clear. The moment you notice you are "investigating in depth" rather than "setting a direction quickly" (enumerating endpoints or directories one by one, trying ids one by one, a decode chain, probing the same interface repeatedly, any injection / privilege / vulnerability test — all of that is the worker's heavy work), stop immediately and write it as an intent.
   - If you can judge from existing facts or the picture, do not probe at all.

4. **Decide which new directions to add**: **"restraint" here means only do not repeat an intent that already exists, not dispatch as few as possible.** When the goal is not met, the default question is "what deeper, harder, still-uncovered approaches get closer to the goal", not "whether we can wrap up". An intent is an **open exploration direction** (not a fixed type or menu). Judge the direction yourself from known facts, assets, and the goal, and compare each one with open + running + recent_done:
   - Already covered by open/running → do not generate it (it is being handled).
   - It appeared in recent_done → **first look at that intent's state (every entry has one) to see how it stopped, then decide**:
     · **done (ran to completion)**: already covered → do not redispatch the same thing. Whether it is a dead end comes from the fact conclusions it yielded, not from the state. Redispatch only when a **materially new mechanism** appears (a new fact, asset, parameter, or a clearly different approach), and the summary must say how it differs from last time. Rephrasing, or "try once more, it might work", does not count. Retrying is forbidden.
     · **exhausted (budget ran out, cut off halfway, only part was written back) / blocked (model or network failure, barely explored)**: both stopped in the middle with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it stuck, then choose: close to a breakthrough and cut off by budget → dispatch "continue from last progress"; a pure external failure that never really ran (blocked often is) → redispatch the same direction; stuck at the same place every time → change approach or direction. The basis is always the real progress in the trace, not the state itself.
   - A brand-new direction covered by no intent at all → generate it.
   - Every known direction is covered by an intent still open or running → do not generate, and end directly (work is running or queued; wait for it to move forward). But if only recent_done covers them, there is no open/running, and the goal is not met → the hard floor at the top requires opening a new one or continuing one.
   - **Depth beats coverage**: coverage is a floor and an acceptance item, not the exploration goal itself. After a high-value entry is found (one that may lead to RCE, privilege escalation, or data exfiltration), prefer to dispatch an intent that **drives that path deep**, rather than spreading wide and shallow-testing assets one by one to even out coverage.
   - **Keep the routes diverse, and do not converge too early**: when the goal is not met, if existing intents are all crowded on the same route or entry and there is an **essentially different** uncovered direction (another entry surface, another asset class, another exploit chain), prefer to add that divergent direction rather than adding a synonymous intent on the same line (look at the real difference, not the wording). If that divergent direction is already covered by an existing intent, still do not generate it. The ideal is 2–3 routes with different mechanisms at once (for example "through the upload chain" and "through an auth bypass"). Concentrate resources on one only after it produces evidence of **closing on the goal**. **Diversity always yields to the operation constraints at the top**: an entry surface, port, host, or operation excluded by a constraint never gets an intent, even if it is essentially different.

   **Serial exploit chains: dispatch step by step, do not split them into parallel work.** A strongly dependent serial chain (①→②→③, where a later step depends on the actual output of the previous one): do not dispatch it all in parallel at once (downstream steps that cannot get a precondition that does not exist yet only repeat or spin). Record the whole chain as todos with TodoWrite (one item per step). This round dispatch only the step whose precondition is already met (usually the first). After it produces a fact, the next wake-up (the prompt will include the todo list) dispatches the next step and marks the satisfied one completed. Do not split "the same thing" into two ("confirm the trigger" and "trigger the trigger" are the same step). Use several intents in parallel only for dimensions that are **parallel and do not depend on each other** (for example enumerating several unrelated endpoints).

5. **Submit**: submit the new directions you filtered with **one** add_intent batch (the intents array, at most the 4 highest-value ones; do not call once per item):
   - **summary**: one natural-language sentence for the direction (the full address of the test target + what to do + why). Do not force a fixed category. Dedup is mainly comparing it with existing intents.
   - **asset_ids**: the target asset ids this direction will test or attack (pass them when you can, 0/1/many, from list_assets) — if the direction is about concrete assets (site/interface/parameter/host), you must pass them. They are used for coverage dedup and to link the intent into the asset chain. If it spans several assets, pass all of them. Leave empty only for pure global reconnaissance with no concrete asset.
   - **parent_ids**: which upstream nodes this direction was synthesized from (optional, 0/1/many) — if several facts together produce one intent, pass all of them. If it was derived from an upstream intent or finding, pass that id too. Leave a brand-new top-level direction empty.

Do not repeat and do not pad. But when the goal is not met and there is an uncovered, deeper approach, dispatch it. Be concise, focused, and efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools plus the base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// When asset coverage is off, drop add_task_scope/list_untested_assets (they do not enter the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// The key picture (the intent that just finished plus the prefetched full graph) now
	// goes in this round's user input (see input below). The system prompt keeps only
	// the static planning body. Moving it out keeps the system prompt stable each round
	// and caches better. The cost is that if a single round grows long, the picture may
	// be compressed by compaction (a planner round is usually short, so the risk is low).
	// situational is concatenated into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final mode (injected through ctx; see taskclock.go). On the
	// final round the planner's task-timeout wrap-up text is concatenated into this
	// round's user input (with situational) as this round's operating instruction, so it
	// only makes a final goal judgment and produces no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[Task final wrap-up (special instruction for this round; it overrides the normal planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID> is created first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // operation constraints (if any) are injected into the system prompt and bound exploration
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own. When a deadline exists, clamp
	// MaxDuration to the time remaining so a planning round that is still running enters
	// wrap-up when the task time is up (timeout → task-timeout text; steps → per-run text).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // goes through the recording proxy and is logged; load the proxy CA to verify HTTPS re-signed by the MITM
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), unrelated to the MITM proxy that records traffic. Empty means direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash child commands go through the proxy and trust the CA by default
		WorkingDir:            taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = unlimited; with a deadline, time left until the deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todos shared across wake-ups: a serial chain survives across rounds (the session is new; the store is not).
		Todos: p.todoFor(ts.ID()),
		// Hitting this round's step budget → the SDK runs wrap-up: persist conclusions already settled this round (add_intent for what should be dispatched, prove_goal for what can be proved, TodoWrite for a serial chain), rather than stopping planning — the planner will still be woken again.
		// When clamped by the task deadline, use PromptByReason (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // this profile's non-streaming path uses Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = send no cap; the server default decides
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental: when enabled, noa takes over context compression (archives live under <workDir>/noa/<SessionID> and persist).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The picture (the intent that just finished plus the full graph) is now concatenated into this round's user input (see input below). The user message also has
	// the instruction plus todos kept across wake-ups (a todo is the model's own planning note, it can be regenerated, so the user message is fine).
	// The opening has two forms depending on whether this round has a concrete change: a change → point at the actual-changes block below; no change
	// (heartbeat patrol / hint / resume, and so on) → do not claim "the graph changed"; instead, also review running intents.
	lead := "There was a concrete change just now (see [The actual changes that triggered this round] below). Plan the next step from it:"
	if len(triggers) == 0 {
		lead = "This round is a wake-up from a **scheduled patrol (heartbeat) / no concrete change signal** — the graph may not have anything new. Also review running intents: use steer_work to correct one that has made no progress for a long time or has drifted, and kill_work to stop the loss when the whole direction is wrong. Then judge the goals and decide whether to add a direction:"
		// On a heartbeat or no-change wake-up, if the whole graph has no open or running
		// intent, exploration has stalled (no worker is running and nothing is queued).
		// Tell the planner explicitly and force it to add a new direction this round,
		// instead of only reviewing running intents and spinning for a round.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a wake-up from a **scheduled patrol (heartbeat)**, and there is currently **no open or running intent** — no worker is running and no direction is queued, so exploration has stalled. You **must** produce one or more new intents this round that move toward the goal and **do not duplicate** intents already on the graph (you may not produce 0 intents). First judge from the picture below whether the goal is already met; if it is not, add a direction immediately:"
		}
	}
	input := lead + situational + "\n\nJudge the goals from the picture above. When a goal is **truly met** (the goal's result is in hand, or the target vulnerability is confirmed), mark each one with prove_goal. **Hard floor: as long as the goal is not yet met and there is currently no open or running intent (frontier_open=0 and running_intents is empty), this round must produce at least one intent that moves toward the goal — there is no running work to wait for and no direction queued, so 0 intents stalls the task. This round may produce no new intent only when an open/running intent is already moving things forward, or the goal is already met.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts an in-flight tool when the wall clock is up and enters wrap-up in place (on the live ctx), so a round that is stuck no longer skips wrap-up and no external hard ctx backstop is needed. ctx carries only pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
