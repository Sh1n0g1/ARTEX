---
name: api-recon
description: Invoke this skill when gathering a website's API endpoints.
---

# API Recon (Frontend Interface Reconnaissance)

Under the assumption of **prior authorization**, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature trigger points** (tabs, dialogs, table actions, etc.).

---

## Boundaries and Prohibitions (Agent must read — crossing these is out of scope)

This skill performs **API / parameter surface reconnaissance only** — it is not a vulnerability discovery or exploitation phase.

### Task boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate paths, methods, parameters, routes, UI trigger points | SQLi/XSS/privilege escalation/brute force/fuzzing vulnerabilities, request tampering attacks, destructive operations |
| **Auth** | Hook + stub/mock to bypass the **client-side** login gate | Requesting or guessing credentials from the user; attempting real login form submission |
| **Runtime** | Hook endpoints without credentials, using mock responses to get the SPA into its post-login shell | Flows that depend on a real backend session to continue |

### Credential-free dynamic analysis (Phase 3 default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap endpoints such as login, permissions, and menus;
2. Return mock bodies for business query endpoints that are **structurally correct, report a successful business code, and may have empty data**;
3. Make the frontend render its post-login pages even without a backend or under a 401 condition, thereby triggering more XHR/fetch/WebSocket calls;
4. **Empty data, blank tables, and placeholder UI are all expected** — don't switch to real login or vulnerability testing because of this.

**In one sentence**: use mocks to open up frontend routes and component mounting, **recording only outbound requests**; what the backend returns doesn't matter — what matters is **which additional endpoints the frontend goes on to call**.

### Hard process prohibitions

| Prohibited | Do instead |
|---|---|
| grep/curl/Read the main entry `index-*.js` to extract API paths before Phase 1 is complete | Run `OUTDIR/harvest_static.py` |
| Hand-writing `extract_apis.py` or similar scripts in place of harvest | Modify `OUTDIR/harvest_static.py` and rerun it |
| Repeating the same grep/command ≥2 times after it fails | Change strategy: read tool_logs, modify harvest, check the reference |
| Skipping Gate A/B and running the original `scripts/` directly | Copy to OUTDIR and adapt it to the target |
| Using real usernames/passwords, OTP, OAuth, or other credentials | stub/mock (see above) |
| Skipping the stub to "get real data" and performing privilege-escalation/injection testing | Only record outbound requests — that's the boundary of recon |
| Deleting, exporting sensitive data, bulk writes, or other irreversible operations | Also applies to coverage clicking |
| Claiming all pages and endpoints have been obtained without completing runtime + dynamic enumeration | See "Definition of Done," or note the limitation |
| Claiming all parameters are known without completing the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff, or validation rules / error-based inference |

---

## Two-Layer Model + Run Modes

| Layer | Output | Limits |
|---|---|---|
| **Static** (JS bundle) | Full set of endpoint paths, draft routes, candidate payload-assembly fields | No HTTP methods; parameters require Phase 1b; misses URLs built dynamically at runtime |
| **Runtime** (live session) | Methods + bodies + responses + dynamic URLs + WS/SSE; multi-sample diff fills in parameters | Requests only fire once a page actually renders; a single sample isn't enough to determine required/optional |

| Run mode | Engine | Use case |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response bodies, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Clicking tabs/dialogs/tables, deeper feature coverage |
| **both** | depth first, then coverage | Most complete, takes longest |

**Parameter methodology** (no generic script): use harvest/regex for paths; use **anchor-window expansion + UI binding-chain tracing + multi-sample diff + error-based inference** for parameters (grep recipes in [reference.md](reference.md) section J).

---

## Definition of Done

Recon can only be declared complete once all of the following hold:

- [ ] **Static**: Phase 1 harvest has produced `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both require **hooks working + the dynamic enumeration loop**
- [ ] **Into the shell**: visiting a business path doesn't land on `/login` (watch for hash routing)
- [ ] **Parameters**: coverage/both have completed the parameter trigger matrix + `param_samples.json`; Phase 5 merges into `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission-tree reconstruction done and rerun, until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs are all present (see the Phase 5 output table); `insert_assets` has written service and endpoint assets

---

## Scripts and Gates

`scripts/` are reference templates only — running the original versions as-is and treating them as the final result is **prohibited**.

**Rule**: read first → adapt to the target → write into `OUTDIR` (e.g. `recon/`) → log it in `CHANGES.md`; if it doesn't fit, rewrite following the methodology and only borrow the structure.

