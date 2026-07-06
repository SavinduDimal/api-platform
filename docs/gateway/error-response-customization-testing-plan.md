# Gateway Error Response Customization — Testing Plan

Step-by-step manual testing guide for the error-response customization feature
(design: `error-response-customization-design.md`, code walkthrough:
`error-response-customization-implementation.md`). Run each step in order; each
builds on the previous. Every scenario lists the exact config to deploy, the
request to send, and the expected response.

The feature customizes three error sources — keep this in mind when a test
"fails": each source is rendered in a different place.

| Source | Example errors | Rendered | Tested in |
|---|---|---|---|
| A — gateway policy errors | 401, 403, 429, policy 500 | policy engine, per request | Steps 2, 3 |
| B — backend errors | upstream's own 4xx/5xx | policy engine, per request, **opt-in** | Step 5 |
| C — Envoy local replies | 503, 504, no-route 404 | Envoy, rendered at deploy time | Steps 4, 6 |

---

## Prerequisites

```bash
# 1. Gateway up (dev compose: controller :9090, gateway :8080, Envoy admin :9901)
cd /Users/savindudimal/Desktop/APIM/Repo/api-platform/gateway
docker compose up -d

# 2. Local mock backend for backend-error tests (keep running in its own terminal).
#    /ok → 200, /boom → 500 with a "revealing" body + X-Backend-Trace header,
#    /slow → 200 after 10 s (for the optional 504 test).
python3 - <<'EOF'
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/boom"):
            code, body = 500, b'{"stack":"secret internal trace at db.go:42","dsn":"pg://user:pass@db"}'
        elif self.path.startswith("/slow"):
            time.sleep(10)
            code, body = 200, b'{"ok":"slow"}'
        else:
            code, body = 200, b'{"ok":true}'
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("X-Backend-Trace", "trace-123")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
HTTPServer(("", 3005), H).serve_forever()
EOF
# Verify: curl -s http://localhost:3005/ok → {"ok":true}
```

---

## Step 1 — Enable the feature and verify config loading

> **What to rebuild:** nothing yet if you are on images containing this feature;
> otherwise `make build` once (controller + runtime both changed).

### 1.1 Create the global error-responses file

Save as `configs/error-responses.test.yaml` (next to `configs/config.toml`):

```yaml
responses:
  "401":
    description: Authentication failure
    content:
      application/json:
        example: { code: 401, message: "Custom auth required", requestId: "${requestId}", api: "${apiName}" }
      application/xml:
        example: "<error><code>401</code><message>Custom auth required</message></error>"
  "404":
    content:
      application/json:
        example: { code: 404, message: "No API matched this request" }
  "503":
    content:
      application/json:
        example: { code: 503, message: "Backend unavailable (custom global)" }
  "504":
    x-status-code-override: 502
    content:
      application/json:
        example: { code: "${statusCode}", message: "Upstream timed out (custom global)" }
  default:
    content:
      application/json:
        example: { code: "${statusCode}", message: "${message}", category: "${category}", requestId: "${requestId}" }
```

### 1.2 Mount it and turn the feature on

The dev compose mounts `configs/config.toml` into **both** containers, so the
`config_file` path must resolve identically in both. Add one volume line to each
service in `docker-compose.yaml`:

```yaml
  gateway-controller:
    volumes:
      - ./configs/error-responses.test.yaml:/etc/gateway/error-responses.yaml:ro   # ADD
  gateway-runtime:
    volumes:
      - ./configs/error-responses.test.yaml:/etc/gateway/error-responses.yaml:ro   # ADD
```

In `configs/config.toml`:

```toml
[error_handling]
enabled = true
config_file = "/etc/gateway/error-responses.yaml"
default_media_type = "application/json"
```

```bash
docker compose up -d --force-recreate gateway-controller gateway-runtime
```

### 1.3 Engine loaded the config (expect a startup log line)

```bash
docker logs gateway-gateway-runtime-1 2>&1 | grep -i "error-response"
```

**Expected:** `Error-response customization enabled ... response_entries=5` listing
the status keys (`401 404 503 504 default`).

### 1.4 Invalid file fails fast on the engine, degrades on the controller

