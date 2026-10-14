# api-recon — reference

Grep recipes, the `config.json` template, and troubleshooting. Every grep runs against the `js/` directory. If a bundle is one line, `js-beautify` or `sed 's/}/}\n/g'` first. A raw grep with a context window is usually enough.

## Scripts

Every file under `scripts/` is a **reference template**. It must be adapted to the target site before it is run. Typical edits:

| Script | Often needs |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success value, stub match rules and body shape, where routes come from, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether L3 is on, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` for destructive links, cookie, depth/max, same-origin filter |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, outer stub field names |
| `config.json` | the single place for every site-specific parameter above |

Put the edited files in the task working directory (for example `recon/`) and note the concrete diffs from the reference scripts in the report.

---

## A. Reversing the three gates

### A1. Render gate — "How does it decide the user is logged in?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Find the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))`, and determine the **storage key**, the **container** (Cookie vs localStorage), and the **encoding**:

| Encoding | How to forge it in config |
|---|---|
| Plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | an unsigned / `alg:none` JWT, or one signed with a key hardcoded in the bundle |
| Encrypted (SM2/AES/RSA) | find the hardcoded key; the render gate can be forged when it only needs a decodable blob; otherwise fall back to static |

→ write `cookies` / `localStorage`.

### A2. Interceptor gate — "What triggers a jump to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(not logged in|please log in again|login expired|unauthorized|session expired|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: the **field name**, the **success value** (usually `0` or `200`), and the **failure value that triggers the redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ write `neutralize.fields` + `neutralize.success`.

### A3. Content gate — "Where do the menu and permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise admin apps):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | sidebar menu rendering |
| `userRouteAuth` in the bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in the bundle | `{ KEY: { name, link } }` | alias resolution (webpack `o.DASHBOARD`) |

Read the consumer to confirm how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Forge by hand** (small sites): build a permissive payload → `stubs`.

**Full permission-tree reconstruction** (large sites, sidebar or submodules still blank): see **section I**.

---

## B. config.json template

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
- `cookies[].value` prefixes: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal
- `forward: true` forwards the real request and rewrites the code field; `false` is a fully offline stub
- `mockTier`: which layers coverage-mode preload enables, for example `L1+L2`, `L1+L2+L3`
- `routes` come from `routes.txt`; after the menu is forged, the harness appends `<a href>` automatically
- `captureResponses` / `recordWs` apply only in depth mode
- `waitUntil`: large SPAs should use `domcontentloaded` so `networkidle2` does not hang
- `routeTimeout`: timeout in milliseconds for one route's `page.goto`
- `proxy`: Puppeteer `--proxy-server`; `HTTP_PROXY` / `HTTPS_PROXY` also work

### B1. Dual-stub template (role_permissions + permissions/all)

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

The outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate. `permissions` must cover every leaf code in the tree.

---

