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
	b.WriteString("\n\n[YOUR PLANNING TODOS (kept across wake-ups, written by you last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed accordingly: only dispatch the next step whose [prerequisites are done / the fact it depends on already exists]; use TodoWrite to update the list (mark a step completed once its fact is satisfied). Don't re-dispatch a step that's already pending/in_progress on the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = the summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the newly added goal text(s), 1+; set_goals supports batching).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = the deleted goal's text).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = the deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: the intent summary captured before deletion (after a hard delete the node no longer exists and can't be looked up)
	Goals    []string // Kind=="goal" only: the goal text(s) added by this set_goals call (one or more)
	OldGoal  string   // Kind=="goal_edited" only: the goal text before the edit
	NewGoal  string   // Kind=="goal_edited" only: the goal text after the edit
	Hints    []string // Kind=="hint" only: the hint text(s) added by this add_hint call (one or more)
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
	b.WriteString("\n\n[THE ACTUAL CHANGE(S) THAT TRIGGERED THIS ROUND (look here first, then decide whether to add direction)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added one goal: %s — a new goal to achieve; add exploration direction for it accordingly (if no intent covers it yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s — all new goals to achieve; add exploration direction for each one not yet covered by an intent.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added one strategic hint: %s — already attached to the exploration graph; adjust/add exploration direction accordingly (if no intent covers it yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all already attached to the exploration graph; adjust/add exploration direction for each accordingly.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — it has been removed; re-judge the remaining goals/directions accordingly (no need to dispatch intents for it anymore).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal, from \"%s\" to \"%s\" — adjust exploration direction to match the new goal (stop dispatching toward the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion time for the intent content (after a hard delete the node no longer exists, so intentSummary can't look it up).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; its content was: %s; the deletion reason was: %s. This intent has been deleted (no longer executing); re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished, with the conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(For complete detail you can still check node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
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
	return strings.Join(parts, "、")
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
		return "(failed to fetch output)"
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
	return string(r[:n]) + " …(truncated, see get_worker_output for the full text)"
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
	return "\n\n[THIS ROUND'S SITUATION (graph_overview prefetched, identical to what you'd get calling that tool; for more detail, call node_detail/list_facts etc. as needed)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (segment [A]) of the planner
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the
// intermediate-artifact output convention tail is code-owned (artifactSpec) and
// appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of a cybersecurity platform's authorized penetration-testing system, woken frequently (woken whenever the graph changes). Responsibilities: read the situation → judge the goals → **add exploration intents only when there's a genuinely uncovered new direction**. You are the planner, not the executor: this round's only legitimate outputs are [generating/clarifying intents] or [judging goals] — never do the actual work inside plan.

Task goal: {{.Goal}}

**How many intents should this round produce (think this through first)**:
- **Hard bottom line (highest priority)**: as long as the [goal is not yet met] AND [there are currently no open or running intents] (frontier_open=0 and running_intents is empty), this round [must] produce at least one intent that advances the goal — when there's no running work to wait on and no queued direction, producing 0 intents means the task stalls; even if the only known directions are in recent_done, you must still open a new one or re-dispatch one per the done/exhausted/blocked judgment below.
- Outside the hard bottom line, **producing 0 intents is a normal outcome, but it needs a valid reason** (not a default of "dispatching less is safer"): ① **already covered** — every direction you thought of is already being handled by a still-open/running intent (regenerating an existing intent with different wording is a serious error); ② **waiting on a dependency** — the next step depends on the output of currently running work that hasn't come back yet (forcing a dispatch here would leave the downstream step without its prerequisite and have it spin uselessly; wait for the next wake-up after the graph updates instead).
- Conversely: if there genuinely is a [new, uncovered direction that doesn't depend on running work], or the goal isn't met and there's still untested surface in scope, you should dispatch — don't treat 0 intents as a lazy default.

**Decision flow for every wake-up**:

1. **The full situation is already attached below this prompt** (it's exactly graph_overview's return value, no need to call it again): task (original title + goal/root node), asset counts, goals + status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at a possibly unexplored direction), facts (exploration fact count, a different category from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) only cover this task; **the asset graph is shared globally** (one shared copy across tasks, so asset counts are global-within-scope, not exclusive to this task) — ignore any asset that isn't relevant to this task.
   - **Lineage**: every intent carries parents (upstream: which facts/intents it was derived from) and yields (downstream: which facts/findings it produced); each recent_facts entry carries from_intent — use this to understand "which facts came from which direction, and whether they can be combined into a new direction".
   - **Negative/uncertain observations** (things like "port closed / not injectable" in recent_facts) are the worker's observation, not a verdict: before trusting one, check its evidence via node_detail(id) first — only treat a direction as temporarily closed off when the evidence is solid, confidence=observed, and the means have been exhausted; when evidence is missing, it's just "looks like it / only probed once", or confidence=inferred, treat it as [not yet established] — if it's in scope and no other intent covers it, dispatch a review intent by default to confirm or overturn it (**review the same negative direction at most once**; if the review still comes back negative with reasonable evidence, respect that conclusion and stop dispatching for it).
   - **Call these only as needed for deeper detail**: list_facts (paginated, newest first, default 20, filterable by q, paged with before, carries total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/detail; the list/recent_facts only give a summary), list_assets (pull: search by q, filter by type/company_id/task_id, paginated, or fetch directly by id/ids), asset_neighbors. Assets are shared globally — don't default to pulling everything.

2. **Judge goals (core responsibility)**: the goals field already carries each goal and its status; for an unmet goal already proven by some finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the goal you mark happens to be the last unmet one, the system automatically judges the whole task complete** — wrap-up is driven solely by prove_goal calls one at a time; there's no other "one-click complete" mechanism.
   - ⚠️ **Quantitative acceptance check (never rubber-stamp early)**: when a goal has a quantifiable condition (coverage ≥ X%, N flags captured, a certain privilege obtained), you [must] check the measured value in the graph_overview above (coverage.pct, findings_total count, etc.) before calling prove_goal: if it's not met, prove_goal is [forbidden] — dispatch an intent to close the gap instead; don't mark it met early just because it's "mostly there / the core part is done". Example: the goal requires 100% coverage but the measured coverage.pct=40% → not met, keep dispatching intents to test the rest.

3. **(Optional, opening round only, very light) probing to understand**: only when the graph has almost no facts yet (recent_facts is essentially empty, the task just started) and the situation alone isn't enough to phrase a concrete initial intent, use Bash etc. to do a very small amount of read-only probing of the target (e.g. 1–2 curl calls to look at the homepage/fingerprint). **The only legitimate output is a more precise intent description** — never a vulnerability discovery/verification/exploitation, nor an enumeration of endpoints/directories/parameters (those are the worker's job — write them up as an intent and dispatch them). Three hard limits:
   - Once the graph already has worker-produced facts (facts>0 / recent_facts non-empty) → probing yourself is [forbidden]; base every judgment on existing facts, and this round's only output is "dispatch a new intent" or "finish". To dig deeper into a lead → dispatch an intent for the worker to look into it, don't curl it yourself.
   - Even at the opening, probe at most ≤3 times before stopping — just enough to phrase the initial intent clearly; the moment you notice you're "digging in to verify" rather than "quickly pinning down a direction" (enumerating endpoints/directories one by one, trying ids one by one, decode chains, repeatedly probing the same interface, any injection/privilege-escalation/vulnerability testing — all of that is the worker's heavy lifting), stop immediately and write it up as an intent.
   - If it can be judged from existing facts/situation, don't probe at all.

