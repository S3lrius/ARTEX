package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "User paused the task",
		"The user paused the task through the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was cancelled on purpose; in-flight intents return to the frontier (open) and, after the task resumes, are claimed again and run from the start")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestrator paused the task",
		"The orchestrator agent called pause_task and paused this task. This Planner/Worker run was cancelled on purpose; in-flight intents return to the frontier (open) and run again after resume")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}). The delete barrier cancelled this task's running Planner, Worker, and main agent; this run's result will not be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task pause",
		"On startup the backend restored the task pause from the state persisted in the database. This run was cancelled; normally no agent is running during recovery")
	AbortGoalMet = cause("goal_met", "Planner judged the goal met",
		"The planner judged the task goal met and set the task to done, then cancelled Workers still running; those intents are marked stopped, not failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task-timeout drain wait elapsed",
		"After the task hit timeout it waited for running Workers to wrap up cleanly, but the 90-second drain grace was still not enough, so it hard-cancelled; intents are marked exhausted, and facts and assets already written during wrap-up are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner killed this intent",
		"The planner called kill_work and stopped this intent on purpose, usually because the direction drifted or no longer has value; the intent is marked stopped and is not claimed again automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused the running Worker. This call was cancelled and the intent became paused; intents, facts, vulnerabilities, and activity already recorded are all kept, and it runs again from the start after resume")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this Worker intent",
		"The user deleted the running Worker. This call was cancelled; after the Worker leaves the write section, the server applies the delete mode the user chose — a soft delete only marks it deleted and keeps every product, a hard delete cascades and removes the intent and downstream nodes supported only by it")
	AbortWorkFinished = cause("work_finished", "Worker finished and released its context",
		"The Worker finished normally and the engine released its context in detachWork. This is not a run interruption; if it shows up in an interruption message, cancellation raced the teardown")
	AbortPausedRaceGuard = cause("paused_race_guard", "Pause refused to start a new run",
		"While the task is paused, the engine refuses to issue a new execution context, so a race between claim and pause cannot let a Worker keep starting; a claimed intent returns to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this chat turn",
		"The user clicked stop and aborted this main-agent or session-agent turn on purpose. Activity already produced is kept, and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause stopped the main-agent chat",
		"When the user paused the task, the running main-agent chat was cancelled at the same time. Activity already produced is kept; resuming the task does not automatically replay this turn")
	AbortChatTurnFinished = cause("chat_turn_finished", "Chat turn ended; context released",
		"This chat turn finished normally and the server is releasing that turn's context. This is not a run interruption; if it shows up in an interruption message, cancellation raced the teardown")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. Every running agent is cancelled; after restart, leftover running intents are reset to open and run again")
	AbortRunHardTimeout = cause("run_hard_timeout", "Per-run hard timeout fired",
		"A single run exceeded the soft wall-clock budget plus the extra grace, which means a model request or a tool did not return for a long time and the normal turn-boundary wrap-up could not run. Check the last tool call that had not returned before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context hit its deadline",
			"The upstream context hit its deadline, but the setter did not attach a named cause through WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Cancel had no named cause",
			"The upstream context was cancelled, but the canceller did not attach a named cause through context.WithCancelCause; register the cause in agent/cancelcause.go and wire it at that cancel site", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
