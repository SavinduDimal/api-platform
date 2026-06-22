# SOAP API Support — Implementation Walkthrough (Steps 1–5)

> ⚠️ **DESIGN-REVIEW UPDATE.** After Step 5, a review **removed operation awareness**: the
> `soap-dispatch` policy and all `operations`/`soapAction`/`bodyElement` handling were
> deleted; a SOAP API is now a single **wildcard** POST (+GET) resource. The Step 3
> soap-dispatch section and the Step 4–5 operation/per-op sections below describe the
> **removed** model. Still current: the SOAP Fault formatter (Step 3) and the new
> `soap-wsdl-rewrite` response-body policy (rewrites backend URLs→gateway URL in WSDL,
> auto-attached to the GET route, toggled by `spec.rewriteWsdl`). Pending a full rewrite in
> Step 6.

This document explains and justifies every code change made for Phase 1 SOAP support.
It is organized by implementation step; each change states **what** was done and **why
that exact change was necessary** — including the architectural facts that forced it.

Companion documents: [Design](soap-api-design.md) · [Plan](soap-api-support-plan.md) ·
[Testing plan](soap-api-testing-plan.md)

---

## 0. Architecture primer (read first — every change traces back to this)

```
deploy (YAML) ─► gateway-controller
                  ├── parse (kind switch) ───────────── pkg/utils/api_deployment.go
                  ├── validate (type switch) ─────────── pkg/config/api_validator.go
                  ├── render templates (reflection) ──── pkg/templateengine (kind-agnostic)
                  ├── persist (kind → per-kind table) ── pkg/storage/sql_store.go
                  ├── ENVOY xDS: legacy translator ───── pkg/xds/translator.go
                  │     routes named METHOD|PATH|VHOST   (transformer registry NOT wired here)
                  └── POLICY xDS: transformer registry ─ pkg/transform/* → RuntimeDeployConfig
                        policy chains keyed BY ROUTE NAME → policy-engine (ext_proc lookup)
```

Three load-bearing facts:

1. **`route_name` is the join key between the two planes.** Envoy passes the matched
   route's name to the policy engine (`xds.route_name`); the engine looks up its policy
   chain by that string. Any route discrimination on the Envoy side must produce the
   identical key on the policy side.
2. **The Envoy translator and the policy transformer are separate code paths.** The
   Envoy route translator's `SetTransformers` is never called in `main.go` — REST Envoy
   routes come from the legacy `translateAPIConfig`; the transformer registry feeds only
   the policy xDS. *Therefore SOAP needs both a legacy translation function and a
   transformer* (Step 2).
3. **The policy engine learns the API kind from the policy xDS**, not from Envoy route
   metadata: transformer `rdc.Metadata.Kind` → RouteConfig resource → engine
   `RouteMetadata.APIKind` → `SharedContext.APIKind`. This is what the SOAP fault
   formatter keys on (Step 3) — and why the transformer matters even before policies are
   used.

---

## Step 1 — Data model, validation, CRUD, storage

**Goal:** `kind: SoapApi` is a first-class artifact: create/list/get/update/delete +
API keys via the management API; persisted; validated. No routing yet.

### 1.1 OpenAPI spec + generated types

**`gateway-controller/api/management-openapi.yaml`** — added `/soap-apis`,
`/soap-apis/{id}`, and the API-key sub-resources, plus schemas `SoapAPIRequest`,
`SoapAPI` (= request + server-managed `status`), `SoapAPIData`, `SoapOperation`.
Regenerated `pkg/api/management/generated.go` via `make generate` (oapi-codegen).

*Why a separate spec type rather than reusing `APIConfigData`:* REST operations are
`method` + `path`; SOAP operations are `name` + `soapAction` — structurally different.
`SoapAPIData` reuses the shared `Upstream` and `Policy` schemas so upstream resolution
and policy validation code paths are common.

