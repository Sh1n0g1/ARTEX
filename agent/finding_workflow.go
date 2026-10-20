package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Finding ID convention**: finding_id is the independent finding-record ID; finding_node_id is the exploration-node ID. The `id` field of list_findings / list_task_findings / node_detail / get_task_node_detail stays the exploration-node ID — read the independent number from the finding_id in that same response. get_finding_traffic / bind_finding_traffic use the independent finding_id. The legacy update_finding_report's finding_id parameter still takes finding_node_id. Don't use the number on report_finding's first line for the evidence tools, and don't guess another number after hitting an ID error."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nBy default the reporter agent checks and binds traffic before writing the report. The submitter should keep verification commands, key output, and any existing real traffic IDs with their purpose in `evidence`, so the reporter agent can cross-check against the execution record — no need to re-search for packets just for binding. Explicit immediate binding is still supported: traffic_refs or evidence_hint_id can submit already-verified references, the latter reading the structured references of a hint specified for this task; if either is invalid the whole submission fails. TCP/no-packet cases don't need these optional parameters. The response's finding_id and finding_node_id denote the independent record and the exploration node respectively."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing off a confirmed finding, keep the verified traffic's ID, purpose, description, and order in the corresponding hint's traffic_refs (a single one goes at the top level, a batch goes in the matching hints element), and state in `text` exactly which finding it proves. Callers must not hand off only the text while discarding existing traffic references. Unverified candidates must not be passed along as evidence."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic evidence hand-off (optional)**: automatic binding is done by default by the reporter agent after the finding is recorded and before the report is written. The submitter should keep verification commands, key output, and existing real traffic IDs with their purpose in `evidence`, with intent_id included in the task, to help the reporter agent trace back — no need to re-search for packets just for binding. When Auto/Planner submits on the executor's behalf, don't discard references the executor already gathered. add_hint / add_task_hint can hand these off via traffic_refs; explicit immediate binding still supports report_finding's traffic_refs / evidence_hint_id. For TCP or no-packet cases, record normally — don't guess IDs, and don't re-probe just to fill in a binding."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen the platform conversation has no task context, don't call report_finding directly; hand off via add_task_hint to the existing matching task, let the task agent record it, and verify the result with list_task_findings."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore judging the goal complete, finish submitting/handing off any evidence you already have. Don't end the task or cancel the Worker just because a finding is recorded in text while the evidence hand-off is still incomplete; a missing packet doesn't require waiting or forcing a capture."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**Automatic traffic linking before the report (enabled)**: you are responsible for checking and binding traffic for the finding just triggered, then writing the report. First get the explicit finding_id and finding_node_id from report_finding's returned JSON, or from get_task_node_detail / list_task_findings. Read the finding details, the corresponding intent's execution record, and the existing evidence list, preferring the real IDs the submitter already handed off. If this verification is HTTP and the traffic tools are available, use traffic_search to narrow candidates, then use traffic_get to verify one by one that the request/response really supports the finding; domain and time are for narrowing only and don't prove ownership. Link the confirmed evidence in reproduction order via bind_finding_traffic(finding_id, traffic_refs), choosing baseline / proof / verification / supporting and stating its purpose. Only operate on this finding — don't create duplicate findings or re-probe the target. After a successful bind, call get_finding_traffic again to get the latest version, read the body you need, then pass the version you actually read as evidence_version to update_finding_report (its finding_id parameter still takes finding_node_id). Existing bindings don't need to be re-appended. For TCP, uncaptured traffic, unavailable tools, or no exact match, skip automatic binding, write the report normally from the text/command evidence and state why, and never guess just to round out the traffic. Don't claim success on a failed bind; keep the existing evidence and explain in the report why nothing was bound."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references corresponding to the specific finding in this hint, order preserved; after hand-off, report_finding can pass evidence_hint_id to carry these references.", "items": obj(map[string]any{"traffic_id": str("the real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("what conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must be a hint node belonging to this task (an inherited hint cannot be used directly for binding)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
