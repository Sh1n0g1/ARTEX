# api-recon — Reference Manual

Grep recipes, the `config.json` template, and troubleshooting. All greps run against the `js/` directory. If a bundle is minified onto a single line, run it through `js-beautify` or `sed 's/}/}\n/g'` first — a raw grep with a context window is usually enough otherwise.

## About the Scripts

All files in `scripts/` are **reference templates** and must be adapted to the target site before running. Typical things to adjust:

| Script | Commonly needed adjustments |
|---|---|
| `harvest_static.py` | Endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub matching rules and body structure, routes source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` for destructive links, cookie, depth/max, same-domain filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | The single entry point for all of the above site-specific parameters |

Adjusted files should be placed in the task's working directory (e.g. `recon/`); note the specific changes relative to the reference scripts in the report.

---

## A. The Three Reverse-Engineering Gates

### A1. Render gate — "How do you tell you're logged in?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` to determine the **storage key**, the **container** (Cookie vs localStorage), and the **encoding**:

| Encoding | How to forge it in config |
|---|---|
| Plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | An unsigned/`alg:none` JWT, or sign it with a key found in the bundle |
| Encrypted (SM2/AES/RSA) | Look for a hardcoded key; you can forge it when the render gate only needs a decodable blob, otherwise fall back to static analysis |

→ Write into `cookies` / `localStorage`.

### A2. Interceptor gate — "What triggers a redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: the **field name**, the **success value** (usually `0` or `200`), and the **failure value that triggers a redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Write into `neutralize.fields` + `neutralize.success`.

### A3. Content gate — "Where do menus/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise admin backends):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | Route guard, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | Sidebar menu rendering |
| `userRouteAuth` in the bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in the bundle | `{ KEY: { name, link } }` | Alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forging** (small sites): build a permissive payload → `stubs`.

**Full permission-tree reconstruction** (large sites where the sidebar/submodules are still blank): see **section I**.

---

## B. config.json Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field notes:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), `both`
- `cookies[].value` prefixes: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal value
- `forward: true` forwards real requests and rewrites the code field; `false` is fully offline stubbing
- `mockTier`: which layers the coverage-mode preload enables, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging the menu, the harness automatically appends `<a href>` entries
- `captureResponses` / `recordWs` only apply in depth mode
- `waitUntil`: use `domcontentloaded` for large SPAs to avoid `networkidle2` hanging
- `routeTimeout`: timeout (ms) for a single route's `page.goto`
- `proxy`: Puppeteer's `--proxy-server`; `HTTP_PROXY` / `HTTPS_PROXY` env vars also work

### B1. Dual stub template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

The outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must cover every leaf code in the tree.

---

## C. Coverage Mode: preload Configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before CDP injection:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify: `window.__API_RECON_PRELOAD__ === true` and the pathname is stable.

Export the recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook Capabilities