*Why `soapVersion`, `operations[].soapAction`:* `soapVersion` declares the dialect
(informational; the runtime infers fault dialect per request — see 3.2). `soapAction`
is the per-operation routing discriminator (Step 5). An **empty `soapAction` is valid**
(doc/literal convention); the validator therefore rejects duplicates only among
*non-empty* actions.

### 1.2 Kind constant and model accessors

**`pkg/models/stored_config.go`** — `KindSoapApi ArtifactKind = "SoapApi"`, plus
`api.SoapAPI` cases in `GetContext()`, `GetPolicies()`, `GetMetadata()`, `GetLabels()`,
`GetAnnotations()`.

*Why:* `StoredConfig.Configuration` is `any`; every accessor is a type switch. Without
these cases, context resolution (used in conflict checks/search), policy retrieval, and
project-ID extraction (annotations → analytics) silently return zero values for SOAP.

### 1.3 Use the full `api.SoapAPI` type everywhere — not `SoapAPIRequest`

Initial implementation used `SoapAPIRequest` and was switched to `SoapAPI`.

*Why:* REST consistently stores/parses the full `api.RestAPI` (which carries `Status`).
`buildResourceResponse` (`pkg/api/handlers/resource_response.go`) injects the
server-managed `status` block by type-switching on the *full* types; a `SoapAPIRequest`
would fall through and responses would lack `status`. Parsing a request body into the
full type is safe — `status` is simply absent. One type everywhere also keeps the
storage round-trip (1.6) and validator switches single-typed.

### 1.4 Validation

**`pkg/config/api_validator.go`** — `Validate()` cases for `api.SoapAPI`/`*api.SoapAPI`;
`validateSoapAPIConfiguration` (kind, apiVersion), `validateSoapData` (displayName rules,
semver, context via the shared `validateContext`, upstream via the shared
`validateUpstream`), `validateSoapOperations` (operation name required + unique;
duplicate **non-empty** `soapAction` rejected — each generates a distinct route, two
identical matchers would shadow).
**`pkg/config/policy_validator.go`** — `ValidateSoapAPIPolicies` validates API-level and
operation-level policy references/params against loaded policy definitions, mirroring
`ValidateRestAPIPolicies`.

### 1.5 Deploy pipeline branches

**`pkg/utils/api_deployment.go`** — `case "SoapApi":` in the parse switch (parses into
`api.SoapAPI`, extracts handle/kind/artifact-id annotation) and in the
validate-and-extract switch (displayName/version + validator invocation).

*Why two switches:* the pipeline parses *before* template rendering and validates the
*rendered* config; both branch on the concrete type. `resolveVhostSentinels` and
`RenderSpec` needed **no** SOAP cases: the former safely no-ops on unknown types; the
latter is reflection-based (marshals to a map, renders spec strings, unmarshals back
into the same concrete type) and is kind-agnostic.

### 1.6 Storage — the per-kind table pattern

**`pkg/storage/gateway-controller-db.sql` / `...postgres.sql`** — `soap_apis` table
(uuid, gateway_id, configuration JSON; FK to `artifacts`). Schemas are
`CREATE TABLE IF NOT EXISTS` and applied at startup via `go:embed`, so no migration
tooling is needed.
**`pkg/storage/sql_store.go`** — `kindToResourceTable`: `"SoapApi" → "soap_apis"`;
`unmarshalSourceConfig`: `case "SoapApi"` unmarshals the stored JSON into `api.SoapAPI`
and populates both `SourceConfiguration` and `Configuration`.

*Why:* the store writes each artifact's source configuration into a kind-specific table
(generic `json.Marshal` + table name from `kindToResourceTable`) and rebuilds typed
configs on read. A missing entry fails the very first deploy with
`unknown kind: SoapApi` — this was hit in testing and is the canonical symptom of a
missing storage registration.

### 1.7 HTTP handlers

