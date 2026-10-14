---
name: api-recon
description: Call this skill when collecting a site's API endpoints.
---

# API Recon (frontend API reconnaissance)

Under **authorization**, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI triggers** (tabs, dialogs, table actions, and so on).

---

## Boundaries and prohibitions (required reading for the agent; crossing them is out of scope)

This skill **only does API / parameter-surface reconnaissance**. It is not vulnerability hunting or exploitation.

### Task boundary

| Scope | Allowed | Forbidden |
|---|---|---|
| **Target** | Enumerate path, method, parameters, routes, and UI triggers | SQLi/XSS/authz bypass/brute force/fuzz for vulnerabilities, request tampering, destructive operations |
| **Auth** | Hook + stub/mock to bypass the **client** login gate | Ask the user for credentials or guess them; submit a real login form |
| **Runtime** | Hook APIs with no credentials and use mock responses so the SPA enters the post-login shell | Flows that can continue only with a real backend session |

### Dynamic analysis with no credentials (Phase 3 default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** login, permission, menu, and other bootstrap APIs;
2. Return a mock body for business query APIs that is **structurally correct, has a successful business code, and may have empty data**;
3. Let the frontend render the post-login page with no backend or under 401, so more XHR/fetch/WebSocket calls fire;
4. **Empty data, blank tables, and placeholder UI are expected** — do not switch to a real login or to vulnerability testing because of them.

**In one sentence**: use mocks to open frontend routes and mount components, and **record only outbound requests**. What the backend returns does not matter. What matters is **which further APIs the frontend will call**.

### Hard prohibitions in the flow

| Forbidden | Do this instead |
|---|---|
| Before Phase 1 finishes, grep/curl/Read the main entry `index-*.js` to extract API paths | Run `OUTDIR/harvest_static.py` |
| Hand-write `extract_apis.py` or another script in place of harvest | Edit `OUTDIR/harvest_static.py` and rerun it |
| Repeat the same grep/command after it has failed 2 or more times | Change strategy: read tool_logs, edit harvest, check reference |
| Skip gates A/B and run the original `scripts/` copies | Copy them to OUTDIR and adapt them to the target |
| Real username/password, OTP, OAuth, or other real auth | stub/mock (see above) |
| Skip the stub "to get real data" and do authz-bypass or injection testing | Record outbound only; that is the recon boundary |
| Irreversible actions such as delete, export of sensitive data, or bulk writes | The same applies to coverage clicks |
| Claim every page and API is known before runtime and dynamic enumeration are done | See "Definition of done", or state the limitation |
| Claim every parameter is known before the parameter-trigger matrix and diff are done | Phase 3b matrix + Phase 5 diff |
| Infer required vs optional from a single runtime sample | Multi-sample diff, or infer from validation rules / errors |

---

## Two-layer model + run modes

| Layer | Output | Ceiling |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, draft routes, callsite field candidates | No HTTP method; parameters need Phase 1b; runtime-concatenated URLs are missed |
| **Runtime** (live session) | Method + body + response + dynamic URL + WS/SSE; multi-sample diff fills in parameters | A page must actually render before it sends requests; one sample cannot decide required vs optional |

| Run mode | Engine | Use when |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response body, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Click tabs/dialogs/tables; deeper feature coverage |
| **both** | depth first, then coverage | Most complete, takes the longest |

**Parameter method** (no general-purpose script): paths come from harvest/regex; parameters come from **anchor window expansion + UI binding chain + multi-sample diff + error inference** (grep recipes are in [reference.md](reference.md) section J).

---

## Definition of done

All of the following must be true before recon can be called complete:

