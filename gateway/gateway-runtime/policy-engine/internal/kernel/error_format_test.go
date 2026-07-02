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
	"context"
	"strings"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"

	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/config"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/executor"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/kernel/errorformat"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/registry"
)

const testGlobalErrorDoc = `
responses:
  "401":
    content:
      application/json:
        example: { code: 401, message: "Custom auth required", requestId: "{{requestId}}", api: "{{apiName}}" }
      application/xml:
        example: "<error><message>Custom auth required</message></error>"
  "500":
    content:
      application/json:
        example: { code: 500, message: "Custom internal error", category: "{{category}}" }
  "504":
    x-status-code-override: 502
    content:
      application/json:
        example: { message: "Custom upstream timeout" }
`

func testErrorResolver(t *testing.T) *errorformat.Resolver {
	t.Helper()
	global, err := errorformat.Parse([]byte(testGlobalErrorDoc))
	require.NoError(t, err)
	return errorformat.NewResolver(global, "application/json")
}

// errorFormatExecCtx builds an execution context carrying the global
// error-response resolver, mirroring what initializeExecutionContext wires up.
func errorFormatExecCtx(t *testing.T) *PolicyExecutionContext {
	t.Helper()
	sharedCtx := &policy.SharedContext{
		APIName:    "TestAPI",
		APIVersion: "v1.0",
	}
	return &PolicyExecutionContext{
		sharedCtx: sharedCtx,
		requestID: "req-42",
		requestHeaderCtx: &policy.RequestHeaderContext{
			SharedContext: sharedCtx,
			Headers:       policy.NewHeaders(map[string][]string{}),
		},
		errorResolver: testErrorResolver(t),
	}
}

func immediate401() policy.ImmediateResponse {
	return policy.ImmediateResponse{
		StatusCode: 401,
		Headers:    map[string]string{"content-type": "application/json", "www-authenticate": "Basic"},
		Body:       []byte(`{"error":"Unauthorized"}`),
	}
}

// =============================================================================
// applyErrorFormat unit tests
// =============================================================================

func TestApplyErrorFormat_MatchReplacesBody(t *testing.T) {
	out := applyErrorFormat(immediate401(), errorFormatExecCtx(t))

	assert.Equal(t, 401, out.StatusCode)
	assert.Equal(t, "application/json", out.Headers["content-type"])
	// Transport headers preserved.
	assert.Equal(t, "Basic", out.Headers["www-authenticate"])
	body := string(out.Body)
	assert.Contains(t, body, "Custom auth required")
	assert.Contains(t, body, `"requestId":"req-42"`)
	assert.Contains(t, body, `"api":"TestAPI"`)
}

func TestApplyErrorFormat_NilContextUnchanged(t *testing.T) {
	in := immediate401()
	out := applyErrorFormat(in, nil)
	assert.Equal(t, in.Body, out.Body)
}

func TestApplyErrorFormat_NoResolverUnchanged(t *testing.T) {
	execCtx := errorFormatExecCtx(t)
	execCtx.errorResolver = nil
	in := immediate401()
	out := applyErrorFormat(in, execCtx)
	assert.Equal(t, in.Body, out.Body)
	assert.Equal(t, in.Headers, out.Headers)
}

func TestApplyErrorFormat_UnmatchedStatusUnchanged(t *testing.T) {
	in := policy.ImmediateResponse{
		StatusCode: 429,
		Headers:    map[string]string{"content-type": "application/json", "retry-after": "10"},
		Body:       []byte(`{"error":"throttled"}`),
	}
	out := applyErrorFormat(in, errorFormatExecCtx(t))
	assert.Equal(t, in.Body, out.Body)
	assert.Equal(t, "10", out.Headers["retry-after"])
}

func TestApplyErrorFormat_NonErrorStatusUnchanged(t *testing.T) {
	in := policy.ImmediateResponse{StatusCode: 302, Headers: map[string]string{"location": "/login"}}
	out := applyErrorFormat(in, errorFormatExecCtx(t))
	assert.Equal(t, in, out)
}