**`pkg/api/handlers/soap_api_handler.go`** (new) — `Create/List/GetById/Update/Delete
SoapAPI` + the five API-key operations, implementing the oapi-codegen `ServerInterface`
methods; modeled line-for-line on the WebSub handler (read body → `DeployAPIConfiguration`
with `Kind: "SoapApi"` → respond via `buildResourceResponseFromStored`). Delete removes
the config + API keys and publishes the API event for xDS propagation.

`mapSoapAPIDeployError` (added after user testing): maps conflict → 409, render errors,
and — crucially — unwraps `ValidationErrorListError` into the response's per-field
`errors` array. *Why:* the WebSub template returned only `err.Error()`
("configuration validation failed with 1 errors") which hides *what* failed; REST's
handler exposes field-level details. A displayName-regex failure during testing was
undiagnosable until this was added.

**`pkg/utils/helpers.go`** (`ExtractNameVersion`) and **`pkg/utils/api_key.go`**
(`extractConfigDisplayNameVersion`) — `api.SoapAPI` cases; the API-key service derives
display name/version from the typed config.

### 1.8 Authorization role map — a hidden, mandatory touchpoint

**`cmd/controller/main.go`** — `/soap-apis*` entries in the hardcoded `relativeRoles`
map (`admin, developer` for CRUD; `admin, consumer` for API keys).

*Why:* the management-API authorization middleware
(`common/authenticators/authz.go`) **rejects any route whose `METHOD path` key is absent
from this map with 403** — it is *not* generated from the spec's `x-basicauth-roles`
extension (that extension is decorative). Symptom when missed: every request returns
`{"error":"forbidden"}` even with valid credentials (hit in testing).

### 1.9 Keeping the rest of the system stable during Step 1

**`pkg/xds/translator.go`** — a temporary explicit *skip* of `SoapApi` in
`TranslateConfigs` (removed in Step 2). *Why:* without it, SOAP configs fell into the
REST fallback which logs `configuration is not a RestAPI` on every snapshot rebuild.
Deliberately **deferred** (would have errored before Step 2 existed):
`transform/registry.go` and `runtime_bootstrap.go`'s kind list — both invoke
`Transform()`, which did not support SOAP yet. CRUD survives controller restarts anyway
because handlers read the DB directly.

---

## Step 2 — Envoy routing + policy transformer (passthrough works end-to-end)

**Goal:** traffic flows: POST (SOAP envelopes) and GET (`?wsdl`) through the gateway to
the backend.

### 2.1 Why two implementations of "translation"

Per primer fact #2, REST has a legacy Envoy translation *and* a transformer for the
policy plane. SOAP mirrors that split exactly:

- without `translateSoapAPIConfig`, Envoy gets no routes (no traffic);
- without `SoapAPITransformer`, the policy xDS logs
  `unsupported kind for runtime config: SoapApi` on every deploy, the engine has **no
  route metadata** (so `APIKind` would be empty — breaking the Step 3 fault formatter),
  and no policy chains can ever be attached.

### 2.2 Envoy side

**`pkg/xds/translator.go` — `translateSoapAPIConfig`** (dispatched from the legacy path:
`else if cfg.Kind == models.KindSoapApi`; the Step 1 skip removed):

- **One upstream cluster** from `upstream.main` via the existing
  `resolveUpstreamCluster` + `createCluster` (TLS, timeouts, DNS — all REST behavior
  inherited for free).
- **Two routes via the existing `createRoute`**, with operation path `"/"`:
  - `POST <context>` — SOAP invocations.
  - `GET <context>` — `?wsdl`/`?xsd` retrieval. *Why no query matcher:* Envoy preserves
    query strings on proxying; a plain GET route is sufficient (design D4).
- Operation path `"/"` triggers `createRoute`'s root-path handling: match regex
  `^/<context>/?$` and rewrite to the upstream path + `/` — i.e., the gateway context maps
  onto the backend SOAP endpoint, which is exactly passthrough semantics. (Consequence:
  generated route names carry a trailing slash — `POST|/calculator/v1/|*` — which matters
  for diagnostics and Step 5.)
