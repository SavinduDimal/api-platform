# SOAP API Testing Plan

Step-by-step testing guide for each implementation step of Phase 1 SOAP API support
(passthrough, SOAP-to-SOAP). Run each section in order; each step builds on the previous.

---

## Prerequisites

```bash
# Start the local SOAP calculator backend (keep this running throughout all tests)
cd /Users/savindudimal/Desktop/APIM/RnD/api-platform-soap-apis/soap-backend
npm start
# Verify: http://localhost:3002/health → {"status":"ok"}
```

---

## Step 1 — CRUD Endpoints

> **What to rebuild:** `gateway-controller` only.
>
> ```bash
> cd /Users/savindudimal/Desktop/APIM/Repo/api-platform/gateway
> make build-controller
> docker compose up -d --force-recreate gateway-controller
> ```

### 1.1 Create a SOAP API (expect 201)

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin \
  -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: calculator-v1
spec:
  displayName: Calculator SOAP API
  version: v1.0
  context: /calculator/v1
  soapVersion: "1.1"
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
  operations:
    - name: Add
      soapAction: "Add"
    - name: Echo
      soapAction: "Echo"
' | jq .
```

**Expected:** HTTP 201, response body contains `"kind":"SoapApi"`, `"status":"deployed"`.

---

### 1.2 List (expect count 1)

```bash
curl -s http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin | jq '{count, name: .soapApis[0].metadata.name}'
```

**Expected:** `{"count":1,"name":"calculator-v1"}`.

---

### 1.3 Get by ID (expect 200 with status block)

```bash
curl -s http://localhost:9090/api/management/v0.9/soap-apis/calculator-v1 \
  -u admin:admin | jq '.status'
```

**Expected:** `state`, `createdAt`, `updatedAt` fields present.

---

### 1.4 Validation rejects bad config (expect 400)

```bash
# Missing upstream URL
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin \
  -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: bad-api
spec:
  displayName: Bad API
  version: v1.0
  context: /bad
  upstream:
    main:
      url: ""
' | jq '.errors'
```

**Expected:** HTTP 400, `errors` array listing the upstream URL validation failure.

---

### 1.5 Duplicate name rejected (expect 409)

```bash
# Re-POST the same calculator-v1
curl -s -o /dev/null -w "%{http_code}" \
  -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin \
  -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: calculator-v1
spec:
  displayName: Calculator SOAP API
  version: v1.0
  context: /calculator/v1
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
'
```

**Expected:** `409`.

---

### 1.6 Duplicate soapAction rejected (expect 400)

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin \
  -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: dup-api
spec:
  displayName: Dup API
  version: v1.0
  context: /dup
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
  operations:
    - name: Op1
      soapAction: "urn:same"
    - name: Op2
      soapAction: "urn:same"
' | jq '.errors'
```

**Expected:** HTTP 400, error mentioning duplicate soapAction.

---

### 1.7 API key management

```bash
# Create
curl -s -X POST \
  http://localhost:9090/api/management/v0.9/soap-apis/calculator-v1/api-keys \
  -u admin:admin \
  -H 'Content-Type: application/json' \
  -d '{"name":"test-key"}' | jq '.apiKey' | cut -c1-10

# List
curl -s http://localhost:9090/api/management/v0.9/soap-apis/calculator-v1/api-keys \
  -u admin:admin | jq '[.apiKeys[].name]'
```

**Expected:** API key string returned on create; name `test-key` in list.

---

### 1.8 Confirm Envoy does NOT yet have calculator routes

```bash
curl -s http://localhost:9901/config_dump | grep -c calculator
```

**Expected:** `0` — no routes yet (xDS routing is Step 2).

---

### 1.9 Delete and confirm gone

```bash
curl -s -X DELETE http://localhost:9090/api/management/v0.9/soap-apis/calculator-v1 \
  -u admin:admin | jq '.status'

curl -s -o /dev/null -w "%{http_code}" \
  http://localhost:9090/api/management/v0.9/soap-apis/calculator-v1 \
  -u admin:admin
```

**Expected:** `"success"` then `404`.

---

## Step 2 — Envoy Route Translation (SOAP Passthrough)

> **What to rebuild:** `gateway-controller` only (no change to `gateway-runtime`).
>
> ```bash
> cd /Users/savindudimal/Desktop/APIM/Repo/api-platform/gateway
> make build-controller
> docker compose up -d --force-recreate gateway-controller
> ```

