# Gateway Error Response Customization — Implementation Walkthrough

**Audience:** code reviewers and future maintainers.
**Companions:** `error-response-customization-design.md` (the approved design), `error-response-customization-plan.md` (the phase plan), `error-response-customization-testing-plan.md` (how to verify).
**Scope:** all four phases are implemented. Phase 0 (schema), Phase 1 (gateway policy errors, global + per-API), Phase 2 (Envoy local replies + no-route 404, global), Phase 3 (backend error reshaping, per-API opt-in), Phase 4 (per-API Envoy local replies).

---

## 0. The one-paragraph mental model

Gateway errors come from **three places**, and each is customized where it is born. **(A) Policy errors** (auth 401, throttle 429, policy 500, …) are `ImmediateResponse`s inside the policy engine — they are rewritten at the engine's existing choke points, per request. **(B) Backend errors** (the upstream app's own 4xx/5xx) flow through the engine's response phase — an API can opt in to have them replaced. **(C) Envoy local replies** (503 no-healthy-upstream, 504 timeout, 404 no-route) never reach the engine — they are customized inside Envoy via xDS-generated `local_reply_config` and a `DirectResponse`, rendered **statically at deploy time**. One OpenAPI-shaped config document drives all three, globally (`[error_handling]` in `config.toml`) and per API (`spec.errorResponses`).

**Backward compatibility invariant:** with nothing configured, every response is byte-for-byte identical to before. Every code path below is behind either the `[error_handling]` flag or the presence of a per-API `errorResponses` block.

---

## 1. The configuration surface (what users write)

One shape everywhere — the OpenAPI 3.x **Responses Object** subset:

```yaml
responses:
  "401":                              # 3-digit status key, or "default"
    description: Authentication failure
    x-status-code-override: 401      # optional: return a different status
    content:
      application/json:               # negotiated against the request Accept header
        example: { code: 401, message: "Auth required", requestId: "${requestId}" }
      application/xml:
        example: "<error>${message}</error>"
```

- **Global:** a YAML file referenced from `config.toml` (`[error_handling] enabled / config_file / default_media_type`). Commented sample: `gateway/configs/error-responses.yaml` — its header comments are the user-facing behavior contract.
- **Per API:** the same shape under `spec.errorResponses` on `RestApi` and `SoapApi` kinds.
- The gateway renders the media type's **`example`** (`schema` is descriptive only). Placeholders: `${statusCode} ${message} ${errorCode} ${category} ${requestId} ${apiName} ${apiVersion}`.
  - ⚠️ **Reviewed decision:** the syntax is `${name}`, not the design's original `{{name}}` sketch. Management-API payloads and filesystem artifacts are rendered through Go `text/template` (artifact templating, `{{ env "..." }}`) *before* YAML parsing — `{{name}}` in a per-API `errorResponses` block fails there with `function "name" not defined`. Found during manual testing; recorded in design §4.4.
- Validation (shared by engine and controller): keys are exact 3-digit 100–599 or `default` (no `5XX` wildcards in v1), override in range, ≥1 media type per entry, media types limited to JSON/XML/`+json`/`+xml`/`text/plain`, each media type needs `example` or `examples`, 16 KB example cap, placeholder whitelist enforced recursively.

---

## 2. Package map (where everything lives)

| Concern | Engine (policy-engine module) | Controller (gateway-controller module) |
|---|---|---|
| Taxonomy + model + parser | `internal/kernel/errorformat/` (`category.go`, `model.go`, `parse.go`) | `pkg/errorconfig/` (`errorconfig.go`, `parse.go`) |
| Request-time resolution/rendering | `errorformat/resolver.go`, `renderer.go` | — |
| Build-time (static) resolution | — | `pkg/errorconfig/resolve.go` (`ResolveStatic`) |
| Choke-point glue (sources A + B) | `internal/kernel/error_format.go` | — |
| Envoy local replies + no-route 404 (source C) | — | `pkg/xds/local_reply.go` |
| Config structs | `internal/config/config.go` (`ErrorHandlingConfig`) | `pkg/config/config.go` (same shape) |
| Per-API schema + validation | — | `api/management-openapi.yaml` → `generated.go`; `pkg/config/api_validator.go` |
| Per-API delivery to engine | `internal/xdsclient/handler.go`, `internal/kernel/mapper.go` | `pkg/transform/{restapi,soapapi}.go`, `pkg/models/runtime_deploy_config.go`, `pkg/policyxds/snapshot.go` |

