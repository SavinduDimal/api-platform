# Gateway Error Response Customization — Implementation Plan

**Companion to:** `docs/gateway/error-response-customization-design.md` (read that first — this plan assumes its taxonomy, OpenAPI config model, and A/B/C error-source split).
**Status:** Ready to implement on approval. **Global configuration is the first slice.**
**Date:** 2026-06-23

---

## Guiding decisions (from design review)

- Config surface = **OpenAPI Responses Object** keyed by HTTP status code (+ `default`), with `content` media-type objects and `example` rendering. See design §4.2.
- Error-category taxonomy is **independent of analytics** — own package, no import from `internal/analytics`, enforced at points that run with analytics off. See design §4.1.1.
- Build order: **Phase 0 (schema, no behavior change) → Phase 1 global → Phase 1 per-API → Phase 2 (Envoy) → Phase 3 (backend) → Phase 4 (per-API Envoy)**.
- Backward compatible throughout: nothing configured ⇒ byte-for-byte identical to today.

## Legend

- **[C]** gateway-controller · **[E]** policy-engine · **[SDK]** `sdk/core/policy` · **[CFG]** config/docs
- Each task lists concrete files. Every phase ends with a build+test gate and a demoable outcome.

---

## Phase 0 — Taxonomy + OpenAPI config schema (no behavior change)

Goal: land all the types, schema, parsing, and validation with **zero runtime behavior change**. Nothing reads the config yet.

### 0.1 Error-category taxonomy (independent package)  [E][C]
- **[E]** New package `gateway/gateway-runtime/policy-engine/internal/kernel/errorformat/category.go`:
  - `type Category string` + constants: `AuthenticationFailure`, `AuthorizationFailure`, `Throttled`, `RequestMalformed`, `RouteNotFound`, `InternalError`, `BackendUnavailable`, `BackendTimeout`, `PayloadTooLarge`, `BackendError`.
  - `func CategoryForStatus(code int) Category` — default classification by status.
  - `func (Category) DefaultStatus() int`.
  - `func (Category) AnalyticsFaultCategory() string` — pure mapping (returns `"TARGET_CONNECTIVITY"`/`"OTHER"` **string literals**, NOT importing `internal/analytics`) for optional correlation.
- **[C]** Mirror the category constants where the controller needs them for validation (`gateway-controller/pkg/constants/` or a small `errorconfig` package). Keep the two lists in sync (single doc table is the source of truth).
- Tests: `category_test.go` — status↔category round-trips; assert the package has **no** import of `internal/analytics` (guard test / lint).

### 0.2 OpenAPI error-response model + parser  [E][C]
- Shared in-memory model representing the OpenAPI Responses subset:
  ```
  ErrorResponses { Responses map[string]ResponseObject }   // key: "401".."5xx" | "default"
  ResponseObject { Description string; StatusOverride *int (x-status-code-override); Content map[string]MediaType }
  MediaType      { Schema any (descriptive); Example any; Examples map[string]any }
  ```
  - **[E]** `internal/kernel/errorformat/model.go` + `parse.go` (YAML → model, validation).
  - **[C]** equivalent parse/validate for the per-API path (can reuse the generated OpenAPI types — see 0.4).
- Validation rules (shared): keys are 3-digit status in 100–599 or `default`; `x-status-code-override` in same range; at least one `content` media type; known media types; `example`/body size ≤ cap (e.g. 16 KB); placeholder names whitelisted.
- Tests: valid/invalid documents; size cap; unknown placeholder rejected.

### 0.3 Global config wiring (parse only, not applied)  [CFG][E][C]
- **[CFG]** Add to `gateway/configs/config-template.toml`:
  ```toml
  [error_handling]
    enabled = false
    config_file = "conf/error-responses.yaml"
    default_media_type = "application/json"
  ```
- **[E]** Add `ErrorHandling` struct to `internal/config/config.go` (koanf); on load, if `enabled`, read+parse `config_file` into the 0.2 model; log a summary; **do not apply**.
- **[C]** Add matching struct to `gateway-controller/pkg/config/config.go` (needed later for Phase 2 Envoy injection; parse now, unused).
- Ship a sample `conf/error-responses.yaml` (commented, disabled by default) mirroring design §4.2.
- Tests: engine boots with `enabled=false` (no-op) and with a valid file (parsed, still no-op); invalid file → clear startup error.

