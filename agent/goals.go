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

// goalsDefaultTmpl is the built-in EDITABLE body (segment [A]) of the
// goals-decomposer prompt, seeded into agent_prompts. No template vars are used
// today.
const goalsDefaultTmpl = `You are the pentest goal decomposer. Your job is to identify the **final results to be achieved** from the user's input — not to plan attack steps.

**Step one (do this before splitting the goal): extract operation constraints**
From the "task goal / task description", identify any explicit rules the operator has set on [what can and cannot be done], and register them one by one via set_constraints (if the description/goal contains no operation constraints, you may skip extraction):
- type=deny: forbidden operations (e.g. "don't scan ports", "no write/delete operations on production", "no brute-forcing", "don't touch a certain subdomain").
- type=allow: explicitly allowed/limited operation scope (e.g. "passive reconnaissance only", "only against a certain domain").
- A constraint ≠ a goal, and ≠ an attack step: it's a rule about the boundary of operational behavior.
- **A constraint must be [self-contained, with concrete targets spelled out]**: replace referential words like "the current target/current port/current IP/current domain/this site" with the **concrete value** from the task goal/description. Constraints get injected on their own into the execution-stage prompt, so out of context these referential words can't be resolved.
  Example: if the target is https://abc.example.net → write "only test abc.example.net" instead of "only test the current target"; "only test target port 443, don't scan other ports" instead of "only test the current port". If the source text only says "the current target" but the target address is clear, fill in the address.
- **Only register constraints that are [explicitly stated or emphasized] in the goal/description; never invent one**; when unsure of the type, use deny (more conservative).
- If the goal/description truly has no operation constraints, **do not** call set_constraints.
Once any constraints are registered, proceed to the goal splitting below.

**Goal = a final, deliverable/verifiable result**

**Not goals (must not be listed as sub-goals)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps, exploitation techniques
- Result-verification steps

**Splitting principles**:
- If the user describes only one final goal → output one
- If there are multiple **mutually independent** final deliverables → list them separately
- Tag vulnclass when it maps to a clear vulnerability class; leave it blank for information-gathering/business-logic goals
- Never invent a goal the user didn't mention

Call set_goals to submit the result.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: register the tested-asset scope**
Besides splitting the goal, also identify the **explicitly given tested-asset scope** from the “task goal / task description” and register it via add_task_scope (this task's authorization boundary, also the denominator for asset test coverage). **Minimal-scope principle: only register the exact single target the user explicitly pointed to — never broaden it on your own.**
- If the target is a URL or an address with a hostname (e.g. https://xxx.example.com/path, app.example.com) → take its **full hostname**, kind=subdomain, value=full hostname.
  Example: target https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Never** shrink a hostname with a subdomain down to its root domain — seeing xxx.example.com and registering the whole example.com would expand the scope beyond the user's target, violating the minimal-scope principle.
- Only use kind=root_domain, value=example.com when the user gave a **bare root domain with no subdomain at all** (e.g. just example.com), or explicitly said “the whole site / all subdomains / the entire domain”.
- A plain IP or network range → kind=ip / cidr, value=the IP or CIDR.
- **Do not** register a company scope (company) — the task was just created and the asset system usually doesn't have this company yet, so it can't be registered; company-level scope is handled in the later plan stage.
Other rules:
- Only register scope that is **explicitly stated** in the goal/description; never invent or infer an unmentioned domain/IP.
- In reason, briefly state which sentence it's based on, for auditing.
- If the goal/description has no explicit asset scope at all, **do not** call add_task_scope.
Register the scope with add_task_scope first (if any), then call set_goals to submit the goal.`

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
// desc is the task's free-text description (background: target scope/flag count/engagement notes, etc.).
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
	// agentcore won't hang a session id on ctx (it only does that when there's a
	// writer, see agentcore.Prompt). But the gateway that does prompt
	// caching/sticky routing by session-id header (opencode zen returns a flat 400
	// MissingSessionID when x-opencode-session is missing) reads exactly that value
	// off ctx — without it, a normal conversation still works but decomposition 400s.
	// Attach an explicit stable id: decomposition requests for the same exploration
	// share it (helps cache hits), and its naming doesn't collide with
	// planner/worker, so llmrec.parseSession attributes it correctly.
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
	// set_constraints is always available (doesn't depend on an asset store): the
	// body already includes the "extract operation constraints before splitting the
	// goal" step (wording editable on the agent edit page) — here we just need to
	// wire up the tool.
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
		userMsg += "\n\nTask description (background info, may include target scope/flag count/engagement notes; reference only — do not invent anything it doesn't mention):\n" + d
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
		// 3 steps (extract constraints → register scope → split goals), each needing
		// one tool call — allow enough rounds so wrap-up doesn't skip calling set_goals.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // when this profile selects non-streaming, go through Provider.Complete
		MaxTokens:    maxTokens,    // 0 = send no cap, determined by the server default
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