| Gate | When | Reference script → OUTDIR copy | Commonly required changes |
|---|---|---|---|
| **A (static)** | After Phase 0, before the **first** run of harvest/spider | `harvest_static.py` / `spider_mpa.py` | **Default regex works out of the box for most sites**; only adjust the endpoint regex, webpack/Vite `publicPath`, or MPA exclude/cookie when the manifest/dialect doesn't match |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**Mandatory SPA order** (not interchangeable; Phase numbering takes priority over "explore first, then script"):

| Step | Required | Prohibited |
|---|---|---|
| After Phase 0 completes | Next Bash command = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | Copy script → make small adjustments as needed → **run it immediately** | Manually extracting APIs first and deciding afterward whether to harvest |
| Before Phase 1 completes | Validate output with `wc -l`; retry harvest on 404s | Hand-writing an extract script; repeatedly grepping a URL that hasn't been downloaded |
| From Phase 1b onward | grep only `OUTDIR/js/*.js` | Using the main bundle instead of harvest |

- Correct: copy `harvest_static.py` → (optionally) adjust the regex → **run it immediately**
- Wrong: curl the main bundle → grep repeatedly → write a throwaway extract script → only harvest at the end
- **MPA**: after Phase 0, the next Bash command = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and Output Constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` files >100KB are **forbidden** from being Read/grepped into context; batch-process them with OUTDIR scripts |
| grep output | Always pipe through `\| head -20` or use `-m 5`; keep only path summaries in the conversation, don't paste bundle snippets |
| Validation | Use `wc -l`, `ls \| wc -l`; don't Read an entire directory |
| Initial regex probing | Optional, ≤1 time, only on chunks/HTML ≤50KB; formal static analysis should rely on harvest |
| reference | Recipes/templates/troubleshooting are in [reference.md](reference.md) — don't duplicate the full text inline |

---

## Execution Roadmap

```
Phase 0 classify + OUTDIR
  → Gate A → Phase 1 harvest (★ run it immediately ★)
  → Phase 1b parameter reverse-engineering
  → Phase 2 three auth gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (if needed) → rerun Phase 3
  → Phase 5 merge report + insert_assets bulk-inserts all discovered services and endpoint assets — no discovered asset may ever be omitted from this insert
```

Check off items in order; **the next Phase cannot begin until the previous one is complete**.

1. [ ] **Phase 0**: initial SPA/MPA triage; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy script → harvest **immediately** → validate with `wc -l` → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor-window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: three auth gates → `config.json` → [Phase 2](#phase-2--three-auth-gates)
5. [ ] **Gate B**: adjust runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm you're into the shell; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-reconstruction)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch the entry HTML and **create `OUTDIR`** (don't modify the skill's own `scripts/`):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produces `forms.txt`, `links.txt`, `api_inline.txt`. If an SPA has forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts and Gates](#scripts-and-gates) and [Tool and Output Constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parses HTML scripts → the webpack/Vite manifest → downloads every lazy chunk → produces `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- Chunk count vs. manifest: retry harvest on 404s rather than manually curling chunks one by one
- If `api_static.txt` has too few entries → loosen the endpoint regex inside OUTDIR and rerun (see reference)

### Phase 1b — Parameter reverse-engineering

