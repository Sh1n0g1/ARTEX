package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**Finding traffic evidence (optional)**: when calling report_finding to report a finding, if there is an HTTP request/response you've reviewed and confirmed supports the finding's conclusion, you may bind the real IDs via traffic_refs in reproduction order; domain and time are only used to narrow candidates, not to presume a match. For TCP and other non-HTTP findings, or when traffic wasn't captured or there's no exact match, omit it or pass [], and keep other verifiable evidence (command output, logs, etc.) in `evidence` — it's recommended to state why nothing was bound. Don't guess IDs, and don't re-probe just to fill in a binding."

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
