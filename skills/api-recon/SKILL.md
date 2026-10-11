---
name: api-recon
description: Invoke this skill when collecting a website's API endpoints.
---

# API Recon (Frontend API Reconnaissance)

Under an **authorized** premise, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature trigger points** (tabs, dialogs, table actions, etc.).

---

## Boundaries and Prohibitions (Agent MUST read · violating this is going out of bounds)

This skill **only performs API / parameter-surface reconnaissance**; it is not a vulnerability-hunting or exploitation phase.

### Task boundary

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate paths, methods, parameters, routes, UI trigger points | SQLi/XSS/privilege-escalation/brute-force/fuzz vulnerabilities, packet-tampering attacks, destructive operations |
| **Authentication** | Hook + stub/mock to bypass the **client-side** login gate | Ask the user for or guess account credentials; attempt a real login-form submission |
| **Runtime** | Hook endpoints without credentials, use mock responses to drive the SPA into the post-login shell | Flows that require a real backend session to continue |

### Credential-less dynamic analysis (Phase 3 default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap endpoints such as login, permissions, and menus;
2. Return mock bodies for business query endpoints that are **structurally correct, with a success business code, and may carry empty data**;
3. Let the frontend still render post-login pages with no backend or a 401 environment, thereby triggering more XHR/fetch/WebSocket;
4. **Empty data, blank tables, and placeholder UI are all expected** — do not switch to real login or vulnerability testing because of them.

**In one sentence**: use mocks to prop open the frontend routes and component mounting, and **only record outbound requests**; what the backend returns does not matter — what matters is **which other endpoints the frontend still sends**.

### Hard flow prohibitions

| Prohibited | Alternative |
|---|---|
| grep/curl/Read the main entry `index-*.js` to extract API paths before Phase 1 is complete | Run `OUTDIR/harvest_static.py` |
| Hand-write scripts like `extract_apis.py` that replace harvest | Edit `OUTDIR/harvest_static.py` and rerun |
| Repeating the same grep/command after it has failed ≥2 times | Change strategy: read tool_logs, edit harvest, check reference |
| Skip gates A/B and directly run the original `scripts/` | Copy to OUTDIR and adapt to the target |
| Real usernames/passwords, OTP, OAuth, and other authentication | stub/mock (see above) |
| Skipping the stub on the pretext of "getting real data" to do privilege-escalation/injection testing | Only record outbound; that is the recon boundary |
| Irreversible operations such as deleting, exporting sensitive data, bulk writes | The same applies to coverage clicks |
| Claiming all pages and endpoints are obtained without completing runtime + dynamic enumeration | See "Definition of done" or note the limitation |
| Claiming all parameters are known without completing the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff, or reverse-infer from validation rules/errors |

---

## Two-layer model + run modes

| Layer | Output | Upper bound |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, route draft, request-assembly field candidates | No HTTP method; parameters require Phase 1b; misses runtime-concatenated URLs |
| **Runtime** (live session) | Method + body + response + dynamic URL + WS/SSE; multi-sample diff to complete parameters | The page must actually render to send requests; a single sample is insufficient to decide required/optional |

| Run mode | Engine | Fits |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response body, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Clicking tabs/dialogs/tables, deeper feature-point coverage |
| **both** | depth first, then coverage | Most complete, longest-running |

**Parameter methodology** (no general-purpose script): paths via harvest/regex; parameters via **anchor window expansion + UI binding chain + multi-sample diff + reverse-inference from errors** (grep recipes in section J of [reference.md](reference.md)).

---

## Definition of done

All of the following must be satisfied before claiming recon is complete:

- [ ] **Static**: Phase 1 harvest produces `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both require **Hook in effect + dynamic enumeration loop**
- [ ] **Entered the shell**: visiting a business path does not return `/login` (watch for hash routing)
- [ ] **Parameters**: coverage/both complete the parameter trigger matrix + `param_samples.json`; Phase 5 merges into `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 restores the permission tree and reruns, until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs complete (see the Phase 5 output table); `insert_assets` writes the service and endpoint assets

---

## Scripts and gates

`scripts/` are reference templates only; **do not** run the originals directly and treat them as the final result.

**Rule**: read first → adapt to the target → write into `OUTDIR` (e.g. `recon/`) → record `CHANGES.md`; if it does not match, rewrite per the methodology, borrowing only the structure.