Browser hooking capabilities built into preload (coverage) and runtime_harvest (depth), and their coverage:

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | Records request URL/method | Yes — `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Discovers Authorization and other headers | Yes — `observe.xhrHeaders` |
| Hook localStorage/cookie reads | Confirms session key names | Optional — `observe.storageReads/cookieReads` |
| Vue route retrieval | Fills in frontendRoutes | Yes — `__API_RECON_ROUTES__` (already-loaded routes) |
| Vue route guard neutralization / login-redirect blocking | Opens up modules to trigger APIs | Yes — `neutralizeVueRouter` + native navigation neutralization |
| React route retrieval | Fills in routes | Partial — static analysis + clicking only; no dedicated hook |
| Page-navigation blocking (login path) | Keeps the page for analysis | Partial — only blocks the login path, to avoid blocking business navigation |
| Hooking encryption libraries (CryptoJS/SM, etc.) | Encrypted parameters → plaintext API body | No — must manually hook the encryption function's input args; record conclusions in config |
| Anti-debugging bypass | Otherwise runtime can't record APIs | No — must be handled manually; static analysis still works |

---

## E. Endpoint Extraction Regex (when static results are too sparse)

Loosen `extract_endpoints` in `harvest_static.py`, or do it manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → Fix |
|---|---|
| Very few static APIs | Endpoint dialect mismatch → loosen the regex (section D) |
| Chunk count ≪ manifest | CSS-only or undeployed chunks; 404s already retried |
| Runtime still shows the login page | Render gate is wrong → recheck A1: key name, container, encoding, domain |
| Into the shell but modules are blank | Content gate → forge the menu (A3); the `routes` path may be wrong |
| Every route returns only bootstrap/locale | Permission codes incomplete → section I permission-tree reconstruction; check the `role_permissions` + `permissions/all` dual stub |
| Sidebar has items but subpages are blank | Tree is missing an intermediate node, or the code doesn't match `userRouteAuth` |
| Every API redirects to login | Interceptor gate → confirm `neutralize`; nested fields need extended walk logic |
| WS frame count is 0 | Subscription only happens after user interaction; increase `perRouteMs` |
| Response body is empty | Only `forward: true` produces real responses |
| Chromium missing | Install Chromium or set `config.chromium` / `CHROMIUM` |
| Lots of mocking but still redirects to login | Hook fired too late, or missing a `location.href` setter → use document-start + preload |
| List is entirely empty | An L3 empty array is normal; keep clicking tabs/settings/details |
| Mistaking a Redux action for a route | Filter out internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload isn't document-start → change the injection timing; or manually clear the guard when `neutralizeVueRouter: false` |
| URL appears in the response but didn't make it into the log | Turn on `extractUrlsFromResponse`; or extract manually from `__API_RECON_DETAIL__` |
| Don't know the Authorization header name | Turn on `observe.xhrHeaders`, or check request headers in DevTools |
| Runtime extremely slow / times out | Switch to `waitUntil: domcontentloaded`; lower `routeTimeout`; avoid `networkidle2` |
| Proxy connection fails | Check `proxy` / env vars; make sure Puppeteer and curl use the same proxy port |

---

## G. Hardened Targets

When the server progressively validates the session (unforgeable signed cookies, server-rendered menus that can't be stubbed), runtime will get stuck at the shell. Expected behavior:

- **Static analysis is sufficient for endpoint enumeration** — module paths are in the code
- If authorization permits, run the same harness with a **real session**: `forward: true`, no neutralize needed, capturing real methods/params/responses

---

## H. Single-Task Checklist

1. Confirm the scope of authorization
2. **Read** `scripts/harvest_static.py` → adapt it to the target → run it → review `api_static.txt`, `routes.txt`
3. **Phase 1b**: path anchor-window expansion + binding layer → `param_candidates.json` (section J)
4. Reverse-engineer A1/A2/A3 → write a site-specific `config.json`
5. **Read and adapt** `runtime_harvest.js` / `preload.js` before running it
6. `runtimeMode=depth`: `npm install` → run the adjusted harvest script
7. `runtimeMode=coverage/both`: inject the adjusted preload at document-start → browser MCP dynamic enumeration + **parameter trigger matrix**
8. Modules not rendering → **section I permission-tree reconstruction** → patch stubs → rerun
9. Multi-sample parameter diff + error-based inference → `params_merged.json`
10. Merge → `site_map.json` + `api_merged.txt`, honestly noting coverage, gaps, and script changes

---

## I. Permission Tree Reconstruction (Phase 4 Deep Dive)

Use this when forging a simple `menus: [{ path, show: true }]` doesn't work and submodules still don't mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, the **response field names**, and the **consuming chunk's filename**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If you get `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parses `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases like `He=o.DASHBOARD`)
2. Resolves aliases → real paths using `route_map.json`
3. Infers parents from code prefixes (`MONITOR_ALERT` → `MONITOR`)
4. Outputs `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, automatically writes `stubs` into `config.json` and extends `routes`

