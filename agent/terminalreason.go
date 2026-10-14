package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "The model ended this turn normally but left no text summary; facts and assets follow this turn's tool-call record",
	harness.ReasonMaxTurns:          "Hit the step limit (MaxTurns): the SDK ran wrap-up and wrote facts and assets back. The intent is marked exhausted so the planner can change direction and continue, not treated as a failure",
	harness.ReasonTimeout:           "Hit the single-run wall-clock budget (MaxDuration): at the deadline an in-flight tool is interrupted and wrap-up starts in place, writing back facts and assets already identified. The intent is marked exhausted",
	harness.ReasonModelError:        "The model or API call failed (network, auth, rate limit, provider 5xx, and so on). After retries are exhausted the intent is marked blocked — a transport failure means this intent was barely explored. Check its execution (get_worker_trace) before redispatching or changing approach",
	harness.ReasonBlockingLimit:     "Context length hit the hard limit and the request was blocked before it was sent; narrow the intent or compress tool results",
	harness.ReasonPromptTooLong:     "The prompt is too long and context-compression retries are exhausted, so execution cannot continue",
	harness.ReasonImageError:        "The current model does not support this turn's multimodal content; switch to a vision-capable model or stop tools from returning images",
	harness.ReasonStopHookPrevented: "A Stop hook blocked this turn from ending, and it could not continue; check whether the task Guard rules are too strict",
	harness.ReasonHookStopped:       "A tool or hook stopped execution on purpose, for example an out-of-scope target or a disabled command; check the block note on the last tool_result",
	harness.ReasonAbortedStreaming:  "The run was cancelled while the model output was still streaming",
	harness.ReasonAbortedTools:      "The run was cancelled during tool execution",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "could not get the cancel reason"
		}
		stage := "execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "model output"
		case harness.ReasonAbortedTools:
			stage = "tool execution"
		}
		sum = "(run interrupted: " + short + "; stopped during " + stage + progressSuffix(term, tr) + ", unfinished)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(hit run budget limit (" + string(reason) + "), wrap-up wrote facts back" + progressSuffix(term, tr) + "; no text summary this time)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(no text summary, terminal state " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal state**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Interruption cause** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Interruption cause**: unavailable; the canceller may not have attached a named cause through context.WithCancelCause\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **Underlying error**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **Partial output generated before cancellation**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **Executed**: %d model turns\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **This run's elapsed time**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Cumulative tokens**: input %d / output %d / cache read %d / cache write %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool calls**: this run ended before any tool call was issued\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool in flight at interruption**: `%s` (ran for %s, **did not return a result**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **Last tool before interruption**: `%s` (returned normally)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "The run context was cancelled, but the layer below produced no Terminal event"
	}
	return "Unknown terminal state; harness may have added a TerminalReason, so add a reasonHint"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", ran " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