```bash
# Break the file (bad status key), recreate, check both containers
sed -i.bak 's/"401"/"9xx"/' configs/error-responses.test.yaml
docker compose up -d --force-recreate gateway-controller gateway-runtime
sleep 3
docker logs gateway-gateway-runtime-1    2>&1 | tail -3   # engine
docker logs gateway-gateway-controller-1 2>&1 | grep -i "error-response" | tail -2

# Restore before continuing!
mv configs/error-responses.test.yaml.bak configs/error-responses.test.yaml
docker compose up -d --force-recreate gateway-controller gateway-runtime
```

**Expected:** the engine exits with
`Failed to load error-response customization config ... invalid response key "9xx"`
(container restarts); the controller logs
`Failed to load error-response customization config; Envoy local replies keep built-in bodies`
but **keeps serving**.

---

## Step 2 — Global gateway policy errors (source A)

### 2.1 Deploy an auth-protected REST API (expect 201)

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin \
  -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-v1
spec:
  displayName: ErrDemo
  version: v1.0
  context: /errdemo/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3005
  policies:
    - name: api-key-auth
      version: v1
      params:
        key: X-API-Key
        in: header
  operations:
    - method: GET
      path: /ok
    - method: GET
      path: /boom
' | jq '.status.state'
```

**Expected:** `"deployed"`.

### 2.2 Unauthenticated request returns the customized 401

```bash
curl -si http://localhost:8080/errdemo/v1.0/ok
```

**Expected:**
- HTTP `401`
- `content-type: application/json`
- Body: `{"api":"ErrDemo","code":401,"message":"Custom auth required","requestId":"<uuid>"}`
  — NOT the built-in `{"error":"..."}`. `${requestId}` is a real UUID and
  `${apiName}` resolved to `ErrDemo`.

### 2.3 Accept header selects the XML variant

```bash
curl -si http://localhost:8080/errdemo/v1.0/ok -H 'Accept: application/xml'
```

**Expected:** HTTP `401`, `content-type: application/xml`,
body `<error><code>401</code><message>Custom auth required</message></error>`.

### 2.4 Unlisted error status falls back to the `default` entry (429)

Deploy a throttled variant and hit it twice:

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-throttle-v1
spec:
  displayName: ErrDemoThrottle
  version: v1.0
  context: /errdemo-throttle/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3005
  policies:
    - name: basic-ratelimit
      version: v1
      params:
        limits:
          - requests: 1
            duration: "1m"
  operations:
    - method: GET
      path: /ok
' | jq '.status.state'

for i in 1 2; do
  echo "--- Request $i ---"
  curl -si http://localhost:8080/errdemo-throttle/v1.0/ok | head -6
done
```

**Expected:** first request `200`; second request `429` with body
`{"category":"THROTTLED","code":"429","message":"Too Many Requests","requestId":"<uuid>"}` —
the `default` entry rendered with the substituted status, HTTP status text, and
category. Transport headers like `retry-after` (if the policy sets them) survive.

### 2.5 Successful calls are untouched

```bash
KEY=$(curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis/errdemo-v1/api-keys \
  -u admin:admin -H 'Content-Type: application/json' -d '{"name":"t1"}' | jq -r '.apiKey')

curl -si -H "X-API-Key: $KEY" http://localhost:8080/errdemo/v1.0/ok | head -6
```

**Expected:** HTTP `200`, body `{"ok":true}`, `x-backend-trace: trace-123` — the
customization only touches error responses.

---

## Step 3 — Per-API `errorResponses` (source A, per-API precedence)

### 3.1 Deploy an API with its own errorResponses (expect 201)

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-perapi-v1
spec:
  displayName: ErrDemoPerAPI
  version: v1.0
  context: /errdemo-perapi/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3005
  policies:
    - name: api-key-auth
      version: v1
      params:
        key: X-API-Key
        in: header
  operations:
    - method: GET
      path: /ok
  errorResponses:
    responses:
      "401":
        description: Custom auth message for this API only
        content:
          application/json:
            example: { error: "Provide a valid API key for ErrDemoPerAPI", hint: "see docs" }
