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

// Package soapdispatch resolves the logical SOAP operation of a request and
// publishes it to the shared policy context (key "soap_operation") and analytics
// metadata, so downstream policies (throttling, authorization) and analytics can
// key on the operation even though all SOAP operations share one HTTP route.
//
// Resolution order:
//  1. SOAPAction HTTP header (SOAP 1.1) — surrounding quotes stripped
//  2. action parameter of the Content-Type header (SOAP 1.2)
//  3. local name of the first child element of the SOAP Body (doc/literal dispatch)
//
// When the resolved action matches a declared operation (params.operations), the
// declared name is used as the logical operation name; otherwise the raw action or
// body element name is used as-is (passthrough does not reject undeclared operations).
package soapdispatch

import (
	"bytes"
	"context"
	"encoding/xml"
	"log/slog"
	"mime"
	"strings"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// Shared-context metadata keys published by this policy.
const (
	// MetadataKeySoapOperation holds the resolved logical operation name.
	MetadataKeySoapOperation = "soap_operation"
	// MetadataKeySoapAction holds the raw SOAPAction / Content-Type action value.
	MetadataKeySoapAction = "soap_action"
)

// SoapDispatchPolicy is a stateless singleton; per-API configuration arrives via
// the params argument of each handler (same pattern as the analytics system policy).
type SoapDispatchPolicy struct{}

var ins = &SoapDispatchPolicy{}

// GetPolicy returns the singleton policy instance.
func GetPolicy(
	metadata policy.PolicyMetadata,
	params map[string]interface{},
) (policy.Policy, error) {
	return ins, nil
}

// GetPolicyV2 is an alias for GetPolicy, provided for compatibility with the
// Builder-generated plugin registry which calls GetPolicyV2 on all plugins.
func GetPolicyV2(
	metadata policy.PolicyMetadata,
	params map[string]interface{},
) (policy.Policy, error) {
	return GetPolicy(metadata, params)
}

// Mode buffers the request body so the SOAP envelope can be inspected; the
// response flow is untouched.
func (p *SoapDispatchPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeProcess,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// OnRequestHeaders resolves the operation from transport headers (SOAPAction or
// the Content-Type action parameter) when possible.
func (p *SoapDispatchPolicy) OnRequestHeaders(_ context.Context, reqCtx *policy.RequestHeaderContext, params map[string]interface{}) policy.RequestHeaderAction {
	if reqCtx.Headers == nil {
		return policy.UpstreamRequestHeaderModifications{}
	}

	action := actionFromHeaders(reqCtx.Headers)
	if action == "" {
		// Doc/literal or empty SOAPAction — resolved from the body in OnRequestBody.
		return policy.UpstreamRequestHeaderModifications{}
	}

	operation := resolveOperationName(action, "", declaredOperations(params))
	publishOperation(reqCtx.SharedContext, operation, action)

	return policy.UpstreamRequestHeaderModifications{
		AnalyticsMetadata: map[string]any{MetadataKeySoapOperation: operation},
	}
}

// OnRequestBody validates the SOAP envelope and resolves the operation from the
// first Body child element when transport headers did not identify it.
func (p *SoapDispatchPolicy) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	alreadyResolved := false
	if reqCtx.SharedContext != nil {
		_, alreadyResolved = reqCtx.SharedContext.Metadata[MetadataKeySoapOperation]
	}

	if alreadyResolved && !validateEnvelopeEnabled(params) {
		return nil
	}

	if reqCtx.Body == nil || !reqCtx.Body.Present || len(reqCtx.Body.Content) == 0 {
		if validateEnvelopeEnabled(params) {
			return malformedEnvelopeResponse("empty request body: a SOAP envelope is required")
		}
		return nil
	}

	bodyElement, err := firstBodyElement(reqCtx.Body.Content)
	if err != nil {
		if validateEnvelopeEnabled(params) {
			return malformedEnvelopeResponse(err.Error())
		}
		slog.Debug("SOAP dispatch: failed to parse envelope", slog.Any("error", err))
		return nil
	}

	if alreadyResolved {
		// Operation already published from headers; envelope validated — nothing more to do.
		return nil
	}

	operation := resolveOperationName("", bodyElement, declaredOperations(params))
	publishOperation(reqCtx.SharedContext, operation, "")

	return policy.UpstreamRequestModifications{
		AnalyticsMetadata: map[string]any{MetadataKeySoapOperation: operation},
	}
}

// publishOperation writes the resolved operation into the shared context so later
// policies in the chain (and later phases) can read it.
func publishOperation(shared *policy.SharedContext, operation, action string) {
	if shared == nil || shared.Metadata == nil || operation == "" {
		return
	}
	shared.Metadata[MetadataKeySoapOperation] = operation
	if action != "" {
		shared.Metadata[MetadataKeySoapAction] = action
	}
}

// actionFromHeaders extracts the SOAP action from the SOAPAction header (1.1) or
// the Content-Type action parameter (1.2). Returns "" when absent/empty.
func actionFromHeaders(headers *policy.Headers) string {
	if vals := headers.Get("soapaction"); len(vals) > 0 {
		if action := strings.Trim(strings.TrimSpace(vals[0]), `"`); action != "" {
			return action
		}
	}
	if vals := headers.Get("content-type"); len(vals) > 0 {
		if _, mtParams, err := mime.ParseMediaType(vals[0]); err == nil {
			if action := strings.TrimSpace(mtParams["action"]); action != "" {
				return action
			}
		}
	}
	return ""
}

// declaredOperation is one entry of the params.operations mapping table.
type declaredOperation struct {
	name       string
	soapAction string
}

// declaredOperations decodes params.operations ([]{name, soapAction}).
func declaredOperations(params map[string]interface{}) []declaredOperation {
	raw, ok := params["operations"].([]interface{})
	if !ok {
		return nil
	}
	ops := make([]declaredOperation, 0, len(raw))
	for _, entry := range raw {
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		var op declaredOperation
		if v, ok := m["name"].(string); ok {
			op.name = v
		}
		if v, ok := m["soapAction"].(string); ok {
			op.soapAction = v
		}
		if op.name != "" || op.soapAction != "" {
			ops = append(ops, op)
		}
	}
	return ops
}

// resolveOperationName maps a raw action and/or body element to a logical operation
// name using the declared operations; falls back to the raw value when undeclared.
func resolveOperationName(action, bodyElement string, ops []declaredOperation) string {
	if action != "" {
		for _, op := range ops {
			if op.soapAction != "" && op.soapAction == action && op.name != "" {
				return op.name
			}
		}
		return action
	}
	if bodyElement != "" {
		for _, op := range ops {
			if op.name != "" && strings.EqualFold(op.name, bodyElement) {
				return op.name
			}
		}
		return bodyElement
	}
	return ""
}

// validateEnvelopeEnabled reads params.validateEnvelope (default true).
func validateEnvelopeEnabled(params map[string]interface{}) bool {
	if v, ok := params["validateEnvelope"].(bool); ok {
		return v
	}
	return true
}

// firstBodyElement parses a SOAP envelope and returns the local name of the first
// child element of the Body. Namespace prefixes are ignored, so both SOAP 1.1 and
// 1.2 envelopes are handled. Returns an error for malformed XML or when the
// document is not an Envelope containing a Body.
func firstBodyElement(body []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))

	depth := 0
	inBody := false
	for {
		tok, err := decoder.Token()
		if err != nil {
			if inBody {
				return "", errMalformed("SOAP Body contains no operation element")
			}
			return "", errMalformed("request body is not a well-formed SOAP envelope")
		}

		start, ok := tok.(xml.StartElement)
		if !ok {
			if _, isEnd := tok.(xml.EndElement); isEnd && inBody {
				// </Body> reached without a child element.
				return "", errMalformed("SOAP Body contains no operation element")
			}
			continue
		}

		depth++
		switch depth {
		case 1:
			if !strings.EqualFold(start.Name.Local, "Envelope") {
				return "", errMalformed("root element is not a SOAP Envelope")
			}
		case 2:
			if strings.EqualFold(start.Name.Local, "Header") {
				// Skip the entire Header subtree.
				if err := decoder.Skip(); err != nil {
					return "", errMalformed("malformed SOAP Header")
				}
				depth--
				continue
			}
			if strings.EqualFold(start.Name.Local, "Body") {
				inBody = true
				continue
			}
			// Unknown level-2 element — skip it.
			if err := decoder.Skip(); err != nil {
				return "", errMalformed("malformed SOAP envelope")
			}
			depth--
		case 3:
			if inBody {
				return start.Name.Local, nil
			}
			if err := decoder.Skip(); err != nil {
				return "", errMalformed("malformed SOAP envelope")
			}
			depth--
		}
	}
}

type soapDispatchError string

func (e soapDispatchError) Error() string { return string(e) }

func errMalformed(msg string) error { return soapDispatchError(msg) }

// malformedEnvelopeResponse rejects the request. The body is plain JSON here; the
// policy-engine kernel rewrites error responses for SOAP APIs into SOAP Faults.
func malformedEnvelopeResponse(reason string) policy.RequestAction {
	return policy.ImmediateResponse{
		StatusCode: 400,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       []byte(`{"error":"Bad Request","message":"` + strings.ReplaceAll(reason, `"`, `'`) + `"}`),
	}
}
