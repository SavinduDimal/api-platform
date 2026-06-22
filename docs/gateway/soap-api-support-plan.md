# SOAP API Support — Implementation Plan (Phase 1: Gateway Passthrough)

## Scope

**Phase 1 (this document):**
- SOAP passthrough (SOAP-to-SOAP): the gateway exposes a SOAP endpoint, applies policies, and proxies the unmodified SOAP envelope to the backend.
- Authentication via HTTP transport: Basic Auth, API Key, OAuth2/JWT bearer (existing policies, reused unchanged).
- Per-operation policy granularity via `SOAPAction` header matching — each declared operation gets its own Envoy route and policy chain.
- SOAP Fault responses for gateway errors (auth 401, throttle 429, not found 404).

**Deferred to Phase 2 / other components:**
- WS-Security (UsernameToken/X.509/SAML inside `<wsse:Security>` SOAP header)
- SOAP-to-REST transformation (expose JSON to clients, transform to/from SOAP for backend)
- **WSDL import** (parse WSDL → generate `SoapApi`): belongs in `platform-api` (control plane), NOT the gateway — see "WSDL Import & Exposure" below
- XML Schema (XSD) payload validation
- MTOM/attachments

## WSDL Import & Exposure (decisions)

**WSDL import is NOT a gateway-controller responsibility.** Rationale: the gateway-controller never parses OpenAPI for REST APIs either — `platform-api` does that via `POST /import/openapi` (`platform-api/src/internal/handler/api.go:786`, using `libopenapi`) and pushes a *declarative* `RestApi` to the gateway. WSDL import is the exact SOAP analog and belongs in `platform-api` (a future `POST /import-wsdl` that parses the WSDL and emits a declarative `SoapApi`). The declarative `SoapApi` model from Step 1 is precisely what such an importer would produce.

> **Decision:** WSDL import → deferred to `platform-api` (out of the gateway-only first phase). In this phase, users hand-author the `operations[]` (name + soapAction) list. Pure passthrough works even with zero operations declared (operations are only needed for per-operation policies/throttling/analytics).

**WSDL exposure to consumers (`?wsdl`):** SOAP clients fetch `<endpoint>?wsdl`. This IS a gateway data-plane behavior.

> **Decision:** Passthrough `?wsdl` (and `?xsd`) to the backend — add a `GET` route on the SOAP context that forwards the query to the backend SOAP service. No WSDL parsing, no storage, no address rewriting in Phase 1. (Folded into Step 2.)

---

## Architecture: How the Gateway Works Today (the parts SOAP touches)

```
API YAML (kind: RestApi)
  → gateway-controller: parse → validate → store → transform(RuntimeDeployConfig)
  → xDS translator: build Envoy Routes/Clusters/Listeners + PolicyChains
  → pushed via gRPC to Router (Envoy) + Policy-Engine
  → at runtime: Envoy matches route → ext_proc to Policy-Engine → proxy to upstream
```

| Concern | Current behaviour | File |
|---------|-------------------|------|
| API type | `Kind` field; branched on at parse, validate, transform, translate | `pkg/models/stored_config.go:34`, `pkg/utils/api_deployment.go:196`, `pkg/config/api_validator.go:91` |
| Routing unit | One Envoy route per `(method, path, vhost)`. Route name = `METHOD\|FULL_PATH\|VHOST` | `pkg/xds/translator.go:151,1743` |
| Route match | Matches `:method` exactly via `HeaderMatcher` + path regex. Extra header matchers supported | `translator.go:1845` |
| Dynamic upstream | `cluster_header` routing lets policy-engine pick upstream via `x-target-upstream` | `translator.go:1811` |
| Policy attachment | API-level + operation-level; system policies prepended; each route_name → one PolicyChain | `pkg/policy/builder.go`, `pkg/utils/system_policies.go` |
| Policy runtime | ext_proc gRPC; policies can buffer+inspect/modify request/response body | `policy-engine/internal/kernel/extproc.go` |
| Error responses | Emitted as JSON (`{"error":"..."}`, content-type `application/json`) | `translator.go:494` |
| Existing policies | `basic-auth`, `api-key-auth`, `jwt-auth`, `subscription-validation`; `basic/advanced/token-based ratelimit`; system `analytics`; `json-xml-mediator` | `build.yaml` |

---

## Why SOAP Is Different