' | jq '.status.state'
```

**Expected:** `"deployed"`.

### 3.2 Per-API 401 beats the global 401

```bash
curl -si http://localhost:8080/errdemo-perapi/v1.0/ok
```

**Expected:** HTTP `401`, body
`{"error":"Provide a valid API key for ErrDemoPerAPI","hint":"see docs"}` —
NOT the global "Custom auth required" body.

### 3.3 The other API still gets the global body (per-status isolation)

```bash
curl -s http://localhost:8080/errdemo/v1.0/ok | jq '.message'
```

**Expected:** `"Custom auth required"` — the per-API override on one API does not
affect others.

### 3.4 Per-API config is delivered via policy xDS (engine sees it at deploy)

```bash
docker logs gateway-gateway-runtime-1 2>&1 | grep -i "route config update" | tail -1
curl -s http://localhost:9002/routes 2>/dev/null | grep -c errdemo-perapi || true
```

**Expected:** a successful route-config update after 3.1 (no
`Ignoring invalid error_responses` warnings).

### 3.5 Validation rejects a bad errorResponses (expect 400)

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-bad
spec:
  displayName: ErrDemoBad
  version: v1.0
  context: /errdemo-bad/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3005
  operations:
    - method: GET
      path: /ok
  errorResponses:
    responses:
      "9xx":
        content:
          application/json:
            example: { error: "bad key" }
' | jq '.error.errors // .errors'
```