> **Why two mirrored packages?** Engine and controller are separate Go modules. `errorformat` (engine) and `errorconfig` (controller) intentionally duplicate the category list and validation rules, each with a keep-in-sync comment pointing at design §4.1. This follows the design's explicit requirement that the taxonomy must NOT be imported from `internal/analytics` — a guard test (`TestNoAnalyticsImport`) parses the engine package's imports to enforce it.

---

## 3. Source A — gateway policy errors (the core path)

### 3.1 Resolution and rendering (`errorformat/resolver.go`, `renderer.go`)

`Resolver.Resolve(status, accept, perAPI, placeholderValues)` is the single decision function:

1. **Entry lookup** — the per-API document resolves as a whole first (exact status key, then its own `default`), then the global document (exact, then `default`). Miss everywhere → `ok=false`, caller keeps the built-in body.
   - ⚠️ **Reviewed decision:** a per-API `default` **shadows** a global exact entry. The plan text was ambiguous; we chose per-layer resolution because an API author's catch-all is more specific to that API than any global rule. Locked in by `TestResolve_PerAPIDefaultShadowsGlobalExact`.
2. **Status override** — `x-status-code-override` replaces the returned status; placeholders reflect the *returned* status.
3. **Media type negotiation** — request `Accept` entries in order of appearance (q-values not weighted in v1), with `type/*` and `*/*` wildcards → configured `default_media_type` → the entry's first media type in sorted order (deterministic).
4. **Rendering** — injection-safe by construction:
   - a **string** example is the literal body; placeholder *values* are JSON-escaped or XML-escaped per media family before insertion;
   - a **structured** example (map/list) has placeholders substituted into its string leaves and is then `json.Marshal`ed — the marshaller escapes everything.

### 3.2 The choke-point function (`internal/kernel/error_format.go`)

`applyErrorFormat(immResp, execCtx)` guards (`status ≥ 400`, resolver present), pulls `Accept` + placeholder values (requestId, apiName, apiVersion) from the execution context, and on a match replaces **status, content-type, and body only** — all other headers (`WWW-Authenticate`, `Retry-After`, `x-error-id`, …) survive, so transport semantics keep working.

It is invoked at **all 8 existing choke points**, always **immediately before** `applySOAPFaultFormat`:

- `translator.go` ×6 — `translateRequestActionsCore`, `TranslateRequestHeaderActions`, `TranslateRequestHeaderActionsWithBodyMerge`, `TranslateResponseHeaderActions`, `TranslateResponseHeaderActionsWithBodyMerge`, `translateResponseActionsCore`
- `execution_context.go` — `handlePolicyError`
- `extproc.go` — the no-policy-chain path (no execution context exists there, so it calls the shared core `formatErrorResponse` with raw-header helpers)

**SOAP composition (design §4.5):** because the error formatter runs first, a customized JSON message gets wrapped into the SOAP fault (carried in `<detail>`); a customized **XML** body passes the SOAP formatter's existing "already-XML" guard untouched — that is how a user supplies a complete custom fault.

**Category hint:** a policy may set the internal header `x-wso2-error-category` on an error response to refine the category beyond the status-code default (no SDK change). The choke point consumes it and **always strips it** so it never reaches a client.

### 3.3 Wiring (who owns the resolver)

- `main.go` **always** installs a resolver on the Kernel: `NewResolver(globalDoc, defaultMediaType)` when `[error_handling]` is enabled, `NewResolver(nil, …)` when disabled. This is deliberate — **per-API customization works even with the global flag off** (the two flags are independent per the design). An invalid global file is a fail-fast startup error in the engine.
- `initializeExecutionContext` copies `kernel.ErrorFormatResolver()` and the route's parsed `ErrorResponses` onto the execution context. The published policy SDK (`SharedContext`) is untouched, per the plan's preference.

### 3.4 Per-API delivery (controller → engine)

```
spec.errorResponses (validated at deploy)
  → transformers serialize to JSON → models.Metadata.ErrorResponses   (pkg/transform)
  → "error_responses" key in the RouteConfig policy-xDS resource      (pkg/policyxds/snapshot.go)
  → engine parses ONCE at deploy time → RouteConfig.ErrorResponses    (internal/xdsclient/handler.go)
  → execCtx.perAPIErrorResponses at request initialization            (internal/kernel/extproc.go)
```