Re-deploy the calculator API before running these tests.

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-binary @examples/soap-api-calculator.yaml | jq .status
```

---

### 2.1 Envoy routes appear

```bash
curl -s http://localhost:9901/config_dump | grep -E "POST\|/calculator|GET\|/calculator" | head
```

**Expected:** two route names visible — one for `POST` and one for `GET`.

---

### 2.2 Cluster created

```bash
curl -s http://localhost:9901/config_dump | grep host_docker_internal
```

**Expected:** cluster entry for `host.docker.internal:3002`.

---

### 2.3 SOAP 1.1 Add invocation (core passthrough)

```bash
curl -s -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Add' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:AddRequest><a>3</a><b>4</b></tns:AddRequest></soap:Body>
</soap:Envelope>'
```

**Expected:** HTTP 200, `<tns:AddResponse><result>7</result></tns:AddResponse>`.

---

### 2.4 Echo operation

```bash
curl -s -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Echo' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:EchoRequest><message>hello via gateway</message></tns:EchoRequest></soap:Body>
</soap:Envelope>'
```

**Expected:** HTTP 200, `<tns:EchoResponse><message>hello via gateway</message></tns:EchoResponse>`.

---

### 2.5 Backend receives the request intact

```bash
curl -s http://localhost:3002/calculator/v1/debug/last-request \
  | jq '{path, soapAction, contentType, operation}'
```

**Expected:**
```json
{
  "path": "/calculator/v1/soap11/",
  "soapAction": "Add",
  "contentType": "text/xml; charset=utf-8",
  "operation": "AddRequest"
}
```

`soapAction` and `Content-Type` are forwarded unchanged. Path is the rewritten upstream path.

---

### 2.6 WSDL passthrough (GET route)

```bash
curl -s -o /dev/null -w "%{http_code}" 'http://localhost:8080/calculator/v1?wsdl'
```

**Expected:** `404` from the backend (the calculator backend serves WSDL at the root path
`/calculator/v1?wsdl`, but the gateway rewrites to `/soap11?wsdl` — the backend does not
handle that path). This confirms the GET route reaches the backend; a real SOAP service
that serves WSDL on the same path as its endpoint would return `200 text/xml`.

> **Note:** To confirm the gateway itself is reaching the backend and not returning a
> gateway-level 404, check:
> ```bash
> curl -s 'http://localhost:8080/calculator/v1?wsdl' -v 2>&1 | grep "< HTTP"
> ```
> A `404` from the backend is distinct from a `404` generated by the gateway (which would
> carry `content-type: application/json` and `{"error":"Not Found"}`).

---

### 2.7 Backend SOAP fault passes through

```bash
curl -s -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: TriggerFault' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body>
    <tns:TriggerFaultRequest>
      <faultString>intended failure</faultString>
      <httpStatus>500</httpStatus>
    </tns:TriggerFaultRequest>
  </soap:Body>
</soap:Envelope>' | grep -o '<soap:Fault>.*</soap:Fault>'
```

**Expected:** backend-generated `<soap:Fault>` passes through verbatim. This is the
**backend's** fault — gateway-generated faults are Step 3.

---

## Step 3 — soap-dispatch Policy + SOAP Fault Formatter

> **What to rebuild:** `gateway-controller` AND `gateway-runtime` (the soap-dispatch
> policy compiles into the policy-engine binary via gateway-builder).
>
> ```bash
> cd /Users/savindudimal/Desktop/APIM/Repo/api-platform/gateway
> make build          # rebuilds all components (controller + runtime with soap-dispatch)
> docker compose up -d --force-recreate
> ```
>
> Re-deploy the calculator API after restart if it was deleted.

---

### 3.1 Verify soap-dispatch policy is compiled into the engine

```bash
curl -s http://localhost:9002/policies | jq '[.[] | select(.name | contains("soap"))]'
```

**Expected:** an entry for `wso2_apip_sys_soap_dispatch` v1.0.0.

> If the admin API is not available, check the policy-engine startup logs:
> ```bash
> docker logs gateway-gateway-runtime-1 2>&1 | grep soap
> ```
> **Expected:** a log line registering `wso2_apip_sys_soap_dispatch`.

---

### 3.2 SOAP Fault on auth failure (401 → SOAP Fault, not JSON)

Deploy the calculator API **with** `api-key-auth`:

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: calculator-auth-v1
spec:
  displayName: Calculator SOAP Auth API
  version: v1.0
  context: /calculator-auth/v1
  soapVersion: "1.1"
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
  policies:
    - name: api-key-auth
      version: v1
      params:
        key: X-API-Key
        in: header
  operations:
    - name: Add
      soapAction: "Add"
' | jq .status
```

