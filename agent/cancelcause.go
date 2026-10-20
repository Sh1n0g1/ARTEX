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
	AbortPausedByUser = cause("paused_by_user", "user paused the task",
		"The user paused the task via the task control endpoint (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; the in-flight intent reverts to frontier(open) and is reclaimed and re-run from scratch once the task resumes")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "the orchestrator agent paused the task",
		"The orchestrator agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; the in-flight intent reverts to frontier(open) and re-runs once resumed")
	AbortTaskDeleted = cause("task_deleted", "the task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier cancelled this task's running Planner, Worker, and main agent. This run's result will never be used")
	AbortPausedOnReload = cause("paused_on_reload", "backend restored the paused task",
		"On startup the backend restored the task's paused state from what was persisted in the database. This run was cancelled; under normal conditions no agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "the planner judged the task goal met",
		"The planner judged the task goal met and marked the task done, then cancelled any still-running Workers; those intents are marked stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "timeout wrap-up grace period expired",
		"After the task hit its timeout, it waited for running Workers to wrap up gracefully, but the 90-second drain grace period wasn't enough, so a hard cancel was issued; the intent is marked exhausted, and any facts/assets already written during wrap-up are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "the planner terminated this intent",
		"The planner called kill_work to actively terminate this intent, usually meaning the direction drifted or is no longer worth pursuing; the intent is marked stopped and is not automatically reclaimed")
	AbortWorkPausedByUser = cause("work_paused_by_user", "user paused this Worker intent",
		"The user paused a running Worker. This call was cancelled and the intent moved to paused; all recorded intents, facts, findings, and activity records are kept, and it re-runs from scratch once resumed")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "user deleted this Worker intent",
		"The user deleted a running Worker. This call was cancelled; once the Worker exits the write zone, the server handles the intent per the user's chosen delete mode — a soft delete only marks it deleted and keeps all output, while a hard delete cascades to remove the intent and any downstream nodes that depend solely on it")
	AbortWorkFinished = cause("work_finished", "worker finished, context released",
		"The Worker finished normally; the engine released its context resources in detachWork. This is not a run interruption — if it appears in an interruption message, it indicates a race between cancellation and the finishing event")
	AbortPausedRaceGuard = cause("paused_race_guard", "run rejected: task is paused",
		"While the task is in the paused state, the engine refuses to issue a new execution context, to prevent a race between claim and pause from letting a Worker keep starting; the already-claimed intent reverts to frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "user stopped this conversation turn",
		"The user clicked stop, actively aborting this main-agent or chat-agent run. Any activity records already produced are kept, and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "task pause aborted the conversation",
		"When the user paused the task, the running main-agent conversation was cancelled in lockstep. Any activity records already produced are kept; resuming the task does not automatically replay this turn's message")
	AbortChatTurnFinished = cause("chat_turn_finished", "turn finished, context released",
		"This conversation turn finished normally; the server is releasing that turn's context resources. This is not a run interruption — if it appears in an interruption message, it indicates a race between cancellation and the finishing event")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "the backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running agents are cancelled; any running intents left over after the restart are reset to open and re-run")
	AbortRunHardTimeout = cause("run_hard_timeout", "the hard per-run timeout backstop fired",
		"A single run exceeded its soft wall-clock budget plus the extra grace period, meaning a model request or some tool call didn't return for a long time, preventing a normal end-of-turn wrap-up from running. Check the last tool call that hadn't returned before the interruption")
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
		return "deadline_exceeded", "upstream context hit its deadline",
			"The upstream context hit its deadline, but whoever set it did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "canceller did not attach a named cause",
			"The upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; please register a cause in agent/cancelcause.go and wire it into that cancellation site", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