1. **One endpoint, always POST.** A SOAP service has a single URL. The HTTP method+path alone cannot identify the operation.
2. **The operation is in the message, not the URL.** Identified by:
   - SOAP 1.1: `SOAPAction` HTTP header (content-type `text/xml; charset=utf-8`)
   - SOAP 1.2: `action` parameter inside `Content-Type: application/soap+xml; action="..."` header
   - Doc/literal with empty SOAPAction: root QName of first child of `<soap:Body>`
3. **Errors must be SOAP Faults.** A SOAP client cannot consume the gateway's JSON `{"error":...}` responses. It expects `<soap:Fault>` with HTTP 500 (for SOAP faults) and `Content-Type: text/xml`.

---

## Routing Strategy

| Scenario | Mechanism | Notes |
|----------|-----------|-------|
| Per-operation with `soapAction` declared | Additional Envoy route matching `:method=POST` + path + `SOAPAction` header | Reuses existing `HeaderMatcher` at `translator.go:1845`. Zero body parsing. Works for SOAP 1.1 / RPC-literal. |
| SOAP 1.2 or doc/literal (action in Content-Type, or empty SOAPAction) | Single service route + body inspection by `soap-dispatch` system policy | Policy buffers body, extracts operation QName, writes to context. |
| Default/catch-all | Single service route (POST + context path) | Applies API-level policies to all unmatched operations. |

Route name format (reusing existing): `POST|/context/version|vhost` for the service route, `POST|/context/version|vhost|soapAction=...` for per-op routes.

---

## Policy Analysis

| Concern | Reuse as-is? | Notes for SOAP Phase 1 |
|---------|-------------|------------------------|
| **Auth** — Basic, API-Key, JWT/OAuth2 | ✅ Yes | Read HTTP headers only. Work transparently. No new policy. |
| **Auth** — WS-Security | ❌ Deferred | New `ws-security-auth` policy (Phase 2). |
| **Authorization** — app/subscription | ✅ Yes | `subscription-validation` unchanged. |
| **Authorization** — per-operation scopes | ⚠️ Via routing | Per-op routes give each op a distinct policy chain → existing scope checks work. |
| **Throttling** — API/app/IP level | ✅ Yes | `basic/advanced/token-based ratelimit` keys on headers/auth context unchanged. |
| **Throttling** — per-operation | ✅ Via routing | Per-op SOAPAction routes → distinct route_name → distinct throttle policy. |
| **Analytics** | ⚠️ Minor extend | Captures API/app/user unchanged. Operation dimension needs resolved op from context. Minor extension to system analytics policy. |
| **SOAP message validation** | ❌ Phase 2 | New `soap-validate` policy (XSD/WSDL-driven). |
| **SOAP↔JSON transform** | ❌ Phase 2 | `json-xml-mediator` is the building block. |

### New primitives required

1. **`soap-dispatch` system policy** (`system-policies/soap-dispatch/`): buffers request body, resolves SOAP operation (SOAPAction → Content-Type action → body QName), validates envelope well-formedness, writes `operation`+`soapVersion` into shared execution context and analytics metadata. Rejects malformed envelopes with a SOAP Fault.

2. **SOAP Fault formatter hook** in `policy-engine/internal/kernel/extproc.go`: when `api_kind == SoapApi`, converts immediate/error responses (auth 401, throttle 429, 404) to `<soap:Fault>` with HTTP 500 and correct content-type. Keyed on `api_kind` from route metadata so it works across all existing policies without modifying them.

---

## Implementation Checklist

### Step 1 — Data Model + Validator + Parser (gateway-controller)
> Goal: deploy a `SoapApi` and see it stored; CRUD endpoints work.