func TestApplyErrorFormat_StatusOverride(t *testing.T) {
	in := policy.ImmediateResponse{
		StatusCode: 504,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"error":"timeout"}`),
	}
	out := applyErrorFormat(in, errorFormatExecCtx(t))
	assert.Equal(t, 502, out.StatusCode)
	assert.Contains(t, string(out.Body), "Custom upstream timeout")
}

func TestApplyErrorFormat_AcceptNegotiation(t *testing.T) {
	execCtx := errorFormatExecCtx(t)
	execCtx.requestHeaderCtx.Headers = policy.NewHeaders(map[string][]string{
		"accept": {"application/xml"},
	})
	out := applyErrorFormat(immediate401(), execCtx)
	assert.Equal(t, "application/xml", out.Headers["content-type"])
	assert.Contains(t, string(out.Body), "<error>")
}

func TestApplyErrorFormat_CategoryHintConsumedAndStripped(t *testing.T) {
	in := policy.ImmediateResponse{
		StatusCode: 500,
		Headers: map[string]string{
			"content-type":          "application/json",
			"x-wso2-error-category": "backend_unavailable",
		},
		Body: []byte(`{"error":"boom"}`),
	}
	out := applyErrorFormat(in, errorFormatExecCtx(t))
	assert.Contains(t, string(out.Body), `"category":"BACKEND_UNAVAILABLE"`)
	_, present := out.Headers["x-wso2-error-category"]
	assert.False(t, present, "internal category hint header must not leak to the client")
}

func TestApplyErrorFormat_DefaultCategoryFromStatus(t *testing.T) {
	in := policy.ImmediateResponse{
		StatusCode: 500,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"error":"boom"}`),
	}
	out := applyErrorFormat(in, errorFormatExecCtx(t))
	assert.Contains(t, string(out.Body), `"category":"INTERNAL_ERROR"`)
}