- Reusing `createRoute` rather than building routes by hand keeps method matching,
  timeouts, host rewriting, and rewrite rules identical to REST.

### 2.3 Policy side

**`pkg/transform/soapapi.go` — `SoapAPITransformer`** (new): produces the
`RuntimeDeployConfig` (routes keyed by the *same* names the Envoy side generates,
upstream cluster, policy chains). It deliberately **wraps `RestAPITransformer`** to reuse
`addUpstreamCluster`, `collectAPIPolicies`, and `buildPolicyChain` — upstream resolution
and policy version-resolution behave identically to REST by construction.

**Wiring** — `pkg/transform/registry.go` (`case "SoapApi"`), `cmd/controller/main.go`
(construct + register), and `cmd/controller/runtime_bootstrap.go` (add `KindSoapApi`).
*Why bootstrap only now:* bootstrap calls `Transform()` on startup; adding the kind in
Step 1 would have failed controller startup. With the transformer in place it restores
SOAP runtime configs after restarts.

---

## Step 3 — `soap-dispatch` policy + SOAP fault formatter

**Goal:** the gateway resolves the invoked SOAP operation, and every gateway-generated
error is a SOAP Fault.

### 3.1 Why a kernel hook instead of touching policies

Auth/throttle policies return an `ImmediateResponse` (status + JSON body). Making each
policy SOAP-aware would (a) touch many published policies, (b) still miss
engine-generated errors (no-chain 500, policy-crash 500). The kernel translates *every*
immediate response to ext_proc at a handful of well-defined sites — formatting there
covers all policies, present and future, in one place (design D7).

### 3.2 `internal/kernel/soap_fault.go` (policy-engine, new)

- `apiKindSoapApi = "SoapApi"` — local constant. *Why not the SDK enum:* the engine
  consumes the SDK as a published module; an enum addition would force an SDK release
  for a string compare (design D9).
- `soapVersionFromContentType` — `application/soap+xml` ⇒ 1.2, else 1.1. *Why infer
  per-request instead of plumbing the API's `soapVersion` through the policy xDS:* no
  cross-component schema change, and the fault always matches the dialect the *client*
  spoke (design D5).
- `buildSoapFault` — renders 1.1 (`faultcode`/`faultstring`) or 1.2
  (`env:Code`/`env:Reason`) envelopes; 4xx ⇒ `Client`/`Sender`, 5xx ⇒
  `Server`/`Receiver`; all text XML-escaped; the original error body is preserved in
  `<detail>` (it was already client-visible — no new disclosure).
- `applySOAPFaultFormat` — the guard logic: only `SoapApi` kind, only status ≥ 400, and
  **pass through any response already carrying an XML content type** (a policy may craft
  its own fault). It replaces only body + Content-Type; **status and transport headers
  are preserved** so `WWW-Authenticate`/`Retry-After` keep working (design D6).
- `rawRequestContentType` — header extraction from the raw ext_proc message for the one
  error path that runs before an execution context exists.

### 3.3 Hook sites — why exactly these eight

- **6 sites in `internal/kernel/translator.go`** — every `ImmediateResponse` translation:
  request-headers, request-body (x2 code paths), response-headers, response-body (x2).
  Identical one-line insertion (`immResp = applySOAPFaultFormat(immResp, execCtx)`) so
  analytics-metadata handling at each site is untouched.
- **`internal/kernel/extproc.go`** — the "no policy chain found" 500, rebuilt through the
  same formatter using `rm.APIKind` + the raw request's content type (no exec context
  exists yet).
- **`internal/kernel/execution_context.go` — `handlePolicyError`** — policy-crash 500,
  same treatment.

Anything less leaves a class of errors answering JSON to SOAP clients.

### 3.4 `system-policies/soap-dispatch/` (new)