| Gate | When | Reference script → OUTDIR copy | Common required changes |
|---|---|---|---|
| **A (static)** | After Phase 0, before running harvest/spider for the **first time** | `harvest_static.py` / `spider_mpa.py` | **The default regex runs directly for most sites**; only when a manifest/dialect does not match, change the endpoint regex, webpack/Vite `publicPath`, MPA exclude/cookie |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success value, stubs, login regex, api prefix, hash/history |

**SPA mandatory order** (not interchangeable; the Phase numbering takes precedence over "explore first, then script"):

| Step | Must | Prohibited |
|---|---|---|
| After Phase 0 completes | Next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | Copy the script → make small changes as needed → **run immediately** | Manually extract APIs first, then decide whether to harvest |
| Before Phase 1 completes | Verify output with `wc -l`; on 404, edit harvest and retry | Hand-write an extract script; repeatedly grep an undownloaded URL |
| From Phase 1b | grep only `OUTDIR/js/*.js` | Use the main bundle instead of harvest |

- ✅ Copy `harvest_static.py` → (optionally) edit the regex → **run immediately**
- ❌ curl the main bundle → grep repeatedly → write a temporary extract → harvest only at the end
- **MPA**: after Phase 0, next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and output constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` >100KB **must not** be Read/grep'd into context; batch-process with OUTDIR scripts |
| grep output | Must use `\| head -20` or `-m 5`; keep only a path summary in the conversation, do not paste bundle fragments |
| Verification | Use `wc -l`, `ls \| wc -l`; do not Read a whole directory |
| regex first look | Optional, ≤1 time, only ≤50KB small chunk or HTML; the authoritative static source is harvest |
| reference | Recipes/templates/troubleshooting in [reference.md](reference.md); do not repeat the full text inline |

---

## Execution roadmap

```
Phase 0 classify + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reverse-engineering
  → Phase 2 three auth gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (when needed) → rerun Phase 3
  → Phase 5 merged report + insert_assets to bulk-insert all discovered service and endpoint API assets; under no circumstances omit an already-discovered asset when inserting
```

Check off in order; **you may not enter the next Phase before the previous item is complete**.

1. [ ] **Phase 0**: first look at SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classify)
2. [ ] **Gate A + Phase 1**: copy the script → harvest **immediately** → verify with `wc -l` → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: three auth gates → `config.json` → [Phase 2](#phase-2--three-auth-gates)
5. [ ] **Gate B**: adjust the runtime script → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm entry into the shell; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-restoration)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — classify

Fetch the entry HTML and **create `OUTDIR`** (do not edit `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produces `forms.txt`, `links.txt`, `api_inline.txt`. For an SPA with forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — static