An invalid per-API document at the engine is logged and dropped — **the route still deploys** (the controller validated it at deploy time, so this is a defense-in-depth path, not a normal one).

---

## 4. Source C — Envoy local replies (global: Phase 2, per-API: Phase 4)

These errors never reach the engine, so they are compiled into the xDS snapshot by the controller (`pkg/xds/local_reply.go`).

### 4.1 Static rendering — a second rendering context

`errorconfig.ResolveStatic(status, defaultMediaType)` renders bodies **when the snapshot is built**, not per request:

- no `Accept` negotiation (the configured default media type, else the entry's first, sorted);
- only `${statusCode}`, `${message}`, `${category}` carry values — request-scoped placeholders render as **empty strings** (there is no request);
- Envoy's own `%COMMAND%` operators (e.g. `%RESPONSE_FLAGS%`) are a third, Envoy-side context.

This two-context split is documented for users in `gateway/configs/error-responses.yaml`.

### 4.2 The mappers — flags only, never status codes

`buildLocalReplyConfig` emits `local_reply_config` mappers on the HCM:

| Filter | Entry used | Category |
|---|---|---|
| response flags `UH, UF, UO` | `503` (or `default`) | BACKEND_UNAVAILABLE |
| response flag `UT` | `504` (or `default`) | BACKEND_TIMEOUT |

⚠️ **Reviewed decision — the most important safety property in Phase 2:** mappers match **response flags only**. ext_proc immediate responses (our already-formatted source-A errors, including per-API overrides) are *also* delivered through Envoy's local-reply machinery; a status-code mapper would clobber them with the global body. Upstream flags are only set for genuine upstream events, so flag matching cannot collide. Consequence: **413 (payload too large) is not customizable** in this slice — it has no dedicated response flag. Documented in the sample config.

Each mapper carries: the static body (`Body` + `BodyFormatOverride` with `%LOCAL_REPLY_BODY%` and the right content-type), a `StatusCode` rewrite when `x-status-code-override` differs, and the header `x-wso2-response-fault-flag: %RESPONSE_FLAGS%` — the **Phase 3 discriminator** (see §5). The engine-side constant `faultFlagHeaderName` mirrors `xds.FaultFlagHeaderName`; both carry keep-in-sync comments.

### 4.3 No-route 404

The hardcoded catch-all `DirectResponse` body `{"error":"Not Found"}` in `TranslateConfigs` is replaced by `noRouteResponse()`: the configured `404` (or `default`) entry's body/content-type/overridden status, falling back to the original byte-for-byte. No-route is inherently global — no API matched, so per-API cannot apply.

On the controller, a bad global file **logs an error and disables customization** rather than crashing xDS — the engine fails fast on the same file in the same deployment, which surfaces misconfiguration without taking route serving down.

### 4.4 Per-API local replies (Phase 4)

Per-API mappers are emitted **ahead of** the global ones — Envoy applies the first matching mapper, so ordering yields per-API > global, mirroring the engine.

Each per-API mapper = `AndFilter( response-flag filter, CEL filter )` where the CEL filter is Envoy's standard `envoy.access_loggers.extension_filters.cel` extension evaluating:

```
xds.route_name in ["GET|/calc/v1|…", "POST|/calc/v1|…"]
```

Route names are sorted and CEL-escaped for deterministic snapshots. `TranslateConfigs` collects each API's route names plus its `errorResponses` (type-switched from the stored config, bridged JSON → `errorconfig.Parse`) and passes them into `createListener` (signature gained a parameter).

⚠️ **Reviewed deviation from the plan:** the plan sketched "a route-injected header carrying the API id" as the mapper key. That was rejected: route-level `request_headers_to_add` are finalized in the router *after* the point where UH (no healthy upstream) local replies are generated — the header would unreliably exist for exactly the errors being customized — and it would leak an internal header to backends on every successful request. `xds.route_name` is always available (the route is matched before any upstream flag can be set) and leaks nothing. The CEL access-log filter is a default extension in stock `envoyproxy/envoy` images (which the gateway image derives from).

---

## 5. Source B — backend error reshaping (Phase 3)

Implemented as roughly 60 lines: `backendErrorImmediateResponse()` in `error_format.go` plus one hook in `processResponseHeaders`.

**Mechanism:** at the response-headers phase, after header policies run and only when no policy short-circuited, the kernel checks whether reshaping applies. If yes, it fabricates a short-circuit:

```go
execResult.ShortCircuited = true
execResult.FinalAction = imm   // backend status + copied response headers, no body
```

…and the **existing** choke point in `TranslateResponseHeaderActions` does everything else — `applyErrorFormat` renders with full request-time features, `applySOAPFaultFormat` composes faults, and analytics/dynamic-metadata plumbing is reused as-is. **No new rendering path exists for source B.** This is the main thing to appreciate in review: the diff is small because the synthesized action rides the Phase 1 rails.

**Opt-in rule (strict, per design "BACKEND_ERROR = passthrough"):** reshaping requires an **exact status entry in the API's own `errorResponses`**. The global file and even a per-API `default` entry never touch backend errors.

**Passthrough guards** (each one tested):
- Envoy local replies — identified by the `x-wso2-response-fault-flag` header (§4.2);
- streaming responses — the body is already flowing, a clean replacement is impossible (documented caveat);
- status < 400, policy short-circuits, nil resolver/per-API config.

**Header/body semantics:** the backend body is **discarded** (reshaping happens at the headers phase; masking backend internals is the point — the body is never even buffered). Backend headers are carried over minus pseudo-headers and body-framing headers (`content-length`, `content-encoding`, `transfer-encoding`, `connection`) since the replacement body has different framing. Response-body policies do not run for reshaped errors — the same behavior as any response-header short-circuit.

**Known, accepted conflation:** when global `[error_handling]` is disabled there are no local-reply mappers, so an *unmarked* Envoy 503 reshapes as if it were a backend 503 if the API opted in for 503. The user-visible result is still their configured body; full discrimination arrives if marking is ever extended to all local replies.

---

## 6. Precedence summary (one table to review against)

Per status code, first match wins:

| Priority | Source A (policy errors) | Source C (local replies) | Source B (backend) |
|---|---|---|---|
| 1 | per-API exact entry | per-API mapper (exact → per-API default) | per-API **exact** entry only |
| 2 | per-API `default` | global mapper (exact → global default) | — |
| 3 | global exact entry | built-in Envoy body | — |
| 4 | global `default` | | |
| 5 | built-in body | | passthrough (always the default) |

Independence properties: per-API works with global disabled (all sources); analytics on/off changes nothing (guard test + no imports); nothing configured ⇒ built-in behavior everywhere.

---

## 7. Things a reviewer should poke at

1. **The 8 choke points** — `grep -n applyErrorFormat gateway/gateway-runtime/policy-engine/internal/kernel/*.go`: every `applySOAPFaultFormat` call site must have `applyErrorFormat` immediately before it, and no new `ImmediateResponse` construction site may bypass the pair.
2. **Header hygiene** — `applyErrorFormat` and the SOAP formatter copy header maps before mutating (policy-owned maps must not be mutated in place); `takeHeader` exists for exactly this reason.
3. **Injection safety** — renderer tests (`TestRenderExample_*Escaping`) prove a hostile placeholder value cannot break out of a JSON string or XML text node. Check the escaping is applied to *values only*, never the template.
4. **Mapper ordering** — in `buildLocalReplyConfig`, per-API mappers must precede global ones; Envoy's first-match-wins is the only thing enforcing per-API > global for source C.
5. **The flag-only rule** — any future mapper added with a `StatusCodeFilter` would silently corrupt engine-formatted errors. The comment block on `localReplyMappings` explains this; treat it as load-bearing.
6. **Deploy-time parse, request-time read** — per-API docs are parsed once in the xDS handler; the request path does map lookups only (no per-request parsing/allocation).
7. **Generated code** — `generated.go` was regenerated with `oapi-codegen@v2.5.1` (not vendored); the embedded-spec base64 blob rewrite is expected diff noise.

## 8. Known limitations (all deliberate, all documented)

- `Accept` q-values are not weighted (order of appearance wins).
- 413 local replies keep the built-in body (no response flag to match).
- Request-scoped placeholders render empty in local-reply bodies (static context).
- `x-wso2-response-fault-flag` is visible to clients on customized local replies (it is also the Phase 3 discriminator; stripping it would require touching every response in the engine).
- Streaming (`FULL_DUPLEX_STREAMED`) errors are never customized (design §7).
- Per-API `errorResponses` exists on `RestApi` and `SoapApi` kinds; other kinds (WebSub, LLM, MCP) can adopt the same field later.