## C. coverage mode: preload config

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before the CDP injection:

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
  stubs: [ /* same stubs as config.json */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Check: `window.__API_RECON_PRELOAD__ === true` and the pathname stays put.

Export recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime hook coverage

Browser hooks built into preload (coverage) and runtime_harvest (depth), and what they cover:

| Hook | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | find Authorization and other headers | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | confirm the session key name | ⚠️ optional `observe.storageReads/cookieReads` |
| Vue route collection | fill in frontendRoutes | ✅ `__API_RECON_ROUTES__` (routes already loaded) |
| Neutralize Vue route guards / block login redirects | open modules so they fire APIs | ✅ `neutralizeVueRouter` + native redirect neutralization |
| React route collection | fill in routes | ⚠️ static + clicks; no dedicated hook |
| Block page navigation (login path) | stay on the page to analyze it | ⚠️ blocks only the login path, so business navigation is not blocked |
| Hook a crypto library (CryptoJS/SM and similar) | encrypted parameters → plaintext API body | ❌ hook the crypto function's arguments by hand; write the conclusion into config |
| Anti-debug bypass | otherwise runtime records no APIs | ❌ handle it by hand; static still works |

---

## E. Endpoint extraction regex (when static finds too little)

Loosen `extract_endpoints` in `harvest_static.py`, or do it by hand:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → what to do |
|---|---|
| Very few static APIs | endpoint dialect does not match → loosen the regex (section D) |
| chunk count ≪ manifest | CSS-only or chunks that were not deployed; 404s were already retried |
| runtime still shows the login page | render gate is wrong → recheck A1: key name, container, encoding, domain |
| Inside the shell but modules are blank | content gate → forge the menu (A3); `routes` paths may be wrong |
| Every route only hits bootstrap/locale | permission codes are incomplete → section I permission-tree reconstruction; check the `role_permissions` + `permissions/all` dual stub |
| Sidebar has items but child pages are blank | the tree is missing intermediate nodes, or codes do not match `userRouteAuth` |
| Every API redirects to login | interceptor gate → confirm `neutralize`; nested fields need the walk logic extended |
| WS frame count is 0 | subscribe happens only after user interaction; increase `perRouteMs` |
| Response body is empty | a real response exists only when `forward: true` |
| Chromium missing | install chromium or set `config.chromium` / `CHROMIUM` |
| Lots of mocks and it still returns to login | the hook is too late, or the `location.href` setter is missing → document-start + preload |
| Lists are all empty | an empty array from L3 is normal; keep clicking tabs/settings/detail |
| Redux actions mistaken for routes | filter internal paths that contain get/set/change/clear/toggle/upload |
| Vue still jumps to login | preload was not document-start → change the injection time; or clear guards by hand when `neutralizeVueRouter: false` |
| URLs in the response never enter the log | turn on `extractUrlsFromResponse`; or extract them by hand from `__API_RECON_DETAIL__` |
| Authorization header name is unknown | turn on `observe.xhrHeaders`, or look at request headers in DevTools |
| runtime is very slow / times out | set `waitUntil: domcontentloaded`; lower `routeTimeout`; do not use `networkidle2` |
| Proxy connection failed | check `proxy` / environment variables; Puppeteer and curl must use the same proxy port |

---

## G. Hardened targets

When the server checks the session step by step (a signed cookie that cannot be forged, or a server-rendered menu that cannot be stubbed), runtime gets stuck at the shell. Expected behavior:

- **Static is enough to enumerate endpoints** — module paths are in the code
- If authorization allows it, run the same harness with a **real session**: `forward: true`, no neutralize, and capture the real methods/params/responses

---

## H. Checklist for one task

1. Confirm the authorized scope
2. **Read** `scripts/harvest_static.py` → adapt it to the target → run it → review `api_static.txt` and `routes.txt`
3. **Phase 1b**: path-anchor window expansion + binding layer → `param_candidates.json` (section J)
4. Reverse A1/A2/A3 → write a site-specific `config.json`
5. **Read and adapt** `runtime_harvest.js` / `preload.js` before running them
6. `runtimeMode=depth`: `npm install` → run the adapted harvest script
7. `runtimeMode=coverage/both`: inject the adapted preload at document-start → browser MCP dynamic enumeration + **parameter-trigger matrix**
8. Modules do not render → **section I permission-tree reconstruction** → patch stubs → rerun
9. Multi-sample parameter diff + error inference → `params_merged.json`
10. Merge → `site_map.json` + `api_merged.txt`, and honestly mark coverage, gaps, and script edits

---

## I. Permission-tree reconstruction (Phase 4 in depth)

Use this when forging a simple `menus: [{ path, show: true }]` does nothing and submodules still do not mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, the **response field names**, and the **consumer chunk filename**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# output: recon/route_map.json
```

If `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or grep by hand:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the permission tree + stubs

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

What the script does:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases such as `He=o.DASHBOARD`)
2. Resolve alias → real path with `route_map.json`
3. Infer the parent from the code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Write `permissions_tree.json`, `permissions_all_stub.json`, and `role_permissions_stub.json`
5. With `--config`, write `stubs` into `config.json` and extend `routes`