Paths come from Phase 1; parameter fields need their own dedicated recon. See [Tool and Output Constraints](#tool-and-output-constraints) for grep rules.

**Completion criteria**: for each important endpoint you can answer — field name, where it's transported, inferred type, whether required, sample value, confidence level.

#### 1b.0 — Transport shape

| Shape | Where parameters live | What to check first in static analysis |
|---|---|---|
| REST JSON | body + query | Near the path anchor: `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameters | `/user/:id` | Route table + `useParams` / `$route.params` |
| Encryption/signing | Wrapped inside `sign`/`data` | Hook the encryption function's input args (reference section D) |

Output: tag each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor-window expansion

Use a known path as the anchor, then expand the window to find the payload-assembly object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clues |
|---|---|
| axios instance | `data` / `params` |
| Unified request helper | Global fields injected by interceptors |
| OpenAPI client | Generated method signatures |
| React Query / SWR | Hook's second argument |
| Vue composable | Composable's input args |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | Trace submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | Global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

Trace the payload-assembly function via the DevTools call stack, working up from `fetch`/`XHR.send`.

#### 1b.3 — Three payload-assembly questions (≠ Phase 2's three auth gates)

| Question | What to answer |
|---|---|
| **Assembly** | Where is the payload built; traces of transforms |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

The interceptor gate (Phase 2) also reveals globally injected fields along the way (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Handoff to Phase 3

Candidate fields come from the static/binding layer; **required/optional/conditional dependencies** require the Phase 3 parameter matrix + diff + Phase 5 error-based inference.

---

## Phase 2 — Three Auth Gates

Grep in `OUTDIR/js/` (piped through `head`) and write findings into `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How does it determine you're logged in? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers a redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Don't treat a localStorage key name as a credential — confirm it from the chunk/request chain.

**Exit = Gate B**: conclusions go into `config.json`, and you modify `OUTDIR/runtime_harvest.js` / `preload.js` accordingly.

### Phase 2b — API observation (optional)

Use `preload.js` in OUTDIR to confirm session key names, Authorization, and nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | Header observation |
| `extractUrlsFromResponse: true` | Sub-APIs found inside responses |
| `observe.storageReads/cookieReads: true` | Fed back into config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Requires Gate B to already be passed; follow the [Boundaries and Prohibitions](#boundaries-and-prohibitions-agent-must-read--crossing-these-is-out-of-scope) and the credential-free mock strategy.

Set `"runtimeMode": "depth" | "coverage" | "both"` in `config.json` (template in reference).

### Hooking and stubbing (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | auth/permission/bootstrap stubs | Get past first-screen auth |
| L2 negative correction | All JSON responses | Not-logged-in codes → success |
| L3 fallback | `/api` etc. not matched by L1 | Empty success body, to open up the UI |

- **depth**: fake auth + `forward` rewrites business codes + `stubs`; walks `routes` (hash/history); produces `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or a userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business paths don't bounce back to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — Coverage dynamic enumeration (mandatory)

1. Main nav/sidebar — click each item, wait 1–3s for network activity
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — view/edit/detail on the first row
4. Toolbar — export, filter, create new (**avoid irreversible deletes**)
5. For each module entered — merge APIs/routes
6. SPA — use controlled `pushState` for paths in `routes.txt` not yet covered (forbidden for MPA)

**Parameter trigger matrix** (mandatory): record once per operation type per module, **diff across multiple samples**:

| Operation | Parameters typically added |
|---|---|
| List first load | Pagination + default filters |
| Clicking search | keyword, filter |
| Advanced filter | More optional fields |
| Create/edit | Full entity |
| Bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Outbound body/headers remain real even under stubbing** — trust the request, not the response. Record into → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicking + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — Permission Tree Reconstruction

**Trigger**: module pages are blank / every route returns only bootstrap data (e.g. locale) → the content gate hasn't been passed.

| Symptom | Meaning |
|---|---|
| Got into the shell successfully | Render gate + interceptor gate passed |
| Sidebar missing items / clicks produce blank pages | Stub shape or permission codes incomplete |
| Same, very small set of APIs on every route | `v-if permission` not passing |
| `routes.txt` far smaller than the bundle | Needs to be backfilled from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub check: the outer `response_code` matches the interceptor gate; flat codes align with the tree; `routes` covers every link in `route_map`.

**Rerun Phase 3** after updating `config.json`. For large SPAs, tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — Merge and Report

### Output table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundle and paths |
| `param_candidates.json` | 1b | Candidate static parameter fields |
| `config.json` | 2 | Three gates + runtime config |
| `runtime_api.json` | 3a | Detailed depth recording (including WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multiple samples, click log, details |
| `route_map.json` etc. | 4 | Permission tree intermediate files (if run) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | Writes all service and endpoint assets into the asset library |

### 5b — Parameter merging

Diff from `param_samples.json` — **there's no generic merge script**. Confidence rules are in reference section J7 (high/medium/low/untriggered).

### 5c — Error-based inference

Within the authorized scope, you may send incomplete requests to read 400 responses (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Watch for `data` wrappers, `variables`, and `bizData` before encryption.

The report must note: runtimeMode, number of static/runtime APIs, parameter confidence, uncovered modules, and a `CHANGES.md` summary of changes relative to the reference scripts.

Suggested `site_map.json` structure:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

More fields and grep recipes are in [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: webpack/Vite/Angular lazy-loading works the same way
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions aren't fully enumerable
- **Blind spots**: JSVMP, WASM, strict HMAC/mTLS validation → fall back to static analysis + note the limitation
- **Parameter blind spots**: conditional dependencies, hidden params, WASM payload assembly → mark as "untriggered"/"unreachable"
- **Static analysis is the safety net**: when runtime is blocked, static analysis can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` template, troubleshooting, hooks, parameter reverse-engineering (section J), site_map template: **[reference.md](reference.md)**
- See the [Scripts and Gates](#scripts-and-gates) table for reference script paths