- [ ] **Static**: Phase 1 harvest produced `api_static.txt`, `routes.txt`, and `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both requires **hooks in effect + the dynamic enumeration loop**
- [ ] **Inside the shell**: visiting a business path does not land on `/login` (watch hash routes)
- [ ] **Parameters**: coverage/both finished the parameter-trigger matrix and `param_samples.json`; Phase 5 merged `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 rebuilt the permission tree and reran until **module-level APIs** appear (not only locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs are complete (see the Phase 5 output table); `insert_assets` wrote service and endpoint assets

---

## Scripts and gates

`scripts/` is reference templates only. **Do not** run the originals and treat that as the final result.

**Rule**: read first → adapt to the target → write into `OUTDIR` (for example `recon/`) → record `CHANGES.md`. If it does not match, rewrite from the method and keep only the structure.

| Gate | When | Reference script → OUTDIR copy | Typical required edits |
|---|---|---|---|
| **A (static)** | After Phase 0, before the **first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **Most sites can run the default regex as-is**; change the endpoint regex, webpack/Vite `publicPath`, and MPA exclude/cookie only when the manifest or dialect does not match |
| **B (runtime)** | After Phase 2, before depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success value, stubs, login regex, api prefix, hash/history |

**SPA order is mandatory** (do not swap steps; phase numbers outrank "explore first, then script"):

| Step | Required | Forbidden |
|---|---|---|
| After Phase 0 | The next Bash command is `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | Copy the script → small edits if needed → **run it immediately** | Extract APIs by hand first, then decide whether to harvest |
| Before Phase 1 is done | Check outputs with `wc -l`; on 404, edit harvest and retry | Hand-write an extract script; grep URLs that were not downloaded |
| From Phase 1b on | grep only `OUTDIR/js/*.js` | Use the main bundle instead of harvest |

- ✅ Copy `harvest_static.py` → (optional) edit the regex → **run it immediately**
- ❌ curl the main bundle → grep many times → write a temporary extract → harvest last
- **MPA**: after Phase 0, the next Bash command is `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and output constraints

| Constraint | Meaning |
|---|---|
| Large files | `index-*.js` over 100KB **must not** be Read/grep'd into context; batch them with OUTDIR scripts |
| grep output | Must use `\| head -20` or `-m 5`; keep only a path summary in the conversation, do not paste bundle fragments |
| Checks | Use `wc -l` and `ls \| wc -l`; do not Read a whole directory |
| Regex probe | Optional, at most once, and only a chunk ≤50KB or HTML; the real static pass is harvest |
| reference | Recipes, templates, and troubleshooting are in [reference.md](reference.md). Do not inline the whole file again |

---

## Execution roadmap

```
Phase 0 classify + OUTDIR
  → gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reversing
  → Phase 2 three auth gates → config.json
  → gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (when needed) → rerun Phase 3
  → Phase 5 merge the report + insert_assets bulk-inserts every discovered service and endpoint API asset. Insertion must not drop any asset that was already discovered
```

Check items in order. **Do not enter the next phase until the previous one is done.**

1. [ ] **Phase 0**: first look at SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0-classification)
2. [ ] **Gate A + Phase 1**: copy the script → harvest **immediately** → check with `wc -l` → [Phase 1](#phase-1-static)
3. [ ] **Phase 1b**: anchor window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b-parameter-reversing)
4. [ ] **Phase 2**: three auth gates → `config.json` → [Phase 2](#phase-2-three-auth-gates)
5. [ ] **Gate B**: adapt the runtime scripts → [Phase 3](#phase-3-runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm the shell was entered; parameter-trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4-permission-tree-reconstruction)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5-merge-and-report)

---

## Phase 0: Classification

Fetch the entry HTML and **create `OUTDIR`** (do not edit `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunk → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Outputs `forms.txt`, `links.txt`, and `api_inline.txt`. If an SPA has forms ≈ 0 → switch to Phase 1.

---

## Phase 1: Static

Follow [Scripts and gates](#scripts-and-gates) and [Tool and output constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML script tags → webpack/Vite manifest → download every lazy chunk → write `js/`, `api_static.txt`, `routes.txt`, and `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: on 404, edit harvest and retry. Do not curl chunks one by one
- too few lines in `api_static.txt` → loosen the endpoint regex inside OUTDIR and rerun (see reference)

### Phase 1b: Parameter reversing

Paths come from Phase 1. Parameter fields need their own recon. Grep rules are in [Tool and output constraints](#tool-and-output-constraints).

**Done when** important APIs can answer: field name, where it is sent, inferred type, required or not, sample value, and confidence.

#### 1b.0: Transport shape

| Shape | Where parameters are | Look at first in static |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` next to the path anchor |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Classic form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameter | `/user/:id` | route table + `useParams` / `$route.params` |
| Encrypted/signed | wrapped in `sign`/`data` | Hook the crypto function's arguments (reference section D) |

Output: mark each API `transport: query|json|form|graphql|encrypted`.

#### 1b.1: Anchor window expansion

Anchor on a known path and widen the window to find the object that builds the request:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper | Parameter clue |
|---|---|
| axios instance | `data` / `params` |
| shared request helper | interceptor injects global fields |
| OpenAPI client | generated method signature |
| React Query / SWR | second argument of the hook |
| Vue composable | composable arguments |

Type leftovers: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2: Binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Method |
|---|---|
| Form submit | follow submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

In the DevTools call stack, walk up from `fetch`/`XHR.send` to the function that builds the request.

#### 1b.3: Three questions about request building (not the Phase 2 auth gates)

| Question | What to answer |
|---|---|
| **Assembly** | where the payload is built, and transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

The interceptor gate (Phase 2) also reads globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4: Handoff to Phase 3

Candidate fields come from the static and binding layers. **Required / optional / conditional dependencies** need the Phase 3 parameter matrix + diff + Phase 5 error inference.

---

## Phase 2: Three auth gates

grep inside `OUTDIR/js/` (with `head`) and write `config.json` (recipes are in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How does it decide the user is logged in? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers a jump to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do the menu and permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat a localStorage key name as a credential — confirm it from the chunk and the request chain.

**Exit = gate B**: the conclusion lands in `config.json`, and `OUTDIR/runtime_harvest.js` / `preload.js` are edited to match.

### Phase 2b: API observation (optional)

Use `preload.js` inside OUTDIR to confirm the session key name, Authorization, and nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | header observation |
| `extractUrlsFromResponse: true` | child APIs inside responses |
| `observe.storageReads/cookieReads: true` | write back into config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3: Runtime

Gate B must already be passed. Follow [Boundaries and prohibitions](#boundaries-and-prohibitions-required-reading-for-the-agent-crossing-them-is-out-of-scope) and the no-credential mock policy.

Set `"runtimeMode": "depth" | "coverage" | "both"` in `config.json` (template in reference).

### Hooks and stubs (shared by depth and coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | auth/permission/bootstrap stub | pass the first-screen auth check |
| L2 negative rewrite | every JSON response | not-logged-in code → success |
| L3 fallback | `/api` and similar that missed L1 | empty success body, so the UI can open |

- **depth**: fake auth + `forward` rewrites the business code + `stubs`; walk `routes` (hash/history); output `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or a userscript)

Check: `window.__API_RECON_PRELOAD__` exists; a business path does not bounce to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b: coverage dynamic enumeration (required)

1. Main nav / sidebar — click each item, wait 1–3s for the network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — view/edit/detail on the first row
4. Toolbar — export, filter, create (**avoid irreversible deletes**)
5. Each time a module is entered — merge APIs/routes
6. SPA — controlled `pushState` for paths in `routes.txt` that were not covered (forbidden for MPA)

**Parameter-trigger matrix** (required): record each module once per action type, and **diff the samples**:

| Action | Parameters that usually appear |
|---|---|
| List first screen | pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | more optional fields |
| Create/edit | the full entity |
| Bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Under a stub, the outbound body and headers are still real** — trust the request. Recording → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4: Permission-tree reconstruction

**Trigger**: module pages are blank / every route only hits bootstrap (for example locale) → the content gate was not passed.

| Symptom | Meaning |
|---|---|
| Entered the shell | render gate + interceptor gate passed |
| Sidebar items missing / click is blank | stub shape is wrong or permission codes are incomplete |
| Every route hits the same tiny set of APIs | `v-if permission` did not pass |
| `routes.txt` is far smaller than the bundle | fill it in from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate output: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub checks: the outer `response_code` matches the interceptor gate; flat codes line up with the tree; `routes` covers every link in `route_map`.

After updating `config.json`, **rerun Phase 3**. A large SPA can tune `waitUntil`, `routeTimeout`, and `perRouteMs` (see reference A3 / section I).

---

## Phase 5: Merge and report

### Output table

| File | Phase | Contents |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | static bundle and paths |
| `param_candidates.json` | 1b | static parameter-field candidates |
| `config.json` | 2 | three gates + runtime config |
| `runtime_api.json` | 3a | depth recording in detail (including WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | multiple samples, click log, detail |
| `route_map.json` and related files | 4 | permission-tree intermediates (if Phase 4 ran) |
| `params_merged.json` | 5 | merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | routes, APIs, params, features, limitations |
| **insert_assets** | 5 | write every service and endpoint asset into the asset library |

### 5b: Merge parameters

diff `param_samples.json`. **There is no general merge script.** Confidence rules are in reference J7 (high/medium/low/not-yet-triggered).

### 5c: Infer from errors

Within the authorized scope, send an incomplete request and read the 400 (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, and similar. Watch the `data` wrapper, `variables`, and pre-encryption `bizData`.

The report must state: runtimeMode, static vs runtime API counts, parameter confidence, uncovered modules, and a `CHANGES.md` summary of edits relative to the reference scripts.

Suggested `site_map.json` shape:

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

## General notes

- **Framework-agnostic**: webpack/Vite/Angular lazy load use the same method
- **Transports**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client fetch can be recorded; RSC/Server Actions cannot be fully enumerated
- **Blind spots**: JSVMP, WASM, HMAC/mTLS with strong checks → static plus a stated limitation
- **Parameter blind spots**: conditional linkage, hidden params, WASM request building → "not yet triggered" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Further material

- Grep recipes, the `config.json` template, troubleshooting, hooks, parameter-reversing section J, and the site_map template: **[reference.md](reference.md)**
- Reference script paths are in the [Scripts and gates](#scripts-and-gates) table