Invoke **without** an API key:

```bash
curl -si -X POST http://localhost:8080/calculator-auth/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Add' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:AddRequest><a>1</a><b>2</b></tns:AddRequest></soap:Body>
</soap:Envelope>'
```

**Expected:**
- HTTP status `401`
- `Content-Type: text/xml; charset=utf-8` (NOT `application/json`)
- Response body contains `<soap:Envelope>` with `<faultcode>soap:Client</faultcode>`
- Does NOT contain `{"error":`

---

### 3.3 SOAP 1.2 fault dialect (401 → SOAP 1.2 fault)

```bash
curl -si -X POST http://localhost:8080/calculator-auth/v1 \
  -H 'Content-Type: application/soap+xml; charset=utf-8' \
  --data-binary '<?xml version="1.0"?>
<env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope"
              xmlns:tns="http://example.com/soap/calculator/v1">
  <env:Body><tns:AddRequest><a>1</a><b>2</b></tns:AddRequest></env:Body>
</env:Envelope>'
```

**Expected:**
- HTTP status `401`
- `Content-Type: application/soap+xml; charset=utf-8`
- Body contains `<env:Fault>` with `<env:Value>env:Sender</env:Value>` (not `soap:Client`)

---

### 3.4 Malformed envelope → SOAP Fault (not JSON 400)

```bash
curl -si -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Add' \
  --data-raw 'this is not xml at all'
```

**Expected:**
- HTTP status `400`
- `Content-Type: text/xml; charset=utf-8`
- Response body contains `<soap:Fault><faultcode>soap:Client</faultcode>`

---

### 3.5 Non-envelope XML → SOAP Fault

```bash
curl -si -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Add' \
  --data-raw '<someRandomXML><notASoapEnvelope/></someRandomXML>'
```

**Expected:** HTTP 400, SOAP Fault body mentioning envelope.

---

### 3.6 Operation resolved from SOAPAction header

Invoke a well-formed request with a valid SOAPAction and check the controller log or
analytics to confirm `soap_operation` is set:

```bash
curl -s -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Add' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:AddRequest><a>10</a><b>5</b></tns:AddRequest></soap:Body>
</soap:Envelope>'

# Confirm policy-engine processed the request (policy logs show operation)
docker logs gateway-gateway-runtime-1 2>&1 | grep -i "soap_operation\|Add" | tail -5
```

**Expected:** request succeeds (HTTP 200, result 15); policy-engine log may show
`soap_operation=Add`.

---

### 3.7 Operation resolved from body QName (doc/literal, empty SOAPAction)

```bash
curl -s -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: ""' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:AddRequest><a>20</a><b>3</b></tns:AddRequest></soap:Body>
</soap:Envelope>'
```

**Expected:** HTTP 200, result 23. The dispatcher falls back to the body QName `AddRequest`
and resolves it to the declared operation `Add`.

---

### 3.8 GET (?wsdl) route: auth applies, envelope validation does not

The GET route carries the same **API-level policies** as the POST route (auth protects
the whole API, including WSDL retrieval — otherwise the GET route would be an
unauthenticated path to the backend). Only the **soap-dispatch** policy is excluded
from GET, since its envelope validation would reject the empty body of a `?wsdl`
request.

**(a) Unauthenticated `?wsdl` → 401 SOAP Fault:**

```bash
curl -si 'http://localhost:8080/calculator-auth/v1?wsdl'
```

**Expected:** HTTP `401`, `Content-Type: text/xml`, body is a `<soap:Fault>` with
`<faultcode>soap:Client</faultcode>` — auth is enforced on WSDL retrieval, and the
fault formatter renders the denial as a SOAP Fault (this also re-verifies 3.2 on the
GET route).

**(b) Authenticated `?wsdl` → no envelope-validation 400; reaches the backend:**

```bash
# Reuse $KEY from 3.9, or create one first
curl -si -H "X-API-Key: $KEY" 'http://localhost:8080/calculator-auth/v1?wsdl'
```

**Expected:** NOT a `400` "SOAP envelope required" fault — proving soap-dispatch is
absent from the GET route. The response comes from the backend. (For the calculator
backend this is a `404`, because it serves its WSDL at the service root rather than at
the `/soap11` endpoint path; a conventional SOAP backend that serves
`endpoint?wsdl` on the same path would return `200 text/xml` here.)