**Expected:** HTTP 400 with a validation error on
`spec.errorResponses.responses.9xx` ("must be a 3-digit status code (100-599) or
'default'"). Also try: an unknown placeholder `${bogus}` in an example, an
unsupported media type (`application/octet-stream`), and a media type with only
`schema` — each must be rejected with a matching field path.

### 3.6 Per-API `x-status-code-override`

Redeploy `errdemo-perapi-v1` (PUT) adding an entry that remaps 401 → 403:

```bash
curl -s -X PUT http://localhost:9090/api/management/v0.9/rest-apis/errdemo-perapi-v1 \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-perapi-v1
spec:
  displayName: ErrDemoPerAPI
  version: v1.0
  context: /errdemo-perapi/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3005
  policies:
    - name: api-key-auth
      version: v1
      params:
        key: X-API-Key
        in: header
  operations:
    - method: GET
      path: /ok
  errorResponses:
    responses:
      "401":
        x-status-code-override: 403
        content:
          application/json:
            example: { error: "Denied", effectiveStatus: "${statusCode}" }
' | jq '.status.state'

curl -si http://localhost:8080/errdemo-perapi/v1.0/ok | head -4
```

**Expected:** HTTP **403** (not 401), body
`{"effectiveStatus":"403","error":"Denied"}` — the `${statusCode}` placeholder
reflects the **returned** status.

---

## Step 4 — Envoy local replies + no-route 404 (source C, global)

These bodies are rendered **statically when the xDS snapshot is built** — no
`Accept` negotiation, and `${requestId}`-style placeholders render empty (see the
comments in `configs/error-responses.yaml`).

### 4.1 Envoy received the local_reply_config

```bash
curl -s http://localhost:9901/config_dump | grep -A 3 '"UH"' | head -8
curl -s http://localhost:9901/config_dump | grep -c 'x-wso2-response-fault-flag'
```

**Expected:** a response-flag mapper listing `UH`, `UF`, `UO`, and ≥1 occurrence of
the fault-flag header (one per mapper, per listener).

### 4.2 No-route request returns the customized 404

```bash
curl -si http://localhost:8080/no/such/api
```

**Expected:** HTTP `404`, `content-type: application/json`,
body `{"code":404,"message":"No API matched this request"}` —
NOT the built-in `{"error":"Not Found"}`.

### 4.3 Unreachable backend returns the customized 503 + fault-flag header

```bash
# Deploy an API pointing at a dead port (nothing listens on 3999)
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-dead-v1
spec:
  displayName: ErrDemoDead
  version: v1.0
  context: /errdemo-dead/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3999
  operations:
    - method: GET
      path: /ok
' | jq '.status.state'

curl -si http://localhost:8080/errdemo-dead/v1.0/ok
```

**Expected:**
- HTTP `503`
- Body `{"code":503,"message":"Backend unavailable (custom global)"}` — NOT Envoy's
  default `upstream connect error or disconnect/reset...` text
- Header `x-wso2-response-fault-flag: UF` (connection refused; `UH` if no healthy host)

### 4.4 Upstream timeout returns 502 via `x-status-code-override` (optional)

The default route timeout is 60 s; temporarily lower it in `configs/config.toml`
so the mock's `/slow` (10 s) trips it:

```toml
[router.upstream.timeouts]
route_timeout_ms = 3000
```

```bash
docker compose up -d --force-recreate gateway-controller
# Add "- method: GET / path: /slow" to errdemo-v1's operations (PUT), then:
curl -si -H "X-API-Key: $KEY" http://localhost:8080/errdemo/v1.0/slow
```

**Expected:** HTTP **502** (Envoy generated 504; the mapper's status rewrite applied),
body `{"code":"502","message":"Upstream timed out (custom global)"}`,
`x-wso2-response-fault-flag: UT`. Restore `route_timeout_ms` afterwards.

### 4.5 Gateway policy errors are NOT clobbered by the mappers

Re-run 2.2. **Expected:** still the engine-rendered 401 body with a real requestId —
proving the local-reply mappers (flag-filtered) do not rewrite ext_proc immediate
responses. This is the key safety property of the flag-only mapper design.

> **Known limitation:** 413 (payload too large) has no Envoy response flag and keeps
> its built-in body — expected, documented, not a bug.

---

## Step 5 — Backend error reshaping (source B, per-API opt-in)

### 5.1 Backend 500 passes through by default (global config alone must NOT reshape)

```bash
curl -si -H "X-API-Key: $KEY" http://localhost:8080/errdemo/v1.0/boom
```

**Expected:** HTTP `500` and the backend's own **revealing body**
(`{"stack":"secret internal trace at db.go:42",...}`) plus `x-backend-trace: trace-123` —
untouched, even though the global file has a `default` entry. Backend reshaping is
strictly per-API opt-in.

### 5.2 Opt in with an exact per-API entry, backend body is masked

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-reshape-v1
spec:
  displayName: ErrDemoReshape
  version: v1.0
  context: /errdemo-reshape/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3005
  operations:
    - method: GET
      path: /boom
  errorResponses:
    responses:
      "500":
        description: Mask backend internals
        content:
          application/json:
            example: { code: 500, message: "Something went wrong on our side", api: "${apiName}" }
' | jq '.status.state'

curl -si http://localhost:8080/errdemo-reshape/v1.0/boom
```

**Expected:**
- HTTP `500`
- Body `{"api":"ErrDemoReshape","code":500,"message":"Something went wrong on our side"}`
  — the backend's stack trace and DSN are **gone**
- `x-backend-trace: trace-123` still present (non-framing backend headers carry over);
  no stale `content-length` mismatch (Envoy reframes)

### 5.3 Only the exact status is opted in

```bash
# The mock returns 200 on /ok — add it and verify success is untouched;
# a backend status other than 500 (e.g. point an operation at a 404 path
# of a real backend) must also pass through unchanged.
curl -s http://localhost:8080/errdemo-reshape/v1.0/boom -o /dev/null -w "%{http_code}\n"
```

**Expected:** only backend `500`s on this API are reshaped; everything else — other
statuses, other APIs, success responses — passes through.

### 5.4 Envoy local replies are not mistaken for backend errors

Point `errdemo-reshape-v1`'s upstream at the dead port `3999` (PUT, keep the
`errorResponses` block) and call it.

**Expected:** the **global 503 local-reply body** from 4.3 with the
`x-wso2-response-fault-flag` header — NOT the per-API 500 mask. The flag header is
the discriminator between source B and source C.

---

## Step 6 — Per-API Envoy local replies (source C, per-API)

### 6.1 Deploy an API with its own 503 entry, upstream dead

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: errdemo-perapi503-v1
spec:
  displayName: ErrDemoPerAPI503
  version: v1.0
  context: /errdemo-perapi503/v1.0
  upstream:
    main:
      url: http://host.docker.internal:3999
  operations:
    - method: GET
      path: /ok
  errorResponses:
    responses:
      "503":
        content:
          application/json:
            example: { error: "ErrDemoPerAPI503 backend is down, try later" }
' | jq '.status.state'
```

### 6.2 Envoy got a route-scoped (CEL) mapper, ordered before the global one

```bash
curl -s http://localhost:9901/config_dump | grep -o 'xds.route_name in [^"]*' | head -2
```

**Expected:** a CEL expression like
`xds.route_name in [\"GET|/errdemo-perapi503/v1.0/ok|...\"]` — the per-API mapper,
which appears **before** the plain flag-only global mappers in the
`local_reply_config`.

### 6.3 The two APIs get different 503 bodies

```bash
echo "--- per-API 503 ---"
curl -s http://localhost:8080/errdemo-perapi503/v1.0/ok
echo; echo "--- global 503 (from 4.3) ---"
curl -s http://localhost:8080/errdemo-dead/v1.0/ok
```

**Expected:**
- `{"error":"ErrDemoPerAPI503 backend is down, try later"}` for the first
- `{"code":503,"message":"Backend unavailable (custom global)"}` for the second —
  per-API beats global, scoped exactly to the API's routes.

---

## Step 7 — SOAP composition

### 7.1 Customized message is wrapped into a SOAP Fault

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: errdemo-soap-v1
spec:
  displayName: ErrDemoSoap
  version: v1.0
  context: /errdemo-soap/v1
  upstream:
    main:
      url: http://host.docker.internal:3005/ok
  policies:
    - name: api-key-auth
      version: v1
      params:
        key: X-API-Key
        in: header
  errorResponses:
    responses:
      "401":
        content:
          application/json:
            example: { error: "SOAP callers must present an API key" }
' | jq '.status.state'

# Unauthenticated SOAP 1.1 call
curl -si -X POST http://localhost:8080/errdemo-soap/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body><Ping/></soap:Body>
</soap:Envelope>'
```

**Expected:**
- HTTP `401`, `Content-Type: text/xml; charset=utf-8` (NOT JSON)
- Body is a `<soap:Fault>` with `<faultcode>soap:Client</faultcode>` whose
  `<detail>` carries the customized JSON message
  (`SOAP callers must present an API key`) — the error formatter ran first, the
  SOAP formatter wrapped its output (design §4.5).

### 7.2 An XML example becomes the fault verbatim (already-XML guard)

Redeploy `errdemo-soap-v1` (PUT) with an XML media type instead:

```yaml
  errorResponses:
    responses:
      "401":
        content:
          application/xml:
            example: "<myns:CustomFault xmlns:myns=\"urn:err\"><reason>key missing</reason></myns:CustomFault>"
```

Repeat the unauthenticated call **with** `-H 'Accept: application/xml'`.

**Expected:** HTTP `401`, body is exactly `<myns:CustomFault ...>` — the SOAP
formatter's already-XML guard let the user-supplied XML through without wrapping.
This is how a full custom fault is supplied.

---

## Step 8 — Analytics independence (design-review condition)

With `[analytics] enabled = false` in `configs/config.toml` (the default), re-run
2.2, 2.4, and 5.2.

**Expected:** identical results — the taxonomy and choke points never touch
analytics state. (The engine-side guard test `TestNoAnalyticsImport` enforces the
no-import rule at build time.)

---

## Step 9 — Kill switch / backward-compat regression

```bash
# Disable globally and remove per-API overrides
#   config.toml: [error_handling] enabled = false
docker compose up -d --force-recreate gateway-controller gateway-runtime

curl -si http://localhost:8080/no/such/api | grep -E "HTTP|error"
curl -si http://localhost:8080/errdemo/v1.0/ok | head -4
curl -si http://localhost:8080/errdemo-dead/v1.0/ok | head -4
```

**Expected (byte-for-byte today's behavior):**
- no-route: `{"error":"Not Found"}`, `content-type: application/json`
- 401: the built-in policy body (`{"error":...}`)
- dead backend: Envoy's default plain-text 503, **no** `x-wso2-response-fault-flag`
- BUT: APIs that still carry `spec.errorResponses` keep their per-API
  customization — the per-API and global switches are independent by design
  (verify with 3.2 if `errdemo-perapi-v1` is still deployed).

Re-enable `[error_handling]` afterwards if you continue testing.

---

## Automated test suites (already implemented — run before/after any change)

```bash
# Engine: taxonomy/parse/resolve/render, all 8 choke points, SOAP composition,
# backend reshaping, per-API xDS delivery, config
cd gateway/gateway-runtime/policy-engine && go test ./...

# Controller: validation, transformers, policy-xDS resource, local-reply golden
# tests (incl. per-API CEL mappers), no-route 404, config
cd gateway/gateway-controller && go test ./...
```

Key suites: `internal/kernel/errorformat` (resolution/negotiation/escaping),
`internal/kernel` `TestChokePoint_*` / `TestBackendReshape_*` /
`TestApplyErrorFormat_*`, `pkg/errorconfig`, `pkg/config`
`TestValidateErrorResponses_*`, `pkg/xds` `TestBuildLocalReplyConfig_*` /
`TestTranslateConfigs_NoRouteDirectResponse` / `TestTranslateConfigs_PerAPILocalReplies`.
The **pre-existing** kernel suites double as the regression gate: they assert
today's exact error bodies and must stay green with customization disabled.

---

## Integration tests (godog) — outstanding

New feature file `gateway/it/features/error-response-customization.feature`
mirroring Steps 2–7 (the existing `api-error-responses.feature` covers management-API
*validation* errors — unrelated). Because enabling `[error_handling]` changes the
404/503 bodies gateway-wide, run these scenarios with a dedicated compose file
(pattern: `docker-compose.test.vhosts-*.yaml`) so existing features that assert on
`{"error":"Not Found"}` keep passing:

```bash
cd gateway/it
COMPOSE_FILE=docker-compose.test.error-customization.yaml \
  go test -v -timeout 30m ./... -godog.tags="@error-customization"
```

Only this suite can prove on a **live Envoy**: that the generated
`local_reply_config` (including the CEL `xds.route_name` filter) is accepted, that
per-API mappers discriminate between two APIs, that ext_proc immediate responses
are not rewritten by the mappers, and that the fault-flag header round-trips into
the engine's response phase.

---

## Postman collection

For an interactive/repeatable alternative to the curl steps above, import
[`error-response-customization.postman_collection.json`](error-response-customization.postman_collection.json)
into Postman (or run it with `newman`). It mirrors Steps 2–7 with assertions —
deploys the `errdemo-*` APIs, saves an API key into a collection variable, exercises
all three error sources (including per-API precedence, backend masking, the
fault-flag header, and SOAP composition), and cleans up after itself:

```bash
# Headless run (requires Step 1 setup: [error_handling] enabled with the test
# error-responses file mounted, plus the mock backend on :3005)
newman run docs/gateway/error-response-customization.postman_collection.json
```

Collection variables (`mgmtUrl`, `gwUrl`, `adminUser`, `adminPass`) default to the
local docker-compose setup. The assertions match the bodies in Step 1.1's
`error-responses.test.yaml` verbatim — if you edit that file, update the collection.
Note: the throttle scenario is time-sensitive (1 req/min); the warm-up request
tolerates a 429 when the collection is re-run inside the window.

---

## Cleanup

```bash
for api in errdemo-v1 errdemo-throttle-v1 errdemo-perapi-v1 errdemo-dead-v1 \
           errdemo-reshape-v1 errdemo-perapi503-v1 errdemo-bad; do
  curl -s -X DELETE http://localhost:9090/api/management/v0.9/rest-apis/$api \
    -u admin:admin | jq -r '.status // empty'
done
curl -s -X DELETE http://localhost:9090/api/management/v0.9/soap-apis/errdemo-soap-v1 \
  -u admin:admin | jq -r '.status // empty'

# Revert config.toml ([error_handling] enabled=false or remove the section),
# remove the error-responses.test.yaml volume mounts, restore route_timeout_ms,
# and stop the python mock backend.
```