`policy-definition.yaml` (name `wso2_apip_sys_soap_dispatch` v1.0.0; params:
`operations[]{name,soapAction}`, `validateEnvelope` default `true`, `soapVersion`),
`soapdispatch.go`, `go.mod` (+ module added to the root `go.work`), and registration in
`system-policies/system-build-lock.yaml` — the file the gateway-builder reads to compile
system policies into the policy-engine binary (this is why Step 3 requires a
**gateway-runtime image rebuild**).

Implementation choices, each with rationale:

- **Stateless singleton; params read per call** — the exact pattern of the analytics
  system policy; the engine passes per-instance params to each handler invocation.
- **`Mode()`: request headers Process, request body Buffer, response Skip** — body
  buffering is required for QName dispatch and envelope validation; the response flow is
  untouched (passthrough).
- **Resolution order** `SOAPAction` header (quotes stripped — clients send both forms) →
  Content-Type `action` param (`mime.ParseMediaType`) → first `<Body>` child local name.
  This is the SOAP-binding-defined precedence for operation identification.
- **Body-element dispatch** (added in the spec-parity follow-up): each operation may
  declare `bodyElement` — the Body wrapper's local name. For document/literal services the
  wrapper frequently differs from the operation name (classic WSDL example: operation
  `GetLastTradePrice`, wrapper `TradePriceRequest`). `resolveOperationName` therefore
  matches the resolved body element against declared `bodyElement`s **first**, then falls
  back to matching the operation `name`, then to the raw element name. The validator
  rejects duplicate non-empty `bodyElement`s (they would dispatch ambiguously). This
  completes the operation-identity model across all three dispatch styles.
- **`firstBodyElement`** — a streaming `encoding/xml` token scan, namespace-agnostic
  (handles 1.1 and 1.2 prefixes), explicitly skipping the `<Header>` subtree; rejects
  non-`Envelope` roots and empty bodies. No DOM build, no DTD processing.
- **Declared-operation mapping with raw-value fallback** — undeclared operations pass
  through with the raw action/QName: *passthrough must not reject traffic the backend
  would accept*; declarations only improve naming and enable per-op policies.
- **Malformed envelope ⇒ 400 `ImmediateResponse`** with a plain JSON body — deliberately
  *not* a hand-built fault, because the kernel hook (3.2/3.3) renders it in the correct
  dialect. Single source of truth for fault formatting.
- **Publication** to `SharedContext.Metadata["soap_operation"/"soap_action"]` — the
  engine-provided cross-policy channel — plus analytics metadata.

### 3.5 Controller injection

**`pkg/constants/constants.go`** — `SOAP_DISPATCH_SYSTEM_POLICY_NAME/VERSION`.
**`pkg/transform/soapapi.go` — `soapDispatchPolicyInstance`** builds the instance with
the API's declared operations + soapVersion as params (the policy's mapping table) and
prepends it to the **POST chain only**, before `InjectSystemPolicies` (final order:
`[analytics, soap-dispatch, user policies]`).

*Why transformer-injected, not `defaultSystemPolicies`:* the global list's `Enabled`
hook receives only the gateway config — no API kind; widening that API would ripple
through all callers. The transformer knows both the kind and the operations (design D8).

*Why POST only:* the GET (`?wsdl`) route has no body; envelope validation would 400
every WSDL fetch. **API-level policies (incl. auth) intentionally remain on the GET
route** — removing them would open an unauthenticated proxy path to the backend and leak
the contract (design D10; validated in user testing when an unauthenticated `?wsdl`
correctly returned a 401 SOAP fault).

---

## Step 4 — Analytics operation labeling (and auth/throttle verification)

**Goal:** published analytics events identify the invoked SOAP operation; confirm
transport policies need no changes.

### 4.1 Why the plan changed — and the analytics policy was *not* touched