- [x] `KindSoapApi = "SoapApi"` — `pkg/models/stored_config.go`
- [x] `SoapAPIRequest`, `SoapAPI`, `SoapAPIData`, `SoapOperation` schemas in `api/management-openapi.yaml`
- [x] `/soap-apis` and `/soap-apis/{id}` endpoints in `api/management-openapi.yaml`
- [x] Regenerate `pkg/api/management/generated.go` via `make generate` in gateway-controller
- [x] `validateSoapAPIConfiguration()` — `pkg/config/api_validator.go`
- [x] `case "SoapApi":` parser branch — `pkg/utils/api_deployment.go`
- [x] Update `GetContext()`, `GetMetadata()`, `GetLabels()`, `GetAnnotations()`, `GetPolicies()` — `pkg/models/stored_config.go`
- [x] `pkg/api/handlers/soap_api_handler.go` — CRUD handler (Create, List, GetById, Update, Delete + API keys)
- [x] **Authorization role map** — add `/soap-apis` entries to the hardcoded `relativeRoles` map in `cmd/controller/main.go` (NOT generated from the `x-basicauth-roles` OpenAPI extension; a missing entry returns HTTP 403 `{"error":"forbidden"}`, not 404)
- [x] **Spec parity pass vs RestAPI** — aligned field patterns (version `^v\d+\.\d+$`, context charset incl. `$version` doc, vhosts `required: [main]` + host pattern), added `subscriptionPlans` (declarative only — see note), full-resource example with `status`, 500 responses on get/update + API-key endpoints, 400 on API-key endpoints, yaml+json bodies on API-key requests, `APIKeyUpdateRequest`/`APIKeyRevocationResponse` schemas on update/revoke. NOTE: `subscriptionPlans` is stored but subscription *enforcement* is still `RestApi`-gated in `pkg/subscriptionxds/subscription_snapshot.go` and `pkg/api/handlers/subscription_handler.go` — wiring SoapApi into those gates is Step 6 work alongside the subscription-validation IT.
- [x] **Storage layer** — `soap_apis` table in both `pkg/storage/gateway-controller-db.sql` and `...postgres.sql`; `kindToResourceTable` + `unmarshalSourceConfig` SoapApi cases in `pkg/storage/sql_store.go` (a missing entry returns `unknown kind: SoapApi` on upsert)
- [x] **Use `api.SoapAPI` (full type w/ Status) everywhere, not `api.SoapAPIRequest`** — mirrors REST so `buildResourceResponse` injects the server-managed `status` block. Touches `stored_config.go`, `api_validator.go`, `policy_validator.go`, `api_deployment.go`, `soap_api_handler.go`
- [x] `buildResourceResponse` (`resource_response.go`), `ExtractNameVersion` (`helpers.go`), `extractConfigDisplayNameVersion` (`api_key.go`) — SoapAPI cases
- [x] **xDS translator** — clean skip of `SoapApi` in `TranslateConfigs` (no routing yet; avoids "not a RestAPI" error spam in snapshot regen)
- [x] Deferred (would error if added pre-Step-2): `transform/registry.go`, `runtime_bootstrap.go:supportsRuntimeBootstrapKind` (both call `Transform`), control-plane sync, immutable loader, subscription xDS. CRUD survives controller restart without these since handlers read the DB directly.

### Step 2 — Transformer + Single-Route xDS + Cluster (gateway-controller) — DONE
> Goal: SOAP passthrough proxies end-to-end (no per-operation policies yet).

**Architecture note discovered during impl:** the Envoy *route* translator never gets `SetTransformers` called, so REST routes come from the **legacy `translateAPIConfig`** path; the transformer/`RuntimeDeployConfig` registry feeds only the **policy xDS** (`policyxds.PolicyManager`). SOAP therefore needs BOTH a legacy translator (Envoy routes) and a transformer (policy chains + `api_kind` route metadata for the policy engine).

- [x] `pkg/transform/soapapi.go` — `SoapAPITransformer` → `RuntimeDeployConfig` (POST + GET routes at context, one upstream cluster); reuses `RestAPITransformer` helpers (`addUpstreamCluster`, `collectAPIPolicies`, `buildPolicyChain`) + injects system policies (analytics)
- [x] Registered SOAP transformer: `transform.NewRegistry(rest, soap, llm)` + `Registry.Transform` switch + `cmd/controller/main.go` wiring; added `KindSoapApi` to `supportsRuntimeBootstrapKind`
- [x] `translateSoapAPIConfig()` in `pkg/xds/translator.go` — parallel to `translateAPIConfig()`; reuses `resolveUpstreamCluster`/`createCluster`/`createRoute`. Removed the Step-1 skip; wired into the legacy dispatch (`else if cfg.Kind == KindSoapApi`)
- [x] Single service endpoint: POST (SOAP) + GET (?wsdl/?xsd passthrough) at the context path (operation path `/` → matches context exactly; query string preserved on passthrough). No separate query-param matching needed.
- [x] Unit tests: `pkg/transform/soapapi_test.go`, `pkg/xds/soapapi_test.go`; example `examples/soap-api.yaml`
- [ ] *(deferred to Step 5)* Per-operation `SOAPAction` header-match routes
- [ ] *(deferred to Step 3)* `soap_version` in route metadata (needed by the SOAP Fault formatter)

### Step 3 — `soap-dispatch` System Policy + SOAP Fault Formatter — DONE
> Goal: operation resolution + SOAP-correct error responses.