**Adapt to the target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent map
- `DEFAULT_EXTRA_PARENT`: orphan nodes that are not a prefix relationship

### I4. Check stub consistency

```bash
# permission count should be ≈ the number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover every link in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare runtime_api.json counts before and after the forge; check whether module APIs such as /attack and /asset appear
```

| Before the forge | After a successful forge |
|---|---|
| the same 3–5 bootstrap calls on every route | different routes fire different module APIs |
| only `/api/locale/language` | module endpoints such as `/api/web/...` appear |
| a handful of routes in `routes.txt` | 80–110+ `routes` from route_map |

### I6. If it still fails

- **coverage mode**: click the sidebar and tabs; permission gating may request data only after interaction
- **stub fields**: compare nesting of the real API (curl + a real session) with the stub
- **extra guards**: grep `hasPermission|checkRole|func.` and other button-level checks, and extend `role_permissions.permissions`
- **static fallback**: module API paths are still in `api_static.txt`; runtime only adds METHOD/body. Keep parameters in `param_candidates.json` plus the samples already recorded

---

## J. Parameter reversing (Phase 1b / 5b / 5c)

**A method, not a general-purpose script.** Find paths with regex. Find parameters with anchor window expansion + the UI binding chain + multi-sample diff + error inference.

### J1. Anchor window expansion — from a path to the object that builds the request

```bash
# anchor on a path known from Phase 1
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrappers and transport shape

```bash
# axios / shared request helper
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

Runtime fill-in: DevTools → Network → the request → **initiator** (call stack). Walk up from `fetch`/`send` to the function that builds the request.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Do not guess fields on ciphertext** — hook the crypto function's **arguments** and record the plaintext payload before encryption. Write the conclusion into `config.json` / `param_candidates.json`.

### J6. Parameter-trigger matrix (required in Phase 3)

Record each module once per action, and diff the request body/query:

| Action | Watch for |
|---|---|
| List first screen | pagination defaults |
| Search | keyword, filters |
| Advanced filter | optional fields |
| Create/edit | the full entity |
| Bulk/export | `ids[]`, `exportType` |
| Sort/page | `sortField`, `order` |

Output `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **High** | static callsite + at least 2 runtime samples agree |
| **Medium** | static only, or only 1 runtime sample |
| **Low** | inferred from a response or an error, not verified a second time |
| **Not yet triggered** | the field is known from static, but the UI or permission path was never reached |

### J8. Quick setup by scenario

| Scenario | Order |
|---|---|
| REST list page | J1 request-building object → J6 four diffs → J3 rules |
| Create/edit form | J3 form `name` → J4 submit chain → runtime submit, and leave a field empty on purpose to see the 400 |
| GraphQL | J2 `variables` declarations → record `variables` for each operation at runtime |
| Encrypted body | J5 hook the arguments → the fields before encryption are the real params |

### J9. Mapping onto api-recon phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 static | J1 anchor window expansion |
| Phase 2 A2 interceptor | globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | different modules have different forms → every field fires only when permissions are enough |
| Phase 5 merge | `params_merged.json` + confidence; do not decide required from one sample |

### J10. Troubleshooting

| Symptom | What to do |
|---|---|
| A field name exists in static and never appears at runtime | mark it "not yet triggered"; finish the permission tree / click advanced filters / each option of a linked select |
| The same path has different body shapes | normal — record one row per `action`; do not force one schema |
| The stub response is fake and you still want the params | **look at the outbound request** body/headers; do not infer params from the stub response |
| A 400 reports a nested field | watch the outer wrapper `data`/`bizData`/`variables` |
| GraphQL only shows the operation name | expand the `variables` JSON; in static, look for `$var: Type` |

---