### 0.4 Per-API schema (OpenAPI) + validation  [C]
- **[C]** Add optional `errorResponses` to `SoapAPIData` **and** `RestAPIData` (and other kinds as feasible) in `gateway-controller/api/management-openapi.yaml`, shaped as the Responses Object subset (reuse/reference existing OpenAPI response/mediatype schemas where possible).
- Regenerate `generated.go` via `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.1 --config=oapi-codegen.yaml api/management-openapi.yaml`.
- Add validation in `pkg/config/api_validator.go` (+ `policy_validator.go` if needed): same rules as 0.2.
- Tests: `api_validator_test.go` — valid/invalid `errorResponses`; deploy round-trip stores + returns it.

### 0.5 Phase 0 gate
- `go build ./...` clean (controller, engine); all suites green; **no behavior change** verifiable by an unchanged-golden test on existing error responses.

---

## Phase 1 — Gateway policy errors (source A): global first, then per-API

Goal: gateway-generated `ImmediateResponse` errors (auth/throttle/no-route-chain/malformed/policy-exec) are shaped by the **global** config, then by **per-API** overrides. Highest value; self-contained in the engine + one controller metadata field.

### 1A — GLOBAL (built first)

#### 1A.1 Resolver + renderer  [E]
- `internal/kernel/errorformat/resolver.go`:
  - `type Resolver struct { global *ErrorResponses; defaultMediaType string }` built once at engine start from Phase 0.3.
  - `Resolve(status int, categoryHint Category, accept string) (statusOut int, contentType string, body []byte, matched bool)`:
    1. pick response entry: exact status key → `default` → not matched (caller keeps built-in body).
    2. apply `x-status-code-override` if present.
    3. negotiate media type from `Accept` → `default_media_type` → first.
    4. render `example` with placeholder substitution (`renderer.go`), escaped per media type, size-capped.
- `renderer.go`: whitelist placeholders (`${statusCode}`, `${message}`, `${errorCode}`, `${category}`, `${requestId}`, `${apiName}`, `${apiVersion}`); JSON- vs XML-escape.
- Tests: negotiation matrix, override, `default` fallback, unmatched passthrough, escaping.

#### 1A.2 `applyErrorFormat` choke-point function  [E]
- `internal/kernel/error_format.go` (kernel-side glue; can live beside `soap_fault.go`):
  ```
  func applyErrorFormat(immResp policy.ImmediateResponse, execCtx *PolicyExecutionContext) policy.ImmediateResponse
  ```
  - guard: only when `StatusCode >= 400`; skip if a policy already set a non-empty custom body with an explicit content-type it wants preserved (mirror the SOAP "already-XML" guard philosophy — decide precedence: config override wins unless body flagged custom).
  - derive `categoryHint` from status (+ optional `x-wso2-error-category` header/DynamicMetadata if a policy set one — additive, no SDK change).
  - pull request `Accept` + placeholder values (requestId, apiName, apiVersion from `execCtx.sharedCtx`).
  - call `Resolver.Resolve`; on match, replace status/headers(content-type)/body.
- Wire the resolver into the kernel (`Kernel` struct or execution context) so `applyErrorFormat` can reach it.

#### 1A.3 Invoke at the 8 choke points + SOAP composition  [E]
- Call `applyErrorFormat(...)` **immediately before** each existing `applySOAPFaultFormat(...)` call:
  - `internal/kernel/translator.go` (×6: ~67, ~363, ~525, ~748, ~850, ~1089)
  - `internal/kernel/execution_context.go:179` (`handlePolicyError`)
  - `internal/kernel/extproc.go:176` (no policy chain) — note: also has the raw pre-context path; handle via `rawRequestContentType` similarly.
- Composition (design §4.5): `applyErrorFormat` first resolves status+message/body; then `applySOAPFaultFormat` wraps into a SOAP Fault for SOAP APIs when the resolved body isn't already XML. Verify the SOAP guard still lets a user supply a full XML fault via config.
- Tests: table-driven kernel tests per choke point asserting the customized global body; SOAP API composition test; **analytics-disabled test** (config applied with `[analytics] enabled=false` and no analytics policy in chain).

#### 1A.4 Phase 1A gate (demoable)
- With a global `error-responses.yaml`, hitting an unauthenticated route returns the **customized 401 body**; an unknown route chain 500 returns the customized 500; SOAP API returns the customized message wrapped as a Fault. Analytics off changes nothing.

### 1B — PER-API (layered on 1A)

#### 1B.1 Deliver per-API config via policy-xDS metadata  [C][E]
- **[C]** In `gateway-controller/pkg/policyxds/snapshot.go` `createRouteConfigResource`, add the API's `errorResponses` (serialized OpenAPI subset) into the route metadata map.
  - Thread it from the stored config → transformers (`pkg/transform/*.go`) → `rdc.Metadata` (add field to `models.Metadata`/RDC) → snapshot.