---

### 3.9 Auth-guarded API: valid API key still works

```bash
# Create an API key
KEY=$(curl -s -X POST \
  http://localhost:9090/api/management/v0.9/soap-apis/calculator-auth-v1/api-keys \
  -u admin:admin -H 'Content-Type: application/json' \
  -d '{"name":"test-key"}' | jq -r '.apiKey')

echo "Key: $KEY"

# Invoke with valid key
curl -si -X POST http://localhost:8080/calculator-auth/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Add' \
  -H "X-API-Key: $KEY" \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:AddRequest><a>6</a><b>7</b></tns:AddRequest></soap:Body>
</soap:Envelope>'
```

**Expected:** HTTP 200, `<result>13</result>`.

---

### 3.10 Rate-limit fault is a SOAP Fault (optional — requires ratelimit policy)

Deploy a SOAP API with a very low limit:

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: calculator-throttle-v1
spec:
  displayName: Calculator SOAP Throttled API
  version: v1.0
  context: /calculator-throttle/v1
  soapVersion: "1.1"
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
  policies:
    - name: basic-ratelimit
      version: v1
      params:
        limits:
          - requests: 1
            duration: "1m"
' | jq .status

# Hit it twice — second should be throttled
for i in 1 2; do
  echo "--- Request $i ---"
  curl -si -X POST http://localhost:8080/calculator-throttle/v1 \
    -H 'Content-Type: text/xml; charset=utf-8' \
    -H 'SOAPAction: Echo' \
    --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:EchoRequest><message>ping</message></tns:EchoRequest></soap:Body>
</soap:Envelope>' | head -8
done
```

**Expected:** first request 200; second request 429 with `Content-Type: text/xml` and a
`<soap:Fault><faultcode>soap:Client</faultcode>` body.

---

## Step 4 — Auth/Throttle Verification + Analytics Operation Labeling

> **What to rebuild:** `gateway-runtime` only (soap-dispatch policy + analytics/ALS
> changed; the controller is untouched in this step).
>
> ```bash
> cd /Users/savindudimal/Desktop/APIM/Repo/api-platform/gateway
> make build-gateway-runtime     # or: make build
> docker compose up -d --force-recreate gateway-runtime
> ```

### 4.1 Auth policy matrix (HTTP-transport auth works as-is)

| Policy | Verified by | Notes |
|---|---|---|
| `api-key-auth` | 3.2 (401 → SOAP Fault) + 3.9 (valid key → 200) | Done in Step 3 testing |
| `basic-ratelimit` | 3.10 (429 → SOAP Fault) | Done in Step 3 testing |
| `jwt-auth` | IT suite (`it/features/jwt-auth.feature` pattern) | Requires a JWKS issuer configured — covered by integration tests in Step 6 rather than manual testing |
| `subscription-validation` | Step 6 IT | Requires application + subscription setup |

If 3.2, 3.9, and 3.10 passed, the Step 4 auth/throttle verification goal is met —
these policies read HTTP headers only and required no SOAP-specific changes.

---

### 4.2 Analytics events carry the SOAP operation

The soap-dispatch policy now publishes the resolved operation into the standard
operation dimension (`SharedContext.OperationPath` → `x-wso2-operation-path`) and
the ALS forwards `soap_operation` / `soap_action` into published events under
`event.Properties["soapAnalytics"]`.

**(a) Enable debug logging** on the policy-engine — edit `configs/config.toml`:

```toml
[logging]
level = "debug"     # was "info"
```

```bash
docker compose up -d --force-recreate gateway-runtime
```

**(b) Invoke an operation** (calculator API from Step 2, no auth):

```bash
curl -s -X POST http://localhost:8080/calculator/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: Add' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:AddRequest><a>3</a><b>4</b></tns:AddRequest></soap:Body>
</soap:Envelope>' > /dev/null
```

**(c) Check the analytics metadata** in the policy-engine logs:

```bash
docker logs gateway-gateway-runtime-1 2>&1 | grep -E "soap_operation|operation-path" | tail -5
```

**Expected:** debug lines showing `soap_operation -> Add`, `soap_action -> Add`, and
`x-wso2-operation-path -> Add` among the analytics metadata keys.

**(d) Doc/literal dispatch also labels correctly** — repeat (b) with `SOAPAction: ""`
(empty). **Expected:** `soap_operation -> AddRequest` (resolved from the body QName).

**(e) (Optional) Moesif end-to-end** — if you have a Moesif application ID, set it in
`configs/config.toml` under `[analytics.publishers.moesif]` and verify events in the
Moesif dashboard carry `soapAnalytics.soap_operation` in their metadata and the
request URI of the SOAP context.

---

## Step 5 — Per-Operation Routes via SOAPAction (per-op policies/throttling)

> **What to rebuild:** `gateway-controller` only (translator + transformer changed;
> the policy-engine is untouched in this step).
>
> ```bash
> cd /Users/savindudimal/Desktop/APIM/Repo/api-platform/gateway
> make build-controller
> docker compose up -d --force-recreate gateway-controller
> ```

### 5.1 Deploy an API with an operation-level rate limit

`Add` gets a 1-request/min limit; `Echo` has no operation-level policy:

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: calculator-perop-v1
spec:
  displayName: Calculator PerOp API
  version: v1.0
  context: /calculator-perop/v1
  soapVersion: "1.1"
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
  operations:
    - name: Add
      soapAction: "Add"
      policies:
        - name: basic-ratelimit
          version: v1
          params:
            limits:
              - requests: 1
                duration: "1m"
    - name: Echo
      soapAction: "Echo"
' | jq .status
```