**Adjust for the target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes with no prefix relationship

### I4. Verify stub consistency

```bash
# permissions count should be ≈ the number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover every link in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare runtime_api.json counts before/after forging; check whether module APIs like /attack, /asset appear
```

| Before forging | After forging (success) |
|---|---|
| Same 3–5 bootstrap entries on every route | Different routes trigger different module APIs |
| Only `/api/locale/language` | Module endpoints like `/api/web/...` appear |
| A handful of routes in `routes.txt` | 80–110+ `routes` sourced from route_map |

### I6. If it still fails

- **Coverage mode**: click the sidebar + tabs — permission gating may only fire the request after interaction
- **Stub fields**: compare the nesting of the real API (curl + real session) against the stub
- **Extra guards**: grep for button-level checks like `hasPermission|checkRole|func.`, and extend `role_permissions.permissions`
- **Static fallback**: module API paths are still in `api_static.txt`; runtime only fills in METHOD/body — keep parameters from `param_candidates.json` + recorded samples

---

## J. Parameter Reverse-Engineering (Phase 1b / 5b / 5c)

**A methodology, not a generic script.** Use regex to find paths; use anchor-window expansion + UI binding-chain tracing + multi-sample diff + error-based inference to find parameters.

### J1. Anchor-window expansion — finding the payload-assembly object from a path

```bash
# anchor on a path already known from Phase 1
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper layers and transport shapes

```bash
# axios / unified request helper
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation gate — required / format / enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding layer — form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Filling gaps at runtime: DevTools → Network → request → **Initiator** (call stack), tracing the payload-assembly function upward from `fetch`/`send`.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Don't guess fields from ciphertext** — hook the encryption function's **input arguments** and record the plaintext payload before encryption; write conclusions into `config.json` / `param_candidates.json`.

### J6. Parameter trigger matrix (mandatory in Phase 3)

Record once per operation per module, diffing the request body/query:

| Operation | What to watch for |
|---|---|
| List first load | Pagination defaults |
| Search | keyword, filters |
| Advanced filter | optional fields |
| Create/edit | Full entity |
| Bulk/export | `ids[]`, `exportType` |
| Sort/paginate | `sortField`, `order` |

Produces `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **High** | Static callsite + ≥2 consistent runtime samples |
| **Medium** | Static only, or just 1 runtime sample |
| **Low** | Inferred from a response/error, not re-verified |
| **Untriggered** | Field known from static analysis, but UI/permissions never reached it |

### J8. Scenario quick-reference

| Scenario | Order |
|---|---|
| REST list page | J1 payload-assembly object → J6 four-way diff → J3 rules |
| Create/edit form | J3 Form name → J4 submit chain → runtime submission + deliberately leave fields blank to see the 400 |
| GraphQL | J2 variables declaration → record variables for each runtime operation |
| Encrypted body | J5 hook the input args → the pre-encryption fields are the real params |

### J9. Mapping to api-recon phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 static | J1 anchor-window expansion |
| Phase 2 A2 interceptor | Globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | Different modules have different forms → full fields only trigger with sufficient permissions |
| Phase 5 merge | `params_merged.json` + confidence; don't determine required-ness from a single sample |

### J10. Troubleshooting

| Symptom | Fix |
|---|---|
| Field name known from static analysis but never appears at runtime | Mark as "untriggered"; backfill the permission tree / click advanced filters / try each option of linked selects |
| Same path, different body shapes | Normal — record separately by `action`, don't force-merge the schema |
| Stub response is fake but you want to see params | **Look at the outbound request's** body/headers, don't infer from the stub response |
| 400 reports a nested field | Watch for outer wrappers like `data`/`bizData`/`variables` |
| GraphQL only shows the operation name | Expand the `variables` JSON; find `$var: Type` in static analysis |

---