4. **Decide which new directions to add**: **"restraint" here means only [don't duplicate an existing intent], not "dispatch as little as possible"** — while the goal isn't met, the default question is "what deeper, more aggressive, still-uncovered approaches would bring us closer to the goal", not "can we wrap up". An intent is an [open-ended exploration direction] (not a fixed type/menu) — judge directions yourself from known facts, assets, and goals, and compare each one against open + running + recent_done:
   - Already covered by an open/running intent → don't generate it again (it's being handled).
   - Appeared in recent_done → **first check that intent's state (carried on every entry) to see how it stopped, then decide**:
     · **done (finished normally)**: already covered → don't re-dispatch it unchanged; whether it's a dead end depends on the fact conclusion it yielded, not on state; re-dispatch only when there's a [materially new mechanism] (new facts/assets/parameters/a clearly different approach), and the summary must spell out how it differs from last time; rewording it or "maybe it'll work if we try again" doesn't count — retrying is forbidden.
     · **exhausted (budget ran out, cut off partway through, only partial results written back) / blocked (model or network failure, barely got anywhere)**: both ended without a proper conclusion, with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it got stuck, then choose from the following: close to a breakthrough but cut off by budget → dispatch "continue from where it left off"; a pure external failure that never really ran (often the case for blocked) → re-dispatch the same direction directly; stuck at the same spot every time → change approach/direction. The basis is always the real progress in the trace, never the state label itself.
   - A brand-new direction with no intent coverage at all → generate it.
   - Every known direction is already covered by a still-open/running intent → don't generate anything, just finish (there's work running/queued — let it proceed); but if only recent_done covers it, with no open/running left and the goal not met → per the hard bottom line above, you must open a new one or re-dispatch.
   - **Depth over coverage**: coverage is a floor/acceptance criterion, not the exploration goal itself; once you find a high-value entry point (potentially leading to RCE/privilege escalation/data exfiltration), prioritize dispatching an intent to [drive that path deep], rather than spreading out for broader coverage with shallow per-asset tests.
   - **Keep routes diverse, don't converge too early**: while the goal isn't met, if all existing intents are crowded onto the same route/entry point, and there's an [essentially different] uncovered direction (another entry surface/another asset class/another exploit chain), prioritize adding that divergent direction rather than piling synonymous intents onto the same line (judge by substantive difference, not wording); if that divergent direction is already covered by an existing intent, still don't generate one. Ideally 2–3 mechanistically different routes coexist (e.g. "attack via the upload chain" and "attack via auth bypass"), and resources only consolidate onto one once it yields evidence of [closing in on the goal]. **But diversity always yields to the [OPERATION CONSTRAINTS] at the top**: an entry surface/port/host/operation excluded by a constraint never gets an intent, even if it's essentially different.

   **Serial exploit chains: dispatch step by step, don't split into parallel.** For a strongly-dependent serial chain (①→②→③, where each step depends on the actual output of the previous one): don't dispatch all of it in parallel at once (a downstream step that can't get its not-yet-existing prerequisite will just repeat/spin uselessly); use TodoWrite to record the whole chain as a todo list (one entry per step), and this round only dispatch the step whose "prerequisite is already satisfied" (usually the first one); once it produces a fact, dispatch the next step on the next wake-up (the prompt will carry the todo list) and mark the satisfied one completed. Don't split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" is one step); only use multiple parallel intents for dimensions that are [parallel and mutually independent] (e.g. enumerating several unrelated endpoints).

5. **Submit**: submit the filtered new directions with [one] add_intent batch call (the intents array, at most the 4 highest-value ones — don't call it repeatedly one at a time):
   - **summary**: a one-sentence natural-language description of the direction (the full target address being tested + what to do + why), without forcing it into a fixed category — dedup mainly relies on comparing it against existing intents.
   - **asset_ids**: the id(s) of the target asset(s) this direction will test/attack (pass them whenever possible — 0/1/many, from list_assets) — whenever the direction revolves around a concrete asset (site/interface/parameter/host) you must pass it, to drive coverage dedup and link into the asset chain; pass all of them when it spans multiple assets; leave empty only for pure global reconnaissance with no concrete asset.
   - **parent_ids**: which upstream node(s) this direction was synthesized from (optional, 0/1/many) — pass all of them when multiple facts combine into one intent, also pass the id when derived from an upstream intent/finding, and leave empty for a brand-new top-level direction.

Don't duplicate, don't force it; but when the goal isn't met and there's an uncovered, deeper approach available, dispatch it. Be concise, focused, and efficient.`

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
	// Domain tools + the base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// When the asset-coverage feature is off, drop add_task_scope/list_untested_assets (keep them out of the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// The key situation (the just-finished intent + the prefetched full graph) now goes
	// into [this round's user input] (see input below); system keeps only the static
	// planning body. Moving it out keeps system stable round to round, which caches
	// better; the trade-off is that if a single round grows long, the situation may get
	// compacted (a planner round is usually short, so the risk is low). situational gets
	// appended into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final-round mode (injected via ctx, see taskclock.go). On the
	// final round, the task-timeout planner wrap-up phrase is appended into this round's
	// user input (alongside situational) as [this round's operating instruction], so it
	// only does the final goal judgment and produces no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[TASK FINAL WRAP-UP (this round's special instruction, overriding the normal planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID>; create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // inject operation constraints (if any) into the system prompt to bound the exploration scope
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own; when there's a deadline, clamp
	// MaxDuration to what's left, so an in-flight planning round wraps up once the task
	// hits its deadline (timeout → task-timeout phrase, step-count → per-run phrase).
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
		EnableWebFetch:  true, // routed through the recording proxy for traffic capture; loads the proxy's CA to verify its MITM-resigned HTTPS certs
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), unrelated to the traffic-recording MITM proxy; empty means direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands default to the proxy + trust its CA
		WorkingDir:            taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = unlimited; with a deadline = time remaining until it
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todos shared across wake-ups: lets a serial chain persist across rounds (the session is new each time, but the store isn't).
		Todos: p.todoFor(ts.ID()),
		// Hitting [this round's] step budget → the SDK runs wrap-up: land the conclusions
		// already worked out this round (the add_intent that should be dispatched, the
		// prove_goal that can be proven, recording a serial chain via TodoWrite) rather
		// than stopping planning altogether — the planner will keep being woken up
		// afterward. When clamped (squeezed by the task deadline), PromptByReason is used
		// instead (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // when this profile selects non-streaming, go through Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = send no cap, determined by the server default
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental feature: once enabled, noa takes over context compaction
	// (archives are centralized, persistently, under <workDir>/noa/<SessionID>).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The situation (the just-finished intent + the full graph) now goes into this
	// round's user input (see input below). user also carries the instruction +
	// cross-wake-up todos (the todo is the model's own planning scratchpad,
	// regenerable, so it's fine to put it in user).
	// The lead-in splits into two cases based on "whether this round has a concrete
	// change": a change → point at the [actual change] block below; no change
	// (heartbeat/timed check / hint / resume, etc.) → don't falsely claim "the graph
	// changed"; instead, suggest reviewing running intents in passing.
	lead := "There's a concrete change just now (see [THE ACTUAL CHANGE(S) THAT TRIGGERED THIS ROUND] below) — plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This round is a wake-up from **a scheduled check-in (heartbeat) / no concrete-change signal** — the graph may not have any new changes. While you're at it, review running intents: use steer_work to correct one that's made no progress for a long time or has drifted, use kill_work to cut your losses on one whose direction is entirely wrong; then judge the goals and decide whether to add direction:"
		// On a heartbeat/no-change wake-up, if the whole graph has no open or running
		// intents at all → exploration has stalled (no worker running, no direction
		// queued). Explicitly tell the planner and force it to add a new direction this
		// round, instead of just reviewing running intents and spinning idly.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a wake-up from **a scheduled check-in (heartbeat)**, and there are currently **no open or running intents at all** — no worker is running, and nothing is queued; exploration has stalled. You **must** produce one or more new intents this round that advance the goal and are **mutually non-duplicate** with the intents already in the graph (producing 0 intents is not allowed); first judge from the situation below whether the goal is already met, and if not, add direction immediately:"
		}
	}
	input := lead + situational + "\n\nJudge the goals from the situation above. Once a goal is [truly met] (the target outcome has been obtained / the target vulnerability has been confirmed), mark it with prove_goal one at a time. **Hard bottom line: as long as the goal isn't met yet and there are currently no open or running intents (frontier_open=0 and running_intents is empty), this round must produce at least one intent that advances the goal — in that case there's no running work to wait on and no queued direction, so producing 0 intents means the task stalls. Only when an existing open/running intent is already advancing things, or the goal is already met, may this round produce no new intents.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts an in-flight tool call and wraps up in place once the
	// wall clock runs out (on the live ctx), so a round stuck forever no longer bypasses
	// wrap-up, and no external hard-ctx backstop is needed. ctx only carries pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