### 5.2 Per-operation Envoy route exists

> Route names carry a trailing slash on the path segment (the service route is the
> context + the root operation path `/`), and the vhost segment is your gateway's
> default vhost (`*` in the default config), e.g.
> `POST|/calculator-perop/v1/|*|soapAction=Add`.

```bash
curl -s http://localhost:9901/config_dump | grep -o 'POST|/calculator-perop/v1/|[^"]*' | sort -u
```

**Expected:** three POST route names — the generic `POST|/calculator-perop/v1/|*`
plus the per-operation `...|soapAction=Add` and `...|soapAction=Echo`. Also confirm
the per-op route carries the `soapaction` header matcher:

```bash
curl -s http://localhost:9901/config_dump | grep -B 2 -A 2 '"soapaction"' | head -12
```

**Expected:** a header matcher with `safe_regex` `^"?Add"?$` (and one for Echo).

### 5.3 Per-operation throttle applies to Add only

```bash
ENVELOPE='<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:AddRequest><a>1</a><b>2</b></tns:AddRequest></soap:Body>
</soap:Envelope>'

# Two Add calls: first 200, second 429 (SOAP Fault)
for i in 1 2; do
  echo "--- Add $i ---"
  curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8080/calculator-perop/v1 \
    -H 'Content-Type: text/xml; charset=utf-8' -H 'SOAPAction: Add' \
    --data-binary "$ENVELOPE"
done

# Echo still works (no op-level limit) — proving the throttle is per-operation
curl -s -o /dev/null -w "Echo: %{http_code}\n" -X POST http://localhost:8080/calculator-perop/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' -H 'SOAPAction: Echo' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:EchoRequest><message>hi</message></tns:EchoRequest></soap:Body>
</soap:Envelope>'
```

**Expected:** `200`, `429`, `Echo: 200`. The 429 body is a SOAP Fault (`faultcode soap:Client`).

### 5.4 Quoted SOAPAction routes to the same per-op route

```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8080/calculator-perop/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' -H 'SOAPAction: "Add"' \
  --data-binary "$ENVELOPE"
```

**Expected:** `429` (still throttled — the quoted form `"Add"` matched the same per-op
route, proving the optional-quotes regex).

### 5.5 Undeclared action falls through to the generic route

```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8080/calculator-perop/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' -H 'SOAPAction: SomethingElse' \
  --data-binary "$ENVELOPE"
```

**Expected:** `200` — no SOAPAction match → generic POST route → no op-level throttle.
(The backend executes AddRequest from the body regardless of the header.)

---

## Step 5b — Document/literal dispatch via `bodyElement`

> **What to rebuild:** `gateway-controller` (validator/transformer) **and**
> `gateway-runtime` (soap-dispatch policy resolution). Use `make build` for both.

The `Subtract` operation is document/literal: it has an **empty `soapAction`** and its SOAP
Body wrapper element (`SubtractOperandsRequest`) **differs from the operation name**
(`Subtract`). This exercises body-element dispatch — there is no header to route or match
on, so the request takes the generic POST route and `soap-dispatch` resolves the operation
from the body, using the `bodyElement` mapping to recover the logical name.