**Design decisions made during impl:**
- **Fault SOAP version is inferred per-request from the request `Content-Type`** (`application/soap+xml` → 1.2 fault, else 1.1) instead of plumbing `soap_version` through the policy-xDS RouteConfig — simpler and more correct (the fault always matches the dialect the client spoke).
- **HTTP status is preserved** (401/429/500 stay as-is; only body + Content-Type are rewritten into a fault) so `WWW-Authenticate` / `Retry-After` transport semantics keep working. Policies that already produced an XML body (hand-crafted faults) are passed through untouched.
- **No SDK change**: the engine/policies reference the published SDK, so `"SoapApi"` is a local constant (`kernel/soap_fault.go`, policy code) rather than a new `APIKind` enum entry.
- **soap-dispatch is injected by the SOAP transformer, not `defaultSystemPolicies`** (which has no kind awareness). It is attached to the **POST route only** — on the GET (`?wsdl`) route its empty-body envelope validation would reject the request.
- **API-level policies (incl. auth) apply to BOTH routes — WSDL retrieval is protected by default.** Only soap-dispatch is excluded from GET. An unauthenticated GET route would be an open proxy path to the backend and would leak the service contract; clients fetching `?wsdl` must authenticate the same way as invocations. (If public WSDL is ever needed, that becomes an explicit spec option in a later phase.)

- [x] **SOAP Fault formatter** — `policy-engine/internal/kernel/soap_fault.go` (`applySOAPFaultFormat`, `buildSoapFault` 1.1+1.2, XML escaping); hooked at all 6 `ImmediateResponse` translation sites in `kernel/translator.go`, the no-chain 500 in `kernel/extproc.go`, and `handlePolicyError` in `kernel/execution_context.go`. 8 unit tests in `soap_fault_test.go`.
- [x] **`system-policies/soap-dispatch/`** — `policy-definition.yaml` (`wso2_apip_sys_soap_dispatch` v1.0.0; params: `operations[]{name,soapAction}`, `validateEnvelope` default true, `soapVersion`), `soapdispatch.go` (resolution order: SOAPAction header → Content-Type `action` param → first Body child local-name; publishes `soap_operation`/`soap_action` to `SharedContext.Metadata` + analytics metadata; rejects malformed envelopes with 400 → rendered as SOAP Fault by the kernel hook). 10 unit tests. Registered in `system-build-lock.yaml`; module added to root `go.work`.
- [x] **Controller injection** — `SOAP_DISPATCH_SYSTEM_POLICY_NAME/VERSION` constants; `soapDispatchPolicyInstance()` in `pkg/transform/soapapi.go` prepends the policy (with declared operations + soapVersion as params) to the **POST** chain before `InjectSystemPolicies`. Final chain order: `[analytics, soap-dispatch, user policies]`.
- [ ] *(Step 4)* Verify existing auth/throttle policies end-to-end on a SOAP API; analytics operation labeling reads `soap_operation`.

### Step 4 — Auth, Throttle, Analytics Wiring — DONE
> Goal: existing HTTP-transport auth + throttle policies work; analytics labels operation correctly.

**Design decision:** the analytics *system policy* needed **no changes**. Instead:
- `soap-dispatch` now sets `SharedContext.OperationPath` to the resolved operation (audited safe: OperationPath is consumed only by analytics metadata `x-wso2-operation-path`, OTel spans, and the Python policy bridge — not by routing/path-rewrite/CEL), and emits `soap_action` alongside `soap_operation` in analytics metadata.
- The ALS event assembly (`policy-engine/internal/analytics/analytics.go`) forwards `soap_operation`/`soap_action` into `event.Properties["soapAnalytics"]` for `SoapApi` events — mirroring the existing MCP pattern (custom analytics keys are otherwise dropped; only explicitly forwarded keys reach publishers).

- [x] `api-key-auth` + `basic-ratelimit` verified end-to-end on SOAP in Step 3 testing (401/429 → SOAP Faults; valid key → 200). No code changes — they read HTTP headers only. `jwt-auth` / `subscription-validation` deferred to Step 6 IT (need issuer/app setup).
- [x] Analytics operation dimension: `soap-dispatch` → `OperationPath` + ALS `soapAnalytics` forwarding; tests `TestProcess_SoapAnalytics`, `TestProcess_NonSoapEventHasNoSoapAnalytics`, OperationPath assertions in soap-dispatch tests.

### Step 5 — Per-Operation Routes via `SOAPAction` Header Match — DONE
> Goal: per-operation throttle/auth/policies via distinct route_name per operation.

