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
	harness.ReasonCompleted:         "The model ended this turn normally but left no text summary; treat this turn's tool-call records as the source of truth for facts and assets.",
	harness.ReasonMaxTurns:          "Reached the turn limit (MaxTurns): the SDK has already wrapped up and written back facts and assets; the intent is marked exhausted so the planner can continue in a different direction, rather than being treated as a failure.",
	harness.ReasonTimeout:           "Reached the wall-clock budget for a single run (MaxDuration): once time is up, the running tool is interrupted and wrap-up happens in place, writing back any facts and assets already identified; the intent is marked exhausted.",
	harness.ReasonModelError:        "The model or API call failed (network, auth, rate limiting, provider 5xx, etc.); after retries are exhausted the intent is marked blocked — a transport-layer failure means this intent was essentially never actually explored. Check its execution trace (get_worker_trace) before deciding whether to redispatch it or try a different approach.",
	harness.ReasonBlockingLimit:     "Context length hit the hard limit and the request was blocked before being sent; narrow the intent's scope or compress tool output.",
	harness.ReasonPromptTooLong:     "The prompt is too long and context-compaction retries are exhausted, so execution cannot continue.",
	harness.ReasonImageError:        "The current model does not support this turn's multimodal content; switch to a vision-capable model or avoid having tools return images.",
	harness.ReasonStopHookPrevented: "A Stop hook prevented this turn from ending and it was then unable to continue; check whether the task's Guard rules are too strict.",
	harness.ReasonHookStopped:       "A tool or hook actively stopped execution, e.g. an out-of-scope target or a disabled command; check the last tool_result for the block explanation.",
	harness.ReasonAbortedStreaming:  "The run was canceled during the model's streaming output stage.",
	harness.ReasonAbortedTools:      "The run was canceled during tool execution.",
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
			short = "could not determine the cancellation reason"
		}
		stage := "mid-execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "the model output stage"
		case harness.ReasonAbortedTools:
			stage = "the tool execution stage"
		}
		sum = "(run interrupted: " + short + "; stopped during " + stage + progressSuffix(term, tr) + ", incomplete)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(reached the run budget limit (" + string(reason) + "); wrapped up and wrote back facts" + progressSuffix(term, tr) + "; no text summary this turn)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(no text summary; terminal state " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + ")"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal state**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Interruption reason** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Interruption reason**: unavailable; the canceller may not have attached a named cause via context.WithCancelCause\n")
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
		fmt.Fprintf(&b, "- **Turns executed**: %d turns\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **Run duration**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Cumulative tokens**: input %d / output %d / cache read %d / cache write %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool calls**: this run ended before issuing any tool call\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool running at time of interruption**: `%s` (running for %s, **no result returned**)\n\n  ```json\n  %s\n  ```\n",
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
		return "The run's context was canceled, but no underlying Terminal event was produced."
	}
	return "Unknown terminal state; the harness may have added a new TerminalReason — please add an entry to reasonHint."
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