- **[E]** Extend `RouteMetadata` (`internal/kernel/extproc.go`) and `SharedContext` (`sdk/core/policy/v1alpha2/context.go`) with the parsed per-API `ErrorResponses` (parse the metadata string once at deploy/route-config build, cache on `RouteConfig`).
  - **[SDK]** adding a field to `SharedContext` is an additive SDK change — verify no breakage; alternatively cache on the engine-side `RouteConfig` only and pass into `applyErrorFormat` via `execCtx` to avoid touching the SDK. **Prefer the engine-side `RouteConfig` approach** to keep the published SDK untouched.

#### 1B.2 Precedence in resolver  [E]
- `Resolve` gains an optional per-API `*ErrorResponses` argument checked **before** global, before `default`, before built-in. Per-status resolution (per-API `401` override doesn't shadow global `429`).
- Tests: precedence matrix (per-API > global > default > built-in); partial per-API override; per-API `x-status-code-override`.

#### 1B.3 Phase 1B gate (demoable)
- Two APIs with different `errorResponses` return different 401 bodies; an API without `errorResponses` falls back to global; SOAP composition intact.

---

## Phase 2 — Envoy local replies, global (source C)

Goal: customize Envoy-generated errors (503/504/413/426) and the no-route 404, which never reach the engine.

- **[C]** Build a `LocalReplyConfig` on the HCM in `pkg/xds/translator.go createListener()` from the controller's Phase 0.3 config: response-flag → status/body mappers, `body_format` from the OpenAPI `example` for the matching status (`UH`/`UF`→503 entry, `UT`→504 entry, etc.). Parametrize the commented template in `policy-engine/configs/envoy.yaml:39-55`.
- **[C]** Replace the hardcoded no-route `DirectResponse` body at `translator.go:490-519` with the configured `404`/`ROUTE_NOT_FOUND` example (fallback to `{"error":"Not Found"}`).
- Tests: xDS golden test for the generated HCM `local_reply_config`; IT test forcing 503 (no upstream), 504 (timeout), 413, and 404.
- Note: local-reply bodies are static Envoy substitutions (`%RESPONSE_CODE%`, `%LOCAL_REPLY_BODY%`), not the engine's placeholder set — document the two rendering contexts.

---

## Phase 3 — Backend error reshaping (source B)

Goal: optionally reshape backend 4xx/5xx responses.

- Prereq: distinguish backend responses from Envoy local replies — add `x-wso2-response-fault-flag: %RESPONSE_FLAGS%` (empty flag ⇒ genuine backend) via the Phase 2 `local_reply_config`/access path, surfaced to the engine response phase.
- **[E]** In the response-header/body phase, when the API opts in (`errorResponses` has an entry for the backend status and reshaping is enabled), rewrite the backend error using the resolver. Passthrough otherwise.
- Tests: backend 500-with-body → configured shape; not-configured → passthrough; streaming caveat documented.

---

## Phase 4 — Per-API Envoy local replies (stretch)

- Per-API customization of Envoy-generated errors via response-map filters keyed on a route-injected header carrying the API id. Ship only on demand. High complexity.

---

## Cross-cutting: testing & rollout

- **Unit:** taxonomy, model/parse/validate, resolver negotiation+precedence, renderer escaping.
- **Kernel:** each of the 8 choke points; SOAP composition; analytics-disabled independence.
- **Controller:** validator tests; deploy round-trip of `errorResponses`; policy-xDS metadata golden; Envoy HCM golden (Phase 2).
- **IT (godog):** end-to-end 401/429/404/503/504 with custom bodies for REST and SOAP; a `[analytics] enabled=false` scenario.
- **Rollout:** feature-flagged by `error_handling.enabled` (global) and presence of `errorResponses` (per-API). Default off ⇒ no change. Rebuild note: engine changes need a policy-engine image rebuild; Envoy changes take effect via xDS.

## Regeneration / tooling reminders

- `oapi-codegen` is **not** vendored — regenerate with `@v2.5.1` (see design §9 / repo memory).
- `management-openapi.yaml` uses `embedded-spec: true`; any schema change rewrites the base64 blob (large but expected diff).

## Open (non-blocking) items to finalize during Phase 0

- Final placeholder set + body-size cap value.
- Whether `examples` (plural) is supported in v1 or deferred to `example` only.
- SDK vs engine-only delivery of per-API config (plan recommends **engine-side `RouteConfig`**, no SDK change).
- Exact "policy already crafted a custom body" precedence vs a config override.
