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

package soapdispatch

import (
	"context"
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const soap11Envelope = `<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:tns="http://example.com/calc">
  <soap:Header><tns:TrackingHeader><id>1</id></tns:TrackingHeader></soap:Header>
  <soap:Body><tns:AddRequest><a>1</a><b>2</b></tns:AddRequest></soap:Body>
</soap:Envelope>`

const soap12Envelope = `<?xml version="1.0"?>
<env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope" xmlns:tns="http://example.com/calc">
  <env:Body><tns:EchoRequest><message>hi</message></tns:EchoRequest></env:Body>
</env:Envelope>`

func opsParams() map[string]interface{} {
	return map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{"name": "Add", "soapAction": "urn:Add"},
			map[string]interface{}{"name": "AddRequest", "soapAction": ""},
		},
	}
}

func headerCtx(headers map[string][]string) *policy.RequestHeaderContext {
	shared := &policy.SharedContext{Metadata: map[string]interface{}{}}
	return &policy.RequestHeaderContext{
		SharedContext: shared,
		Headers:       policy.NewHeaders(headers),
	}
}

func bodyCtx(shared *policy.SharedContext, body string) *policy.RequestContext {
	if shared == nil {
		shared = &policy.SharedContext{Metadata: map[string]interface{}{}}
	}
	return &policy.RequestContext{
		SharedContext: shared,
		Body:          &policy.Body{Content: []byte(body), Present: true, EndOfStream: true},
	}
}

func TestOnRequestHeaders_SoapActionHeader(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := headerCtx(map[string][]string{
		"soapaction":   {`"urn:Add"`},
		"content-type": {"text/xml; charset=utf-8"},
	})

	p.OnRequestHeaders(context.Background(), ctx, opsParams())

	if got := ctx.SharedContext.Metadata[MetadataKeySoapOperation]; got != "Add" {
		t.Fatalf("expected operation 'Add' (declared name for urn:Add), got %v", got)
	}
	if got := ctx.SharedContext.Metadata[MetadataKeySoapAction]; got != "urn:Add" {
		t.Fatalf("expected raw action 'urn:Add', got %v", got)
	}
}

func TestOnRequestHeaders_ContentTypeActionParam(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := headerCtx(map[string][]string{
		"content-type": {`application/soap+xml; charset=utf-8; action="urn:Add"`},
	})

	p.OnRequestHeaders(context.Background(), ctx, opsParams())

	if got := ctx.SharedContext.Metadata[MetadataKeySoapOperation]; got != "Add" {
		t.Fatalf("expected operation 'Add' from content-type action, got %v", got)
	}
}

func TestOnRequestHeaders_UndeclaredActionPassedThrough(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := headerCtx(map[string][]string{"soapaction": {"urn:Unknown"}})

	p.OnRequestHeaders(context.Background(), ctx, opsParams())

	if got := ctx.SharedContext.Metadata[MetadataKeySoapOperation]; got != "urn:Unknown" {
		t.Fatalf("expected raw action for undeclared op, got %v", got)
	}
}

func TestOnRequestBody_ResolvesFromBodyQName(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := bodyCtx(nil, soap11Envelope)

	p.OnRequestBody(context.Background(), ctx, opsParams())

	// AddRequest is declared (matched by name) → logical name AddRequest.
	if got := ctx.SharedContext.Metadata[MetadataKeySoapOperation]; got != "AddRequest" {
		t.Fatalf("expected operation 'AddRequest' from body QName, got %v", got)
	}
}

func TestOnRequestBody_Soap12Envelope(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := bodyCtx(nil, soap12Envelope)

	p.OnRequestBody(context.Background(), ctx, map[string]interface{}{})

	if got := ctx.SharedContext.Metadata[MetadataKeySoapOperation]; got != "EchoRequest" {
		t.Fatalf("expected operation 'EchoRequest', got %v", got)
	}
}

func TestOnRequestBody_MalformedEnvelopeRejected(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := bodyCtx(nil, `{"not":"xml"}`)

	action := p.OnRequestBody(context.Background(), ctx, map[string]interface{}{})

	imm, ok := action.(policy.ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse for malformed envelope, got %T", action)
	}
	if imm.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", imm.StatusCode)
	}
}

func TestOnRequestBody_NonEnvelopeXMLRejected(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := bodyCtx(nil, `<notSoap><x/></notSoap>`)

	action := p.OnRequestBody(context.Background(), ctx, map[string]interface{}{})

	if _, ok := action.(policy.ImmediateResponse); !ok {
		t.Fatalf("expected ImmediateResponse for non-envelope XML, got %T", action)
	}
}

func TestOnRequestBody_ValidationDisabledSkipsMalformed(t *testing.T) {
	p := &SoapDispatchPolicy{}
	ctx := bodyCtx(nil, `not xml at all`)

	action := p.OnRequestBody(context.Background(), ctx, map[string]interface{}{"validateEnvelope": false})

	if action != nil {
		t.Fatalf("expected nil action when validation disabled, got %T", action)
	}
}

func TestOnRequestBody_AlreadyResolvedValidatesEnvelope(t *testing.T) {
	p := &SoapDispatchPolicy{}
	shared := &policy.SharedContext{Metadata: map[string]interface{}{
		MetadataKeySoapOperation: "Add",
	}}
	ctx := bodyCtx(shared, `garbage`)

	action := p.OnRequestBody(context.Background(), ctx, map[string]interface{}{})

	if _, ok := action.(policy.ImmediateResponse); !ok {
		t.Fatalf("expected malformed envelope rejection even when operation resolved, got %T", action)
	}
}

func TestFirstBodyElement_EmptyBodyElement(t *testing.T) {
	_, err := firstBodyElement([]byte(`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body></soap:Body></soap:Envelope>`))
	if err == nil {
		t.Fatal("expected error for envelope with empty Body")
	}
}