**Mechanics:**
- Route name: `POST|FULL_PATH|VHOST|soapAction=ACTION` via `xds.GenerateSoapOperationRouteName()` — shared by the Envoy translator and the SOAP transformer so route names and policy-chain keys align.
- The Envoy per-op route = the generic POST route + a `soapaction` header matcher using regex `^"?<action>"?$` (SOAP 1.1 clients send the value quoted or unquoted).
- **No explicit ordering needed**: the route sorter ranks routes with more header matchers above those with fewer at equal path specificity, so per-op routes (`:method` + `soapaction`) automatically match before the generic POST route.
- Fallback: undeclared actions, doc/literal (empty SOAPAction), and SOAP 1.2 (action in Content-Type, no header) fall through to the generic POST route — API-level policies only, operation still resolved by soap-dispatch for analytics.
- Per-op `Route.OperationPath` is set to the logical operation name so route metadata (analytics/tracing) is labelled even before soap-dispatch runs.

- [x] `translateSoapAPIConfig()`: per-op routes (createRoute + renamed + extra `soapaction` regex matcher); empty-soapAction operations skipped
- [x] `SoapAPITransformer.Transform()`: per-op route keys + chains = `[analytics, soap-dispatch, API-level, operation-level]` via `buildPolicyChain(apiPolicies, apiData.Policies, op.Policies)`; generic POST route keeps API-level only
- [x] Tests: per-op route shape/regex/name (xds), per-op chain composition + no-leak-to-generic-route (transform)

### Step 6 — Tests + Example + Docs
> Goal: full test coverage, example for users.

- [ ] `it/features/soap.feature` — deploy `SoapApi`, invoke with SOAP 1.1 envelope + SOAPAction, assert response; assert auth failures return SOAP Faults; assert per-op throttle
- [ ] SOAP echo backend in `it/` docker-compose
- [ ] `examples/soap-api.yaml`
- [ ] Update this document with completed steps

---

## SoapAPI Resource Format (user-facing YAML)

```yaml
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: stockquote-api-v1.0
  labels:
    environment: production
spec:
  displayName: StockQuote API
  version: v1.0
  context: /stockquote/$version
  soapVersion: "1.1"                     # "1.1" (default) or "1.2"

  upstream:
    main:
      url: http://stockquote-service:8080/services/StockQuoteService

  policies:
    - name: api-key-auth
      version: v1
      params:
        key: X-API-Key
        in: header
    - name: basic-ratelimit
      version: v1
      params:
        limits:
          - requests: 1000
            duration: "1m"

  operations:
    - name: getQuote
      soapAction: "urn:getQuote"         # empty string "" is valid (SOAP 1.1 default)
      policies:
        - name: basic-ratelimit
          version: v1
          params:
            limits:
              - requests: 100
                duration: "1m"
    - name: getFullQuote
      soapAction: "urn:getFullQuote"
```

---

## Key Files Reference

| Component | File | Purpose |
|-----------|------|---------|
| Kind constants | `gateway-controller/pkg/models/stored_config.go` | `KindSoapApi` |
| OpenAPI schema | `gateway-controller/api/management-openapi.yaml` | `SoapAPIRequest`, `SoapAPI`, `SoapAPIData`, `SoapOperation` |
| Generated types | `gateway-controller/pkg/api/management/generated.go` | Auto-generated; run `make generate` |
| Validator | `gateway-controller/pkg/config/api_validator.go` | `validateSoapAPIConfiguration()` |
| Parser dispatch | `gateway-controller/pkg/utils/api_deployment.go` | `case "SoapApi":` |
| StoredConfig methods | `gateway-controller/pkg/models/stored_config.go` | `GetContext()`, `GetMetadata()`, `GetLabels()`, etc. |
| CRUD handler | `gateway-controller/pkg/api/handlers/soap_api_handler.go` | HTTP handler for `/soap-apis` |
| Transformer | `gateway-controller/pkg/transform/soapapi.go` | Step 2 |
| xDS translator | `gateway-controller/pkg/xds/translator.go` | `translateSoapAPIConfig()` — Step 2 |
| System policy | `gateway/system-policies/soap-dispatch/` | Step 3 |
| Fault formatter | `gateway-runtime/policy-engine/internal/kernel/extproc.go` | Step 3 |
| IT tests | `gateway/it/features/soap.feature` | Step 6 |
| Example | `gateway/examples/soap-api.yaml` | Step 6 |
