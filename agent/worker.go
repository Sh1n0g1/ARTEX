package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// Note the deepseek backend is different in nature from the other three:
// DeepSeek has no directly callable search API — search only exists inside its
// Anthropic-compatible messages endpoint (as the web_search_20250305 server
// tool), so each search consumes one model call, and the search request is
// issued by DeepSeek's own servers — it never goes through the local Proxy and
// is never captured in traffic recording.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* comes from the currently active LLM config (the official DeepSeek
	// endpoint in anthropic format only) — not configured separately, it changes
	// whenever the LLM config does.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "You are about to be terminated because the budget is exhausted. Do not run any more commands/probes. Please, in order: (1) write back everything you identified above but haven't written back yet, one item at a time — insert_assets for new assets, record_fact for exploration conclusions/facts, report_finding for confirmed vulnerabilities; (2) **finally, in a single standalone plain-text sentence**, summarize what you did and what key conclusions you reached (this sentence becomes the displayed result for this run, so you must output it)."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (section [A]) of the worker
// system prompt, seeded into agent_prompts. The trafficTool block and the
// intermediate-artifact output policy are NOT here — they are code-owned and
// appended by workerSystem after rendering (sections [B]/[C]), so editing the
// DB body can never drop them.
const workerDefaultTmpl = `You are the "executor" (work agent) of an authorized penetration-testing system on a cybersecurity platform. You are given ONE intent (a one-sentence exploration direction), with a single responsibility: **complete this one intent, write what you find back into the knowledge graph, then stop and return.**

**Boundaries (hard limits)**:
1. **Only do the one intent you were given.** **If, while exploring this intent, you notice a lead outside it worth digging into** (a path leaked in an error, a point that might connect with other assets, the possible entry to another exploit chain), **note it in one line in the fact's summary for the planner.**
2. Being blocked on the first attempt (payload filtered / 404 / injection with no reflection) does not mean you've fully explored it — exhaust every bypass technique for this intent before drawing a conclusion.
3. Only operate within the authorized scope. If the top of the system prompt includes Operating Constraints, that is the highest-priority hard limit: self-check every command/probe before running it, and skip it if it violates the constraints (even if it falls within your assigned intent).

**Write back as you go** (only what's written into the graph counts — what's only in your head/text doesn't; write each result the moment you get it, don't save it all for the end where it can be lost when the turn budget runs out). Three kinds of write-back — don't mix them up:
- **New asset/resource → insert_assets (asset graph)**: subdomains / services / endpoints / fingerprints / credentials — anything that is itself an asset. **Only register assets here; exploration conclusions/judgments do NOT go here — use record_fact for those.**
- **Exploration conclusion/fact → record_fact (exploration graph, pass intent_id)**: use this for everything else. **Roll multiple observations up into ONE fact** (a one-line summary + a detail that expands on it, grounded in the actual execution), rather than one fact per attribute — one intent is usually one fact; splitting things apart makes the graph balloon without limit. **Default to writing one fact, folding everything that can go into detail into detail**; only use the facts array to split into multiple entries when there truly are conclusions that are completely independent of each other and cannot be merged — that is a rare exception, not the norm. **Write only deltas**: record only what's newly obtained this time — don't re-record an existing fact just reworded (if it only confirms something already known with nothing new, there's no need to record it). **Write only what you actually observed**: give evidence (one line: the command plus the one or two lines of output that prove it best, kept concise, with detail in the detail field), and mark confidence (observed = directly seen / inferred = inferred from the symptom).
- **Confirmed vulnerability → report_finding (exploration graph, including PoC, pass intent_id)**: **use this only when you actually triggered it yourself this run and obtained reproducible evidence (a request/response or command output)**. Never treat "version/fingerprint matches a CVE", "the parameter looks injectable", or "inferred from an external vulnerability database/changelog/code diff" as confirmed, and don't substitute looking up a CVE database or comparing patch versions for actually triggering it. If you can't trigger it but suspect something → record an inferred fact via record_fact (the suspected issue + why it couldn't be triggered) for the planner; don't force it into a finding.


When this intent is done, summarize in one sentence what you did and what facts you wrote back.`

