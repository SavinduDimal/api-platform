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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"

	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/config"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/executor"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/registry"
)

// soapExecCtx builds a minimal execution context for a SOAP API request with the
// given request content type.
func soapExecCtx(requestContentType string) *PolicyExecutionContext {
	sharedCtx := &policy.SharedContext{APIKind: policy.APIKind(apiKindSoapApi)}
	headers := policy.NewHeaders(map[string][]string{
		"content-type": {requestContentType},
	})
	return &PolicyExecutionContext{
		sharedCtx: sharedCtx,
		requestHeaderCtx: &policy.RequestHeaderContext{
			SharedContext: sharedCtx,
			Headers:       headers,
		},
	}
}

func jsonError401() policy.ImmediateResponse {
	return policy.ImmediateResponse{
		StatusCode: 401,
		Headers:    map[string]string{"content-type": "application/json", "www-authenticate": "Basic"},
		Body:       []byte(`{"error":"Unauthorized"}`),
	}
}

func TestApplySOAPFaultFormat_NonSoapKindUnchanged(t *testing.T) {
	execCtx := soapExecCtx("text/xml")
	execCtx.sharedCtx.APIKind = policy.APIKindRestApi

	in := jsonError401()
	out := applySOAPFaultFormat(in, execCtx)

	assert.Equal(t, in.Body, out.Body)
	assert.Equal(t, "application/json", out.Headers["content-type"])
}

func TestApplySOAPFaultFormat_Soap11Fault(t *testing.T) {
	out := applySOAPFaultFormat(jsonError401(), soapExecCtx("text/xml; charset=utf-8"))

	// Status + transport headers preserved; body + content-type rewritten.
	assert.Equal(t, 401, out.StatusCode)
	assert.Equal(t, "Basic", out.Headers["www-authenticate"])
	assert.Equal(t, soap11ContentType, out.Headers["content-type"])

	body := string(out.Body)
	assert.Contains(t, body, "http://schemas.xmlsoap.org/soap/envelope/")
	assert.Contains(t, body, "<faultcode>soap:Client</faultcode>")
	assert.Contains(t, body, "<faultstring>Unauthorized</faultstring>")
	// Original body preserved (escaped) in detail.
	assert.Contains(t, body, "&quot;Unauthorized&quot;")
}

func TestApplySOAPFaultFormat_Soap12Fault(t *testing.T) {
	out := applySOAPFaultFormat(jsonError401(), soapExecCtx(`application/soap+xml; charset=utf-8; action="urn:op"`))

	assert.Equal(t, soap12ContentType, out.Headers["content-type"])

	body := string(out.Body)
	assert.Contains(t, body, "http://www.w3.org/2003/05/soap-envelope")
	assert.Contains(t, body, "<env:Value>env:Sender</env:Value>")
	assert.Contains(t, body, "Unauthorized")
}

func TestApplySOAPFaultFormat_ServerFaultFor5xx(t *testing.T) {
	in := policy.ImmediateResponse{
		StatusCode: 503,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"error":"upstream down"}`),
	}
	out := applySOAPFaultFormat(in, soapExecCtx("text/xml"))

	assert.Contains(t, string(out.Body), "<faultcode>soap:Server</faultcode>")
	assert.Equal(t, 503, out.StatusCode)
}

func TestApplySOAPFaultFormat_NonErrorUnchanged(t *testing.T) {
	in := policy.ImmediateResponse{
		StatusCode: 204,
		Headers:    map[string]string{"content-type": "application/json"},
	}
	out := applySOAPFaultFormat(in, soapExecCtx("text/xml"))
	assert.Equal(t, in.Headers["content-type"], out.Headers["content-type"])
	assert.Empty(t, out.Body)
}

func TestApplySOAPFaultFormat_ExistingXMLBodyUnchanged(t *testing.T) {
	customFault := []byte(`<soap:Envelope>...custom fault...</soap:Envelope>`)
	in := policy.ImmediateResponse{
		StatusCode: 400,
		Headers:    map[string]string{"Content-Type": "text/xml"},
		Body:       customFault,
	}
	out := applySOAPFaultFormat(in, soapExecCtx("text/xml"))
	assert.Equal(t, customFault, out.Body)
}

func TestBuildSoapFault_EscapesContent(t *testing.T) {
	body := string(buildSoapFault(soapVersion11, "soap:Client", `bad <input> & "quotes"`, ""))
	assert.Contains(t, body, "bad &lt;input&gt; &amp; &quot;quotes&quot;")
	assert.False(t, strings.Contains(body, "<input>"))
}

// TestTranslateRequestActionsCore_SoapFault verifies the end-to-end translation path
// rewrites a short-circuit error into a SOAP Fault for SoapApi requests.
func TestTranslateRequestActionsCore_SoapFault(t *testing.T) {
	kernel := NewKernel()
	chainExecutor := executor.NewChainExecutor(nil, nil, nil)
	server := NewExternalProcessorServer(kernel, chainExecutor, config.TracingConfig{}, "")

	chain := &registry.PolicyChain{}
	execCtx := newPolicyExecutionContext(server, "test-route", chain)
	soapCtx := soapExecCtx("text/xml; charset=utf-8")
	execCtx.sharedCtx = soapCtx.sharedCtx
	execCtx.requestHeaderCtx = soapCtx.requestHeaderCtx
	execCtx.requestBodyCtx = &policy.RequestContext{
		Path:          "/calculator/v1",
		SharedContext: execCtx.sharedCtx,
	}

	result := &executor.RequestExecutionResult{
		ShortCircuited: true,
		FinalAction: policy.ImmediateResponse{
			StatusCode: 429,
			Headers:    map[string]string{"content-type": "application/json"},
			Body:       []byte(`{"error":"rate limit exceeded"}`),
		},
	}

	rsl, err := translateRequestActionsCore(result, execCtx)
	require.NoError(t, err)
	require.NotNil(t, rsl.ImmediateResp)

	immediate := rsl.ImmediateResp.GetImmediateResponse()
	require.NotNil(t, immediate)
	assert.Equal(t, uint32(429), uint32(immediate.Status.Code))
	assert.Contains(t, string(immediate.Body), "<faultcode>soap:Client</faultcode>")
	assert.Contains(t, string(immediate.Body), "Too Many Requests")
}