func TestApplyErrorFormat_PerAPIPrecedence(t *testing.T) {
	perAPI, err := errorformat.Parse([]byte(`
responses:
  "401":
    content:
      application/json:
        example: { message: "Per-API auth message" }
`))
	require.NoError(t, err)

	execCtx := errorFormatExecCtx(t)
	execCtx.perAPIErrorResponses = perAPI

	out := applyErrorFormat(immediate401(), execCtx)
	assert.Contains(t, string(out.Body), "Per-API auth message")

	// A status the per-API doc doesn't cover falls back to global.
	in500 := policy.ImmediateResponse{
		StatusCode: 500,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"error":"boom"}`),
	}
	out500 := applyErrorFormat(in500, execCtx)
	assert.Contains(t, string(out500.Body), "Custom internal error")
}

func TestApplyErrorFormat_PerAPIOnlyNoGlobal(t *testing.T) {
	perAPI, err := errorformat.Parse([]byte(`
responses:
  "401":
    content:
      application/json:
        example: { message: "Per-API only" }
`))
	require.NoError(t, err)

	execCtx := errorFormatExecCtx(t)
	// [error_handling] disabled: resolver installed with nil global.
	execCtx.errorResolver = errorformat.NewResolver(nil, "application/json")
	execCtx.perAPIErrorResponses = perAPI

	out := applyErrorFormat(immediate401(), execCtx)
	assert.Contains(t, string(out.Body), "Per-API only")
}

// TestApplyErrorFormat_AnalyticsIndependent proves the design-review
// condition: customization runs with no analytics configuration, no
// analytics policy in the chain, and [analytics] enabled=false (the kernel
// error path never consults analytics state — this test builds the same
// contexts the kernel uses at its choke points with zero analytics setup).
func TestApplyErrorFormat_AnalyticsIndependent(t *testing.T) {
	kernel := NewKernel()
	kernel.SetErrorFormatResolver(testErrorResolver(t))
	chainExecutor := executor.NewChainExecutor(nil, nil, nil)
	server := NewExternalProcessorServer(kernel, chainExecutor, config.TracingConfig{}, "")

	execCtx := newPolicyExecutionContext(server, "test-route", &registry.PolicyChain{})
	execCtx.errorResolver = kernel.ErrorFormatResolver()
	execCtx.sharedCtx = &policy.SharedContext{}

	out := applyErrorFormat(immediate401(), execCtx)
	assert.Contains(t, string(out.Body), "Custom auth required")
}

// =============================================================================
// SOAP composition (design §4.5)
// =============================================================================

func TestApplyErrorFormat_SOAPComposition_JSONBodyWrappedInFault(t *testing.T) {
	execCtx := errorFormatExecCtx(t)
	execCtx.sharedCtx.APIKind = policy.APIKind(apiKindSoapApi)
	execCtx.requestHeaderCtx.Headers = policy.NewHeaders(map[string][]string{
		"content-type": {"text/xml; charset=utf-8"},
	})

	out := applyErrorFormat(immediate401(), execCtx)
	out = applySOAPFaultFormat(out, execCtx)

	assert.Equal(t, soap11ContentType, out.Headers["content-type"])
	body := string(out.Body)
	assert.Contains(t, body, "<faultcode>soap:Client</faultcode>")
	// The customized message travels into the fault detail.
	assert.Contains(t, body, "Custom auth required")
}

func TestApplyErrorFormat_SOAPComposition_XMLBodyLeftAlone(t *testing.T) {
	execCtx := errorFormatExecCtx(t)
	execCtx.sharedCtx.APIKind = policy.APIKind(apiKindSoapApi)
	execCtx.requestHeaderCtx.Headers = policy.NewHeaders(map[string][]string{
		"content-type": {"text/xml"},
		"accept":       {"application/xml"},
	})

	out := applyErrorFormat(immediate401(), execCtx)
	out = applySOAPFaultFormat(out, execCtx)

	// The config produced an XML body — the SOAP formatter's already-XML
	// guard lets the user-supplied XML through untouched.
	assert.Equal(t, "application/xml", out.Headers["content-type"])
	assert.Equal(t, "<error><message>Custom auth required</message></error>", string(out.Body))
}

// =============================================================================
// Choke-point coverage: all six translator sites + handlePolicyError
// =============================================================================

func newErrorFormatTestExecCtx(t *testing.T) *PolicyExecutionContext {
	kernel := NewKernel()
	kernel.SetErrorFormatResolver(testErrorResolver(t))
	chainExecutor := executor.NewChainExecutor(nil, nil, nil)
	server := NewExternalProcessorServer(kernel, chainExecutor, config.TracingConfig{}, "")

	execCtx := newPolicyExecutionContext(server, "test-route", &registry.PolicyChain{})
	execCtx.errorResolver = kernel.ErrorFormatResolver()
	execCtx.sharedCtx = &policy.SharedContext{APIName: "TestAPI", APIVersion: "v1.0"}
	execCtx.requestBodyCtx = &policy.RequestContext{
		Path:          "/api/test",
		SharedContext: execCtx.sharedCtx,
	}
	return execCtx
}

func shortCircuit401() policy.ImmediateResponse {
	return policy.ImmediateResponse{
		StatusCode: 401,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"error":"Unauthorized"}`),
	}
}

func assertCustomized401(t *testing.T, resp *extprocv3.ProcessingResponse) {
	t.Helper()
	require.NotNil(t, resp)
	immediate := resp.GetImmediateResponse()
	require.NotNil(t, immediate)
	assert.Equal(t, uint32(401), uint32(immediate.Status.Code))
	assert.Contains(t, string(immediate.Body), "Custom auth required")
}

func TestChokePoint_TranslateRequestActionsCore(t *testing.T) {
	execCtx := newErrorFormatTestExecCtx(t)
	result := &executor.RequestExecutionResult{ShortCircuited: true, FinalAction: shortCircuit401()}

	rsl, err := translateRequestActionsCore(result, execCtx)
	require.NoError(t, err)
	assertCustomized401(t, rsl.ImmediateResp)
}