The original plan said "extend the analytics system policy". Tracing the pipeline showed
the actual gaps were elsewhere: (a) the event's resource field is taken from the request
path — identical for every SOAP operation; (b) **custom analytics keys are dropped at the
Access Log Service unless explicitly forwarded** — only known keys reach publishers. MCP
(same one-endpoint/many-operations shape) already solved this with an explicit forwarding
block; SOAP follows that precedent. Two small changes, zero changes to the analytics
policy:

### 4.2 `soap-dispatch`: set the standard operation dimension

`publishOperation` now also sets `SharedContext.OperationPath` to the resolved operation.
*Why this is safe:* audited every consumer — `OperationPath` feeds only the
`x-wso2-operation-path` analytics key, OpenTelemetry span attributes, and the Python
policy bridge; **nothing in routing, path rewriting, or CEL reads it**. For SOAP routes
its route-level value is just `/`, so overwriting strictly adds information.
`OnRequestHeaders` additionally emits `soap_action` in analytics metadata.

### 4.3 ALS forwarding (`internal/analytics/analytics.go`)

A `SoapApi` block (mirroring the MCP block, after it) forwards
`soap_operation`/`soap_action` from the extracted metadata into
`event.Properties["soapAnalytics"]`. Note the analytics package's `APITypeKey` constant
actually holds `x-wso2-api-kind` (naming is historical) — the comparison
`== "SoapApi"` is correct.

### 4.4 Auth/throttle verification — no code

`api-key-auth` (401-fault + valid-key paths) and `basic-ratelimit` (429-fault) were
verified end-to-end in manual testing with **zero changes** — they read HTTP headers
only, which was the core reuse claim of the design. `jwt-auth` and
`subscription-validation` need IT fixtures (issuer, application) and are deferred to the
Step 6 integration suite, matching how REST covers them.

---

## Step 5 — Per-operation routes via SOAPAction

