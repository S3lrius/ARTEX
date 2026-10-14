package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up prompt (settlement prompt): when an agent is stopped because its turn
// budget is exhausted (MaxTurns) or it times out (run_seconds/MaxDuration), the
// SDK settlement phase injects this prompt so the agent first writes back what it
// has identified but not persisted, then outputs a one-line summary, instead of
// ending with unfinished work.
//
// Each agent's wrap-up prompt can be overridden in the admin UI (stored in
// agents.wrapup_prompt); empty uses the built-in default here. Only the prompt
// body is editable. Which tools are disabled, and how many turns the wrap-up
// itself gets, are fixed in code.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, keyed by agent key. worker reuses the
// historically hardcoded settleWrapUpPrompt (defined in worker.go). planner and
// mainagent each have their own. Anything unmatched (custom agents) uses the
// generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: built-in default turn budget for each agent's own wrap-up
// phase (overridable by a positive admin value). All are 10 turns so the wrap-up
// has enough steps to persist. Unmatched keys use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "This planning round is about to run out of steps. Only this round is ending. The system will wake you again as the situation changes so you can keep planning. The task is not ending, and you do not need to close out the whole plan here. Persist the conclusions you have already settled this round so the round is not wasted, but do not invent intents just to wrap up (zero intents this round is still a completely normal result): (1) if you have already decided an exploration direction that should be dispatched now, submit it in one add_intent batch (do not hold back a direction you have already decided); (2) for a goal already proved by a finding or fact, call prove_goal and mark it met (do not miss one); (3) if you have identified a serial exploit chain that must be stepped through, record it with TodoWrite so the next wake-up can dispatch the next step. When that is done, end this round. Do not output a summary."

const mainAgentWrapUpDefault = "You are about to run out of steps, and this interaction is ending. Do not start new exploration or operations. In one plain-text sentence on its own, summarize the current progress, the key conclusions, and the suggested next step for the user."

const genericWrapUpDefault = "You are about to be stopped because the budget is exhausted. First write back results that are done but not yet persisted, then in one plain-text sentence on its own summarize what you did and the key conclusions (that sentence is shown as this run's result)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up text (see the task-level timeout and wrap-up design doc) ----------
//
// This is a second set, separate from the per-run wrap-up. Per-run means "this
// run's budget is used up". Task timeout means "the whole task has reached its
// time and is about to end". The meanings are often opposite (especially the
// planner: per-run says "do not stop, keep planning"; task timeout says "time is
// up, stop planning and make a final judgment"). Configured only for worker and
// planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB overrides for the task-timeout
// wrap-up text and turn count (wired to agents.task_timeout_wrapup_prompt /
// _max_turns, worker and planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has hit its timeout and is about to end** (not this run's budget; the whole engagement has reached its time). This is the last chance: (1) persist everything you have identified but not yet written back — new assets with insert_assets, exploration conclusions/facts with record_fact, confirmed vulnerabilities with report_finding; (2) do not start any new command or probe; (3) **finally, in one plain-text sentence on its own**, summarize the key conclusions for this intent."

const plannerTaskTimeoutDefault = "**The whole task has hit its timeout and is about to end** (not this round; the whole task is stopping). Using all current facts and findings, make one last goal judgment: for every goal the evidence proves is met, call prove_goal and mark it met (do not miss one). **Do not generate any new intents** (an intent dispatched now would not be executed). After the judgment, stop. Do not output a summary."

// TaskTimeoutWrapupDefault returns an agent's built-in task-timeout wrap-up text
// (admin placeholder / restore-default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // unconfigured (mainagent/chat) returns an empty string
}

// resolveTaskTimeoutWrapup: a non-empty DB override wins over the built-in default.
// An empty string means this agent has no task-timeout text (not worker/planner);
// the caller should fall back to the per-run text.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // default: reuse the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is clamped by the task deadline: Timeout wrap-up means the task time is up, so use the task-timeout text;
//     MaxTurns wrap-up means the step budget inside the clamped window ran out first and the task still has minutes left, so fall back to the per-run text.
//   - clamped=false → the task is not near its deadline: both reasons use the per-run text (this reduces to wrapupSettlement).
//
// PromptByReason handed to the harness picks by the actual reason at wrap-up time, so there is no mismatch at build time.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // task time is up
				harness.ReasonMaxTurns: perRun, // steps ran out first; the task still has time left
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