### 5b.1 Deploy an API declaring the doc/literal operation

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: calculator-doclit-v1
spec:
  displayName: Calculator DocLit API
  version: v1.0
  context: /calculator-doclit/v1
  soapVersion: "1.1"
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
  operations:
    - name: Add
      soapAction: "urn:Add"
    - name: Subtract
      soapAction: ""                       # document/literal: no SOAPAction
      bodyElement: SubtractOperandsRequest # wrapper element != operation name
' | jq .status
```

### 5b.2 No per-operation route is created for Subtract (empty soapAction)

```bash
curl -s http://localhost:9901/config_dump | grep -o 'POST|/calculator-doclit/v1/|[^"]*' | sort -u
```

**Expected:** the generic `POST|/calculator-doclit/v1/|*` and `...|soapAction=urn:Add`,
but **no** `soapAction` route for Subtract — empty actions cannot be header-matched and
fall through to the generic route by design.

### 5b.3 Invoke Subtract with an empty SOAPAction (body-element dispatch)

```bash
curl -s -X POST http://localhost:8080/calculator-doclit/v1 \
  -H 'Content-Type: text/xml; charset=utf-8' \
  -H 'SOAPAction: ""' \
  --data-binary '<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:tns="http://example.com/soap/calculator/v1">
  <soap:Body><tns:SubtractOperandsRequest><a>10</a><b>3</b></tns:SubtractOperandsRequest></soap:Body>
</soap:Envelope>'
```

**Expected:** HTTP 200, `<tns:SubtractOperandsResponse><result>7</result></...>` — the
gateway proxied the doc/literal request and the backend computed `10 - 3`.

### 5b.4 The operation resolves to the logical name `Subtract`

With debug logging enabled (see 4.2a), after the 5b.3 call:

```bash
docker logs gateway-gateway-runtime-1 2>&1 | grep -E "soap_operation|operation-path" | tail -3
```

**Expected:** `soap_operation -> Subtract` (and `x-wso2-operation-path -> Subtract`) — the
`bodyElement` mapping (`SubtractOperandsRequest` → `Subtract`) recovered the logical name
even though the wire element is `SubtractOperandsRequest`. Without the mapping the operation
would have been labelled `SubtractOperandsRequest`.

### 5b.5 Duplicate bodyElement is rejected

```bash
curl -s -X POST http://localhost:9090/api/management/v0.9/soap-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-raw '
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: SoapApi
metadata:
  name: dup-bodyelement
spec:
  displayName: Dup BodyElement
  version: v1.0
  context: /dup-be/v1
  upstream:
    main:
      url: http://host.docker.internal:3002/calculator/v1/soap11
  operations:
    - name: opA
      bodyElement: SameWrapper
    - name: opB
      bodyElement: SameWrapper
' | jq '.errors'
```

**Expected:** HTTP 400 with an error on `spec.operations[1].bodyElement`
("Duplicate bodyElement ...").

---

## Postman collection

For an interactive/repeatable alternative to the curl steps above, import
[`soap-api.postman_collection.json`](soap-api.postman_collection.json) into Postman (or run
it with `newman`). It mirrors these scenarios with assertions:

```bash
# Headless run (requires the backend + gateway up, per Prerequisites)
newman run docs/gateway/soap-api.postman_collection.json
```

Collection variables (`mgmtUrl`, `gwUrl`, `adminUser`, `adminPass`) default to the local
docker-compose setup; the API-key requests save the generated key into a collection
variable so later authenticated requests reuse it automatically.

---

## Cleanup

```bash
for api in calculator-v1 calculator-auth-v1 calculator-throttle-v1 calculator-perop-v1 calculator-doclit-v1; do
  curl -s -X DELETE http://localhost:9090/api/management/v0.9/soap-apis/$api \
    -u admin:admin | jq -r '.status + " " + .id'
done
```

---

## Quick Regression Check — REST APIs Unaffected

After all SOAP tests, verify an existing REST API still receives JSON errors (not SOAP Faults):

```bash
# Deploy the sample echo API if not already present
curl -s -X POST http://localhost:9090/api/management/v0.9/rest-apis \
  -u admin:admin -H 'Content-Type: application/yaml' \
  --data-binary @examples/sample-echo-api.yaml | jq .status

# Request without auth (expect JSON 401, not a SOAP Fault)
curl -si http://localhost:8080/echo/anything
```

**Expected:** HTTP 401, `Content-Type: application/json`, `{"error":"..."}` body —
unchanged from before SOAP support was added.