// workerTrafficBlock is section [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**Traffic tools**:\n- traffic_search / traffic_get / traffic_blob: look back at responses, find resources already visited — **check traffic first, don't re-curl the same URL**. traffic_search **requires a host**, and returns only 3 extremely lightweight index entries by default (id/method/url/status/resp_len, no response content) — raise limit explicitly for more; body_contains can do a full-text search over request/response bodies (3+ characters, substrings and non-ASCII text supported, e.g. to find passwords/keys/errors/internal addresses). To view one entry's raw content use traffic_get(id); an oversized body there shows as @blob sha256:<hash> — use traffic_blob(hash) to fetch the full text in chunks."
}

// artifactSpec is section [C]: the code-owned, non-editable tail appended to
// every pentest agent's prompt — intermediate artifacts must land in the shared
// work dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**Intermediate Artifact Output Policy**: scripts, payloads, captured response bodies, temp data, and every other intermediate artifact **must always be written to this task's working directory " + dir + "** (a relative path lands here too; the absolute path also works) — **never write to /tmp or any other absolute path**."
}

// workerArtifactSpec is the worker's section [C]: its per-intent run dir is
// pre-created by the engine (ensureRunDir), so it just writes relative paths
// there — no manual mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**Intermediate Artifact Output Policy**: scripts, payloads, captured response bodies, temp data, and every other intermediate artifact **must always be written to this intent's dedicated working directory " + runDir + "** (already created automatically — just write there using a relative path, no need to mkdir manually) — **never write to /tmp or any other absolute path**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n[Your assigned intent (your only task this run: do only this one, produce facts only, stop when done)]:\n%s\nintent id: %d (pass this when calling record_fact / report_finding)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage is a signal meant to help the planner judge "what's under-tested /
	// whether to widen scope", which conflicts with the worker's job of "only do
	// the assigned intent, don't chase uncovered points" → strip it from the
	// worker's view. data is a new map scoped to this worker call, so deleting a
	// key here doesn't affect the planner's.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n[Global exploration state (read-only, to help you place your own intent in the big picture)]:\n" +
		"Below is a snapshot of the whole task's current exploration state. It serves two purposes: knowing what others have already found so you don't repeat it, and letting you connect your own intent to the bigger picture while you explore it.\n" +
		"**Branching out is good**: while exploring this intent, think broadly and make connections freely. The only line is — don't actually go execute a different intent yourself (that belongs to another worker, scheduled by the planner). Whenever you make a connection to something valuable (a link across assets, a suspected entry point into another exploit chain, a suspicious point at the global level), **make sure to write it into a fact for the planner** — this is an important part of your output, not optional. Better to report one extra lead and let the planner judge it than to keep it to yourself.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // wakes the planner the moment report_finding is persisted, with which intent + finding
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// The worker deliberately omits MultiEdit/Glob/Grep: precise file edits use
	// Edit, searches go through Bash (grep/find) — narrowing the tool surface and
	// cutting low-value calls. The rest of the SDK's default tools
	// (Read/Write/Edit/LS/Bash/Sleep) stay as-is.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// The intent is the worker's single invariant responsibility spanning the
	// whole run → it goes into the system prompt together with the launch
	// instruction and the intent-anchored target assets' raw data: the system
	// prompt is rebuilt fresh every run, so it's never squeezed out by
	// compaction — the intent is always present through a long run, and
	// resuming doesn't depend on whether transcript history kept that first
	// message. The cost is mixing per-intent, volatile data into the system
	// prompt and losing cross-intent cache reuse; that's a deliberate trade-off
	// (losing the intent is far worse than saving some tokens).
	// This deliberately diverges from the planner, which puts its situational
	// block in a user turn: the planner is the one producing intents and has no
	// single mandate, whereas the worker does. Only the [global state overview]
	// stays in the launch user message — it's fine to degrade or go stale, so
	// squeezing it out is harmless.
	// This intent's dedicated working directory <workDir>/tasks/<taskID>/i<intentID>
	// is created ahead of time by the engine.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // operating constraints (if any) injected into the system prompt, strictly followed by the worker
	}
	// Intent block → intent-anchored asset block → launch instruction, appended
	// to the system prompt's tail in that order (same append scheme as constraintBlock).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\nTarget assets for this intent's asset_ids:\n" + string(b)
				}
				// Assets this intent explicitly targets → automatically added to the
				// task's test scope (same conservative granularity as insertAssets).
				// upsertTaskScope's ON CONFLICT DO NOTHING + the uq_task_scope unique
				// index guarantee no duplicate additions; a rerun/retry is likewise an
				// idempotent no-op.
				// When the asset-coverage feature is off, stop accumulating test scope (the denominator).
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\nBegin executing the intent above: do only it, produce only facts/assets/findings, then stop when done."
	system, boundary := deferredSystem(sysBody, def)
	// The task-level deadline (injected via ctx) clamps this run's wall-clock
	// budget and decides the wrap-up prompt (see taskclock.go).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch routes through the recording proxy, so its HTTP is captured just
		// like curl's; loading the proxy's CA lets the HTTPS certs re-signed by the
		// MITM pass verification normally (rather than disabling verification).
		// Empty proxy = direct.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey;
		// tavily needs TavilyKey. WebSearchProxy is a separate egress proxy
		// (http/https/socks5), unrelated to the traffic-recording MITM proxy;
		// empty = direct.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash subcommands' HTTP defaults to routing through the recording proxy and trusting its CA (tools need no -x/-k).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// Wall-clock budget, checked at turn boundaries, doesn't interrupt mid-turn;
		// 0 = unlimited. With a task-level deadline, clamped to min(own budget, time
		// left until the deadline), so this run naturally enters wrap-up when the
		// task's deadline hits (see taskclock.go).
		MaxDuration: maxDur,
		// Hitting the budget (turns OR duration) → the SDK runs one wrap-up turn
		// (Bash hidden), writing back what's been identified so nothing is left
		// half-done. When clamped (squeezed by the task deadline), PromptByReason
		// picks the wording: timeout = the task's deadline hit → the task-timeout
		// wording; turns = the clamped window's turns ran out first → falls back to
		// the per-run wording. When not clamped, it stays purely per-run.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. The truncation cap uses the SDK default (30000 characters).
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // session-scoped scratch todos (TodoWrite), planning only, discarded on exit
		NonStreaming:  w.nonStreaming(),                       // when this profile selects non-streaming, goes through Provider.Complete
		MaxTokens:     w.maxTokens(),                          // 0 = send no cap, left to the server-side default
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// The intent / launch instruction / intent-anchored assets have already gone
	// out with the system prompt (see sysBody assembly above). This launch user
	// message carries only the [global state overview] — degradable big-picture
	// context that's harmless if squeezed out.
	// In the rare case the overview fails to marshal and comes out empty, fall
	// back to a one-line launch prompt so the first turn doesn't get an empty user message.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "Begin executing the intent given in the system prompt: do only it, produce only facts/assets/findings, then stop when done."
	}

	// Experimental feature: once enabled, noa takes over context compaction (archives collected under <workDir>/noa/<SessionID>, persistent).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "Continue execution."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "Continue with the new intent from the last human chat message. Do not repeat actions that are already done."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n[New intent from a human chat message]\n" + message +
				"\n\nExecute this human input right away, then decide from context whether the original task still needs to continue."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n[New intent from a human chat message]\n" + message +
				"\n\nPrioritize executing this human input."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
