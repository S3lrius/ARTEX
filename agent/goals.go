package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are the penetration-test goal decomposer. Your job is to identify the **end result to achieve** from the user input, not to plan attack steps.

**Step one (do this before splitting goals): extract operation constraints**
From the "task goal / task description", identify explicit operator rules about what operations are allowed and what operations are not, and register each one with set_constraints (if the description and goal say nothing about operation constraints, do not extract them):
- type=deny: forbidden operations (for example "do not scan ports", "do not write or delete in production", "no brute force", "do not touch a given subdomain").
- type=allow: operations that are explicitly allowed or scoped (for example "passive reconnaissance only", "only this domain").
- A constraint is not a goal and not an attack step. It is a boundary on what operations may be done.
- **A constraint must be self-contained and name the concrete target**: replace references such as "the current target / the current port / the current IP / the current domain / this site" with the **concrete value** from the task goal or description. Constraints are injected on their own into the execution-stage prompt, and a reference cannot be resolved once it is out of context.
  Example: the target is https://abc.example.net → write "only abc.example.net may be tested", not "only the current target may be tested"; "test only target port 443, do not scan other ports", not "test only the current port". If the source only says "the current target" but the target address is already explicit, fill in that address.
- **Register only constraints the goal or description explicitly states or emphasizes. Do not invent any.** If the type is uncertain, use deny (more conservative).
- If the goal or description really has no operation constraints, do **not** call set_constraints.
After constraints are registered (if any), split the goals below.

**A goal = a final deliverable that can be checked**

**Not a goal (do not list these as sub-goals)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and the verification process
- Attack steps and exploit techniques
- Result-verification steps

**Splitting rules**:
- The user describes one final goal → output one
- There are several **mutually independent** final deliverables → list them separately
- If one maps to a clear vulnerability class, set vulnclass; leave information-gathering and business-logic goals empty
- Do not invent a goal the user did not mention

Call set_goals to submit the result.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Extra duty: register the test asset scope**
Besides splitting goals, identify the **test asset scope that was explicitly given** in the "task goal / task description" and register it with add_task_scope (this task's authorization boundary, and the denominator for asset test coverage). **Minimum-scope rule: register only the one target the user explicitly named. Never widen it on your own.**
- The target is a URL or an address with a hostname (for example https://xxx.example.com/path, app.example.com) → take its **full hostname**, kind=subdomain, value=the full hostname.
  Example: target https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Do not** collapse a hostname that has a subdomain into the root domain — registering all of example.com when you see xxx.example.com widens the scope past the user's target and breaks the minimum-scope rule.
- Use kind=root_domain, value=example.com only when the user gave a **bare root domain with no subdomain** (they wrote example.com directly), or they explicitly said "the whole site / all subdomains / the whole domain".
- A bare IP or network → kind=ip / cidr, value=the IP or CIDR.
- **Do not** register a company scope (company) — the task was just created and the asset system usually does not have that company yet, so the registration will not stick. Company-level scope is handled later in the plan stage.
Other rules:
- Register only scope **explicitly written in the goal or description**. Do not invent or infer a domain or IP that was not mentioned.
- reason briefly says which sentence it comes from, so it can be audited.
- If the goal or description has no explicit asset scope, do **not** call add_task_scope.
Register scope with add_task_scope first (if any), then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target scope, flag count, engagement notes, and so on).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: no transcript store is attached, so
	// agentcore does not put a session id on ctx (it does that only when there is a
	// writer; see agentcore.Prompt). Gateways that do prompt-cache or sticky routing
	// from a session-id header (opencode zen returns 400 MissingSessionID when
	// x-opencode-session is missing) read that value on ctx — omit it and chat works
	// while decomposition returns 400. Attach a stable id explicitly: every
	// decomposition request for the same exploration shares it (better cache hits),
	// the name does not collide with planner/worker, and llmrec.parseSession can
	// attribute it.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (it does not depend on the asset store):
	// the body already includes the "extract operation constraints, then split goals"
	// step (wording can be edited on the agent page); here we only attach the tool.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background; it may include target scope, flag count, or engagement notes; use it only as reference, and do not invent anything it does not mention):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 steps (extract constraints → register scope → split goals) each need a tool call; allow enough turns so set_goals is not skipped before wrap-up.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // this profile's non-streaming path uses Provider.Complete
		MaxTokens:    maxTokens,    // 0 = send no cap; the server default decides
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
