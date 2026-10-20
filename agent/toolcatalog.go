package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns the "built-in tools" from plain code into an enumerable
// catalog that the DB can override:
//   - BuiltinToolSeeds(): expands the three execution agents' built-in tool
//     sets into seed records (key + description + parameter schema + default
//     bound agents) for the server to idempotently seed into the tools table on startup.
//   - the ToolResolve hook: at runtime, applies "filter by agent + override
//     description/schema + inject parameter defaults" to the already-assembled
//     tool list, based on the DB's tools rows. The key/handler stay in code —
//     the DB only changes the "prose and defaults."
// The handler (Call behavior) always comes from code — the DB can never change
// it, only the description shown to the model and the default input values.

// ToolSeed is a seedable snapshot of one built-in tool: Key is
// CoreTool.Name() (permanently tied to its handler, read-only in the UI),
// Desc/Schema come from the tool's definition in code, and Agents is which
// agents code binds it to by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // top-level description (overridable in the UI)
	Schema map[string]any // parameter JSON-Schema (structure read-only; description/default editable in the UI)
	Agents []string       // agent keys it's bound to by default (worker/planner/mainagent)
}

// builtinToolsByAgent builds each execution agent's domain tool set using a
// "read-only shell" ToolSet (nil stores). Tool constructors only stuff closures
// into a Spec and never dereference the store at construction time, so nil is
// safe — these tools are only ever used here to read
// Name()/Description()/InputSchema(), never Call()ed.
//
// Deliberately EXCLUDES the SDK's generic tools, actool.DefaultTools()
// (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash): every agent has them fixed,
// with no "who it's bound to" decision to make, and their descriptions mostly
// live in Prompt() (this table only overrides Description(), which would be a
// misleading half-override). Not seeding them → no DB row → ToolResolve passes
// them through unchanged, same behavior as before. Only artex's own domain
// tools go into this manageable table.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (the goal-decomposer) is bound by default to set_goals +
		// set_constraints: it uses them to write the goals it extracted and the
		// operating constraints it derived into the DB. Shares the same managed
		// tools as mainagent; the web UI can edit description/schema and toggle per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto is bound by default to finding reporting + asset management tools;
		// other domain tools can be toggled on as needed in the UI.
		// A new DB gets this from the seed; an existing DB is migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (the solo pentest agent) is bound by default to: query assets /
		// insert assets / report finding / list findings / list companies.
		// A new DB gets this from the seed; an existing DB is migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools still go into the catalog as usual
// (visible in the web UI, manually toggleable per agent), but are bound to NO
// agent by default — ToolResolve drops any tool with an empty binding for
// every agent, requiring explicit opt-in. The reason they still sit in some
// agent's base tool set (e.g. goal_met in PlannerTools) is twofold: so the seed
// can construct it to get its desc/schema, and so that once a user manually
// binds it back, it's present in base at runtime for ToolResolve to keep.
//
// goal_met: bypasses per-finding prove_goal calls and declares the WHOLE TASK
// done directly — high-stakes with a real risk of a wrong call, and redundant
// with "prove_goal on the last goal → auto wrap-up" — so it's unbound for every
// agent by default, and bound manually only when actually needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds merges each agent's built-in tool set, deduplicated, into a
// seed list: a tool with the same name (e.g. list_assets, held by multiple
// agents) becomes a single entry with Agents as the union; tools in
// defaultUnbound are forced to an empty binding.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // goes into the catalog, manually bindable, but bound to no agent by default (stored as [] not null, consistent with other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// has default input values injected. Tools with no matching DB row (MCP/skill/host
// tools like traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