Follow [Scripts and gates](#scripts-and-gates) · [Tool and output constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML script → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: on 404, edit harvest and retry; do not manually curl chunks one by one
- `api_static.txt` too sparse → relax the endpoint regex in OUTDIR and rerun (see reference)

### Phase 1b — parameter reverse-engineering

Paths come from Phase 1; parameter fields must be recon'd separately. See [Tool and output constraints](#tool-and-output-constraints) for grep rules.

**Completion criteria**: for important endpoints you can answer — field name, transport location, inferred type, whether required, sample value, confidence.

#### 1b.0 — transport form

| Form | Where parameters are | Static priority |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` next to the path anchor |
| GraphQL | `variables` | gql template, `$page: Int` |
| Classic form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameter | `/user/:id` | route table + `useParams` / `$route.params` |
| Encrypted/signed | wrapped into `sign`/`data` | Hook the encryption function's input (reference section D) |

Output: annotate each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — anchor window expansion

Using a known path as an anchor, expand the window to find the assembly object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapping layer | Parameter clue |
|---|---|
| axios instance | `data` / `params` |
| unified request | interceptor injects global fields |
| OpenAPI client | generated method signature |
| React Query / SWR | hook's second argument |
| Vue composable | composable arguments |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | follow submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

From DevTools call stack, trace upward from `fetch`/`XHR.send` to the assembly function.

#### 1b.3 — three assembly questions (≠ the Phase 2 auth gates)

| Question | What to answer |
|---|---|
| **Assembly** | where the payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / header |

The interceptor gate (Phase 2) incidentally reads the globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — handoff to Phase 3

Candidate fields come from the static/binding layer; **required/optional/conditional dependency** require the Phase 3 parameter matrix + diff + Phase 5 reverse-inference from errors.

---

## Phase 2 — three auth gates

grep in `OUTDIR/js/` (with `head`), and write `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How is "logged in" determined? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers the jump to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat a localStorage key name as a credential — confirm it from the chunk/request chain.

**Exit = Gate B**: land the conclusions in `config.json`, and edit `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API observation (optional)

Use `preload.js` in OUTDIR to confirm the session key names, Authorization, and nested API URLs:

| Setting | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | header observation |
| `extractUrlsFromResponse: true` | sub-APIs inside responses |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — runtime

Gate B must already be passed; follow [Boundaries and Prohibitions](#boundaries-and-prohibitions-agent-must-read--violating-this-is-going-out-of-bounds) · the credential-less mock strategy.

In `config.json` set `"runtimeMode": "depth" | "coverage" | "both"` (templates in reference).

### Hook and stub (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | auth/permission/bootstrap stub | pass the first-screen authentication |
| L2 negative correction | all JSON responses | not-logged-in code → success |
| L3 fallback | `/api` etc. not matched by L1 | empty success body, prop open the UI |

- **depth**: fake auth + `forward` rewriting the business code + `stubs`; traverse `routes` (hash/history); produces `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or Userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; a business path does not return `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage dynamic enumeration (required)

1. Main navigation/sidebar — click every item, wait 1–3s for the network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — first-row view/edit/detail
4. Toolbar — export, filter, create (**avoid irreversible deletes**)
5. On entering each module — merge APIs/routes
6. SPA — controlled `pushState` for paths not covered in `routes.txt` (prohibited for MPA)

**Parameter trigger matrix** (required): record once per operation type for each module, and **diff multiple samples**:

| Operation | Parameters usually added |
|---|---|
| List first screen | pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | more optionals |
| Create/edit | full entity |
| Bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Under a stub, the outbound body/headers are still real** — go by the request. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — permission tree restoration

**Trigger**: module pages are blank / each route returns only bootstrap (e.g. locale) → the content gate has not passed.

| Symptom | Meaning |
|---|---|
| Entered the shell successfully | render gate + interceptor gate already passed |
| Sidebar missing items / clicks go blank | stub shape or permission codes incomplete |
| Every route has the same, very few APIs | `v-if permission` not passing |
| `routes.txt` far fewer than the bundle | must be completed from the auth module |

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

stub check: the outer `response_code` matches the interceptor gate; flat codes align with the tree; `routes` covers all links in `route_map`.

After updating `config.json`, **rerun Phase 3**. For large SPAs you can tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — merge and report

### Output table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | static bundle and paths |
| `param_candidates.json` | 1b | static parameter field candidates |
| `config.json` | 2 | three gates + runtime config |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | multiple samples, click log, detail |
| `route_map.json` etc. | 4 | permission tree intermediate files (if executed) |
| `params_merged.json` | 5 | merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | write all service and endpoint assets into the asset store |

### 5b — parameter merging

Diff from `param_samples.json`; **no general-purpose merge script**. See reference J7 for confidence rules (high/medium/low/pending-trigger).

### 5c — reverse-inference from errors

Within the authorized scope you may send incomplete requests to read 400s (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Watch for the `data` wrapper, `variables`, and `bizData` before encryption.

The report must note: runtimeMode, the static/runtime API counts, parameter confidence, uncovered modules, and a `CHANGES.md` summary relative to the reference scripts.

Suggested structure for `site_map.json`:

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

See [reference.md](reference.md) for more fields and grep recipes.

---

## General notes

- **Framework-agnostic**: webpack/Vite/Angular lazy load use the same method
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions are not fully enumerable
- **Blind spots**: JSVMP, WASM, strong HMAC/mTLS validation → static + note the limitation
- **Parameter blind spots**: conditional linkage, hidden params, WASM assembly → "pending-trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional resources

- Grep recipes, `config.json` template, troubleshooting, Hook, parameter reverse-engineering section J, site_map template: **[reference.md](reference.md)**
- Reference script paths: see the [Scripts and gates](#scripts-and-gates) table
