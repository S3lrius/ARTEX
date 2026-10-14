package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns built-in tools from pure code into a catalog that can be
// enumerated and overridden by the DB:
//   - BuiltinToolSeeds(): expands the three execution agents' built-in tool sets
//     into seed records (key + description + parameter schema + agents bound by
//     default) so the server can seed the tools table idempotently at startup.
//   - The ToolResolve hook: at runtime, filters the assembled tools by agent from
//     the tools rows in the DB, overrides description/schema, and injects parameter
//     defaults. key/handler stay in code. The DB changes only the prose and the defaults.
// The handler (Call behavior) always comes from code — the DB cannot change it, only
// the description the model sees and the default arguments.

// ToolSeed is a seedable snapshot of a built-in tool: key is CoreTool.Name() (bound
// to the handler, read-only in the UI), Desc/Schema come from the tool definition
// in code, and Agents is which agents the code gives it to by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, not editable
	Desc   string         // top-level description (overridable in the UI)
	Schema map[string]any // parameter JSON Schema (structure is read-only; description/default can be edited in the UI)
	Agents []string       // agent keys bound by default (worker/planner/mainagent)
}

// builtinToolsByAgent builds each execution agent's domain tool set with a
// read-only empty-shell ToolSet (nil stores). Tool constructors only put closures
// into Spec and do not dereference the store at construction, so nil is safe —
// these tools are used here only to read Name()/Description()/InputSchema(), never Call.
//
// Deliberately excludes the SDK general tools actool.DefaultTools() (Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash): every agent always has them, there is no choice of who they bind to, and
// most of their description lives in Prompt() (this table only overrides Description(),
// which would be a misleading half-override). Not seeding them means no DB row, so
// ToolResolve lets them through unchanged, the same as before. Only ARTEX's own domain tools are managed in the table.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (the goal decomposer) binds set_goals + set_constraints by default: that
		// is how the split goals and the extracted operation constraints are written.
		// It shares those managed tools with mainagent; the web UI can edit description/schema and toggle them per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto binds vulnerability reporting and asset-management tools by default. Other domain tools can be toggled in the UI.
		// A new database is written from this seed; an old database is migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (the solo pentest agent) binds by default: list assets / insert assets / report a vulnerability / list vulnerabilities / list companies.
		// A new database is written from this seed; an old database is migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools still enter the catalog (visible on the web, and
// an agent can be checked by hand), but by default they bind no agent — ToolResolve
// drops an unbound tool for every agent, so it requires an explicit opt-in.
// They stay in some agent's base tool set (for example goal_met in PlannerTools) for
// two reasons: the seed can construct them and read desc/schema, and after a user
// binds one back it is in the runtime base so ToolResolve can keep it.
//
// goal_met skips proving goals one by one and declares the whole task complete from
// the global view. It has high weight and a risk of a wrong call, and it duplicates
// "prove_goal marks the last goal → automatic close-out", so no agent gets it by
// default. Bind it by hand when it is needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds merges each agent's built-in tool set into a deduplicated seed list: a tool with the same name (for example list_assets, which several agents have) becomes one row, and Agents is the union. Tools in defaultUnbound are forced to an empty binding.
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
			agents = []string{} // in the catalog and bindable by hand, but given to no agent by default (store [] rather than null, consistent with the other tools)
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
// default arguments get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
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
