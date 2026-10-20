package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up prompt (wrap-up / settlement prompt): when an agent is terminated
// because its turns are exhausted (MaxTurns) or it timed out
// (run_seconds/MaxDuration), the SDK's settlement phase injects this prompt so
// the agent first persists everything it identified but hasn't written back
// yet, then outputs a one-line summary — avoiding a half-finished run.
//
// Each agent's wrap-up prompt can be overridden as needed from the admin UI
// (stored in agents.wrapup_prompt); leaving it empty uses the built-in default
// here. Only the prompt body is editable; which tools get disabled and how many
// turns the wrap-up phase itself gets are fixed code policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. worker reuses the
// historically hardcoded settleWrapUpPrompt (defined in worker.go); planner and
// mainagent each have their own version; anything not matched (a custom agent)
// falls back to the generic default.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: the built-in default turn budget for each agent's OWN
// wrap-up phase (can be overridden from the admin UI with a value > 0). All set
// to 10 turns, enough for the wrap-up phase to persist its results. Anything
// not matched falls back to genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your turn budget for this planning round is about to run out — note this is only the end of THIS round; the system will wake you again to keep planning as the situation changes, this is not the task ending, and you don't need to wrap up the whole plan here. Land the conclusions you've already worked out this round so it isn't wasted, but also don't force an intent into existence just to have something to wrap up with (ending this round with 0 intents is a perfectly normal outcome): (1) if you've identified exploration directions that should be dispatched right now, submit them in one add_intent batch call (don't sit on ones you've already worked out); (2) for any goal already proven by a finding/fact, call prove_goal to mark it met (don't miss one); (3) if you've identified a serial exploit chain that needs to be worked in steps, note it with TodoWrite so it can be picked up next time you're woken. Once done, just end this round — no summary text needed."

const mainAgentWrapUpDefault = "Your turn budget is about to run out and this interaction is about to end. Do not start any new exploration/operation. Summarize the current progress, key conclusions, and the suggested next step for the user in **a single standalone plain-text sentence**."

const genericWrapUpDefault = "You are about to be terminated because the budget is exhausted. First write back any completed results that haven't been persisted yet, then summarize what you did and what key conclusions you reached in **a single standalone plain-text sentence** (this sentence will be shown as this run's result)."

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

// ---------- Task-level timeout wrap-up prompts (see docs/task-level-timeout-and-settlement-design.md) ----------
//
// These are a SEPARATE set from the per-run wrap-up prompts: per-run means "your
// budget for this one run is spent"; task timeout means "the whole task has hit
// its deadline and is about to end." Their meaning is often opposite (especially
// for planner: per-run says "don't stop, keep planning," task timeout says "stop
// planning now that the deadline hit, make your final call"). Only configured for worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB overrides for the task-timeout
// wrap-up prompt and turn count (wired to agents.task_timeout_wrapup_prompt /
// _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (this is not your budget for this one run — the whole exploration has hit its deadline). This is your last chance: (1) persist ALL content you've identified but haven't written back yet — new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed vulnerabilities via report_finding; (2) do not start any new commands/probes; (3) **finally, in a single standalone plain-text sentence**, summarize your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (this is not this round — the whole task is terminating). Based on ALL current facts and findings, make one final goal determination: for any goal already proven by evidence, call prove_goal to mark it met (don't miss one). **Do not generate any new intents** (dispatching one now would never get executed anyway). Once you're done determining goals, wrap up — no summary text needed."

// TaskTimeoutWrapupDefault returns the built-in default task-timeout wrap-up
// prompt for an agent key (used by the admin UI as a placeholder / "restore default").
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // returns "" for agents with none configured (mainagent/chat)
}

// resolveTaskTimeoutWrapup: a non-empty DB override takes priority over the
// built-in default. An empty string means this agent has no task-timeout prompt
// (not worker/planner), in which case the caller should fall back to the per-run prompt.
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
	return resolveWrapupTurns(agentKey) // defaults to reusing the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is being clamped by the task deadline: wrapping up
//     due to Timeout = the task hit its deadline → use the task-timeout prompt;
//     wrapping up due to MaxTurns = turns ran out first within the clamped window
//     while the task still has minutes left → fall back to the per-run prompt.
//   - clamped=false → the task still has plenty of time: both reasons use the
//     per-run prompt (i.e. this degrades to wrapupSettlement).
//
// Handed to the harness's PromptByReason, which picks based on the ACTUAL reason
// at wrap-up time, so there's no mismatch baked in ahead of time.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value used for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // the task hit its deadline
				harness.ReasonMaxTurns: perRun, // turns ran out first, task still has time left
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