**Goal:** operation-level policies/throttling by giving each declared operation its own
route → its own policy chain (the route-name join key, primer fact #1).

### 5.1 Route naming

**`pkg/xds/translator.go` — `GenerateSoapOperationRouteName`**:
`POST|FULL_PATH|VHOST|soapAction=ACTION` — the standard 3-segment name plus a suffix
segment. *Why a shared helper:* the Envoy translator names the route; the transformer
keys the policy chain; one helper guarantees they can never drift. *Why suffix after the
vhost:* the vhost is positionally the 3rd segment for every parser (see 5.4) — suffix
segments never disturb it, even if a `soapAction` itself contained `|`.

### 5.2 Envoy side

In `translateSoapAPIConfig`, for each declared operation with a non-empty `soapAction`:
clone the generic POST route via `createRoute` (identical match/rewrite/cluster), rename
with the helper, and append a `soapaction` header matcher with regex
**`^"?<QuoteMeta(action)>"?$`**.

- *Why regex, not exact match:* SOAP 1.1 clients legitimately send
  `SOAPAction: "urn:Add"` **or** `SOAPAction: urn:Add`; exact matching would miss the
  quoted form (confirmed in testing — a quoted action must hit the same per-op route).
- *Why no explicit ordering:* the existing route sorter ranks routes with more header
  matchers first at equal path specificity, so per-op routes (`:method` + `soapaction`)
  automatically beat the generic POST route (`:method` only).
- Operations with an empty `soapAction` get **no** route — doc/literal requests carry no
  usable header; they take the generic route where `soap-dispatch` resolves the operation
  from the body.

### 5.3 Policy side

In `SoapAPITransformer.Transform`, per declared operation: a route entry under the same
key (with `OperationPath = op.Name`, labeling route metadata with the logical operation
even before `soap-dispatch` runs) and a chain built by
`buildPolicyChain(apiPolicies, apiData.Policies, **op.Policies**)` — this is the line
that makes `spec.operations[].policies` effective — plus `soap-dispatch` and system
policies. The generic POST route keeps API-level policies only, so operation policies
never leak onto fallback traffic.

### 5.4 The vhost-grouping bug (found in user testing) — and its fix

**Symptom:** policy xDS showed all 4 routes/chains, but Envoy had only the 2 generic
routes; per-op throttling never engaged.
**Root cause:** `TranslateConfigs` groups routes into virtual hosts by parsing the route
name — `strings.Split(name, "|")` with a **`len(parts) != 3`** check. The new 4-segment
per-op names failed it and were *silently dropped from every virtual host*.
**Fix:** relax to `len(parts) < 3` — the vhost is positional (`parts[2]`) and unaffected
by suffix segments. Verified by grep that this grouping is the **only** route-name parser
in the controller *and* the policy-engine, so no other component needed changes.
**Hardening:** the relaxation is only sound if user input cannot inject `|` *before* the
vhost segment — so validation now rejects `|` in `spec.context` (shared
`validateContext`, all kinds) and REST `spec.operations[].path`. Previously such configs
were accepted and their routes silently dropped; now they fail fast with a clear error.
**Process lesson, encoded as a test:** the original unit tests exercised
`translateSoapAPIConfig` in isolation and passed while the pipeline was broken.
`TestTranslateConfigs_SoapPerOpRouteInVirtualHost` now runs the full
config → `TranslateConfigs` → virtual-host pipeline, asserting the per-op route is
present *and sorted above* the generic route.

---

## Cross-cutting: checklist for adding any new API kind

Every item below bit us (or would have) during this work; each maps to a concrete failure
mode observed in testing:

| Touchpoint | Failure when missed |
|---|---|
| `relativeRoles` map in `cmd/controller/main.go` | HTTP **403 `{"error":"forbidden"}`** on all new endpoints (the `x-basicauth-roles` spec extension is *not* consumed) |
| Per-kind DB table in **both** SQL schemas + `kindToResourceTable` + `unmarshalSourceConfig` | `unknown kind: <Kind>` on the first deploy |
| Use the **full** generated type (with `Status`) in all type switches | Responses missing the `status` block; switches silently fall through |
| `StoredConfig` accessors (`GetContext`/`GetPolicies`/`GetMetadata`/…) | Zero values leak into conflict checks, search, analytics |
| Envoy translation **and** transformer registry (two planes!) | Routes without policies, or policies without routes; engine missing `APIKind` |
| `supportsRuntimeBootstrapKind` (only once `Transform` works) | Runtime configs not restored after controller restart — or startup errors if added too early |
| Route-name format discipline (vhost = 3rd `\|`-segment) | Routes silently dropped from virtual hosts |
| Rebuild scope awareness | Controller changes need `make build-controller`; compiled-in system policies need the **runtime** image (`make build`/`build-gateway-runtime`) |

## Test inventory (all green)

| Area | File | Covers |
|---|---|---|
| Validator | `pkg/config/api_validator_test.go` (additions) | context/path `\|` rejection (+ existing SOAP validation paths) |
| Transformer | `pkg/transform/soapapi_test.go` | base routes/cluster, API-level chains, soap-dispatch POST-only + params, per-op chains + no-leak-to-generic, wrong-kind |
| Envoy translation | `pkg/xds/soapapi_test.go` | route shape/methods/cluster, per-op route name + matcher regex, **full-pipeline vhost grouping + sort order**, wrong-kind |
| Fault formatter | `policy-engine/internal/kernel/soap_fault_test.go` | 1.1/1.2 dialects, status/header preservation, 4xx/5xx fault codes, XML pass-through, non-SOAP no-op, XML escaping, end-to-end translation |
| soap-dispatch | `system-policies/soap-dispatch/soapdispatch_test.go` | header/content-type/QName resolution, declared-op mapping, undeclared fallback, malformed/non-envelope rejection, validation toggle, `OperationPath` publication |
| Analytics | `policy-engine/internal/analytics/analytics_test.go` (additions) | `soapAnalytics` forwarding + non-SOAP regression |

Manual end-to-end procedures: [soap-api-testing-plan.md](soap-api-testing-plan.md).