func TestChokePoint_TranslateRequestHeaderActions(t *testing.T) {
	execCtx := newErrorFormatTestExecCtx(t)
	result := &executor.RequestHeaderExecutionResult{ShortCircuited: true, FinalAction: shortCircuit401()}

	resp, err := TranslateRequestHeaderActions(result, execCtx.policyChain, execCtx)
	require.NoError(t, err)
	assertCustomized401(t, resp)
}

func TestChokePoint_TranslateRequestHeaderActionsWithBodyMerge(t *testing.T) {
	execCtx := newErrorFormatTestExecCtx(t)
	headerResult := &executor.RequestHeaderExecutionResult{}
	bodyResult := &executor.RequestExecutionResult{ShortCircuited: true, FinalAction: shortCircuit401()}

	resp, err := TranslateRequestHeaderActionsWithBodyMerge(headerResult, bodyResult, execCtx)
	require.NoError(t, err)
	assertCustomized401(t, resp)
}

func TestChokePoint_TranslateResponseHeaderActions(t *testing.T) {
	execCtx := newErrorFormatTestExecCtx(t)
	result := &executor.ResponseHeaderExecutionResult{ShortCircuited: true, FinalAction: shortCircuit401()}

	resp, err := TranslateResponseHeaderActions(result, execCtx)
	require.NoError(t, err)
	assertCustomized401(t, resp)
}

func TestChokePoint_TranslateResponseHeaderActionsWithBodyMerge(t *testing.T) {
	execCtx := newErrorFormatTestExecCtx(t)
	headerResult := &executor.ResponseHeaderExecutionResult{}
	bodyResult := &executor.ResponseExecutionResult{ShortCircuited: true, FinalAction: shortCircuit401()}

	resp, err := TranslateResponseHeaderActionsWithBodyMerge(headerResult, bodyResult, execCtx)
	require.NoError(t, err)
	assertCustomized401(t, resp)
}

func TestChokePoint_TranslateResponseActionsCore(t *testing.T) {
	execCtx := newErrorFormatTestExecCtx(t)
	execCtx.responseBodyCtx = &policy.ResponseContext{SharedContext: execCtx.sharedCtx}
	result := &executor.ResponseExecutionResult{ShortCircuited: true, FinalAction: shortCircuit401()}

	_, _, _, _, immediateResp, err := translateResponseActionsCore(result, execCtx)
	require.NoError(t, err)
	assertCustomized401(t, immediateResp)
}

func TestChokePoint_HandlePolicyError(t *testing.T) {
	execCtx := newErrorFormatTestExecCtx(t)
	execCtx.requestID = "req-err-1"

	resp := execCtx.handlePolicyError(context.Background(), assert.AnError, "request_headers")
	require.NotNil(t, resp)
	immediate := resp.GetImmediateResponse()
	require.NotNil(t, immediate)
	assert.Equal(t, uint32(500), uint32(immediate.Status.Code))
	assert.Contains(t, string(immediate.Body), "Custom internal error")
	// The x-error-id correlation header survives customization.
	found := false
	for _, h := range immediate.Headers.SetHeaders {
		if strings.EqualFold(h.Header.Key, "x-error-id") {
			found = true
		}
	}
	assert.True(t, found, "x-error-id header should be preserved")
}

// =============================================================================
// Raw pre-context path helpers (no policy chain found)
// =============================================================================

func TestFormatErrorResponse_RawPath(t *testing.T) {
	in := policy.ImmediateResponse{
		StatusCode: 500,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"error":"Internal Server Error"}`),
	}
	out := formatErrorResponse(in, testErrorResolver(t), nil, "application/json", errorformat.PlaceholderValues{
		APIName:    "RawAPI",
		APIVersion: "v2.0",
	})
	assert.Contains(t, string(out.Body), "Custom internal error")
	assert.Equal(t, 500, out.StatusCode)
}
