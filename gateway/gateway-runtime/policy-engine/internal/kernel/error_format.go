/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package kernel

import (
	"strconv"
	"strings"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"

	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/kernel/errorformat"
)

// errorCategoryHintHeader is an internal header a policy may set on an error
// ImmediateResponse to refine the error category beyond the status-code
// default (additive — no SDK change). It is consumed here and never reaches
// the client.
const errorCategoryHintHeader = "x-wso2-error-category"

// faultFlagHeaderName marks a response as an Envoy-generated local reply
// (added by the controller's local_reply_config mappers with
// %RESPONSE_FLAGS%). Its presence tells the response phase that a 5xx did
// NOT come from the backend, so backend error reshaping must pass it
// through. Keep in sync with xds.FaultFlagHeaderName in the controller.
const faultFlagHeaderName = "x-wso2-response-fault-flag"

// applyErrorFormat rewrites an error ImmediateResponse according to the
// error-response customization config (per-API override first, then global).
// It is invoked at every kernel choke point immediately before
// applySOAPFaultFormat, so a customized message is subsequently wrapped into
// a SOAP Fault for SOAP APIs (design §4.5) unless the customization already
// produced an XML body.
//
// Responses are returned unchanged when the status is not an error (< 400)
// or when no configuration matches the status — nothing configured means
// byte-for-byte today's behavior.
func applyErrorFormat(immResp policy.ImmediateResponse, execCtx *PolicyExecutionContext) policy.ImmediateResponse {
	if execCtx == nil {
		return immResp
	}

	vals := errorformat.PlaceholderValues{RequestID: execCtx.requestID}
	if execCtx.sharedCtx != nil {
		vals.APIName = execCtx.sharedCtx.APIName
		vals.APIVersion = execCtx.sharedCtx.APIVersion
		if vals.RequestID == "" {
			vals.RequestID = execCtx.sharedCtx.RequestID
		}
	}

	accept := ""
	if execCtx.requestHeaderCtx != nil && execCtx.requestHeaderCtx.Headers != nil {
		if vs := execCtx.requestHeaderCtx.Headers.Get("accept"); len(vs) > 0 {
			accept = vs[0]
		}
	}

	return formatErrorResponse(immResp, execCtx.errorResolver, execCtx.perAPIErrorResponses, accept, vals)
}

// formatErrorResponse is the shared core of applyErrorFormat, also used on
// the raw pre-context path (no policy chain found) where no execution
// context exists. On a config match it replaces the status code,
// content-type, and body; other headers (WWW-Authenticate, Retry-After,
// x-error-id, ...) are preserved.
func formatErrorResponse(
	immResp policy.ImmediateResponse,
	resolver *errorformat.Resolver,
	perAPI *errorformat.ErrorResponses,
	accept string,
	vals errorformat.PlaceholderValues,
) policy.ImmediateResponse {
	if immResp.StatusCode < 400 || resolver == nil {
		return immResp
	}

	// A policy may refine the category via the internal hint header; strip
	// it either way so it never leaks to the client.
	hint, hadHint := takeHeader(&immResp, errorCategoryHintHeader)
	if hadHint && hint != "" {
		vals.Category = errorformat.Category(strings.ToUpper(hint))
	}

	res, ok := resolver.Resolve(immResp.StatusCode, accept, perAPI, vals)
	if !ok {
		return immResp
	}

	headers := make(map[string]string, len(immResp.Headers)+1)
	for k, v := range immResp.Headers {
		if strings.EqualFold(k, "content-type") {
			continue // replaced below
		}
		headers[k] = v
	}
	headers["content-type"] = res.ContentType

	immResp.StatusCode = res.StatusCode
	immResp.Headers = headers
	immResp.Body = res.Body
	return immResp
}

// backendErrorImmediateResponse decides whether the current backend error
// response should be reshaped (source B, design §4.3) and, if so, returns a
// synthesized ImmediateResponse for the response-header short-circuit path —
// the standard choke point (applyErrorFormat → applySOAPFaultFormat) then
// renders the configured body.
//
// Reshaping is strictly OPT-IN per API: it requires an EXACT status entry in
// this API's errorResponses document. The global configuration and the
// per-API "default" entry never reshape backend responses (BACKEND_ERROR is
// passthrough by default). Passthrough also applies to:
//   - Envoy local replies (marked with the x-wso2-response-fault-flag header
//     by the controller's local_reply_config) — those are source C;
//   - streaming responses (the body is already flowing; a clean replacement
//     is impossible — the documented streaming caveat).
//
// The upstream body is discarded (it never reaches the client), which is the
// point: reshaping exists to mask backend error internals. Response headers
// (post policy mutations) are carried over, minus body-framing headers.
func (ec *PolicyExecutionContext) backendErrorImmediateResponse() (policy.ImmediateResponse, bool) {
	var zero policy.ImmediateResponse
	if ec.responseHeaderCtx == nil || ec.errorResolver == nil || ec.perAPIErrorResponses == nil {
		return zero, false
	}
	if ec.isStreamingResponse {
		return zero, false
	}
	status := ec.responseHeaderCtx.ResponseStatus
	if status < 400 {
		return zero, false
	}
	if _, optedIn := ec.perAPIErrorResponses.Responses[strconv.Itoa(status)]; !optedIn {
		return zero, false
	}
	// An Envoy local reply is not a backend response — leave it alone.
	if ec.responseHeaderCtx.ResponseHeaders != nil {
		if flags := ec.responseHeaderCtx.ResponseHeaders.Get(faultFlagHeaderName); len(flags) > 0 && flags[0] != "" {
			return zero, false
		}
	}

	headers := make(map[string]string)
	if ec.responseHeaderCtx.ResponseHeaders != nil {
		for name, values := range ec.responseHeaderCtx.ResponseHeaders.GetAll() {
			lower := strings.ToLower(name)
			// Skip pseudo-headers and body-framing headers: the replacement
			// body has different length/encoding, and content-type is
			// re-negotiated by the resolver.
			if strings.HasPrefix(lower, ":") {
				continue
			}
			switch lower {
			case "content-length", "content-encoding", "transfer-encoding", "connection":
				continue
			}
			if len(values) > 0 {
				headers[name] = values[0]
			}
		}
	}

	return policy.ImmediateResponse{StatusCode: status, Headers: headers}, true
}

// takeHeader removes a header (case-insensitively) from the response and
// returns its value. The headers map is copied before mutation because it is
// owned by the policy that produced the response.
func takeHeader(immResp *policy.ImmediateResponse, name string) (string, bool) {
	found := false
	value := ""
	for k, v := range immResp.Headers {
		if strings.EqualFold(k, name) {
			found = true
			value = v
			break
		}
	}
	if !found {
		return "", false
	}
	headers := make(map[string]string, len(immResp.Headers)-1)
	for k, v := range immResp.Headers {
		if strings.EqualFold(k, name) {
			continue
		}
		headers[k] = v
	}
	immResp.Headers = headers
	return value, true
}

// rawRequestHeader extracts a header value from a raw ext_proc
// RequestHeaders message. Used for engine-generated errors that occur before
// an execution context exists (e.g. no policy chain found).
func rawRequestHeader(req *extprocv3.ProcessingRequest, name string) string {
	headers, ok := req.Request.(*extprocv3.ProcessingRequest_RequestHeaders)
	if !ok || headers.RequestHeaders == nil || headers.RequestHeaders.Headers == nil {
		return ""
	}
	for _, h := range headers.RequestHeaders.Headers.Headers {
		if strings.EqualFold(h.Key, name) {
			if len(h.RawValue) > 0 {
				return string(h.RawValue)
			}
			return h.Value
		}
	}
	return ""
}
