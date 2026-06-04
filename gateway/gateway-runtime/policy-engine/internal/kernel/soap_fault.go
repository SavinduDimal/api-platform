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
	"fmt"
	"net/http"
	"strings"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// apiKindSoapApi is the artifact kind string for SOAP APIs as delivered in the
// policy xDS route metadata. Kept as a local constant because the policy SDK's
// APIKind enum does not (yet) include SOAP.
const apiKindSoapApi = "SoapApi"

const (
	soap11ContentType = "text/xml; charset=utf-8"
	soap12ContentType = "application/soap+xml; charset=utf-8"

	soapVersion11 = "1.1"
	soapVersion12 = "1.2"
)

// soapVersionFromContentType infers the SOAP version a client speaks from its
// request Content-Type: application/soap+xml → SOAP 1.2, anything else → SOAP 1.1.
// Inferring per-request (rather than from API config) means a fault is always
// rendered in the dialect the client used.
func soapVersionFromContentType(contentType string) string {
	if strings.Contains(strings.ToLower(contentType), "application/soap+xml") {
		return soapVersion12
	}
	return soapVersion11
}

// soapFaultContentType returns the response Content-Type for a fault in the given SOAP version.
func soapFaultContentType(version string) string {
	if version == soapVersion12 {
		return soap12ContentType
	}
	return soap11ContentType
}

// soapFaultCode maps an HTTP status code to the SOAP fault code for the given
// version: 4xx → Client/Sender (caller's fault), everything else → Server/Receiver.
func soapFaultCode(version string, statusCode int) string {
	clientFault := statusCode >= 400 && statusCode < 500
	if version == soapVersion12 {
		if clientFault {
			return "env:Sender"
		}
		return "env:Receiver"
	}
	if clientFault {
		return "soap:Client"
	}
	return "soap:Server"
}

// buildSoapFault renders a SOAP Fault envelope for the given version. faultString
// and detail are XML-escaped; detail is omitted when empty.
func buildSoapFault(version, faultCode, faultString, detail string) []byte {
	esc := escapeXMLText
	if version == soapVersion12 {
		var detailXML string
		if detail != "" {
			detailXML = "<env:Detail><detail>" + esc(detail) + "</detail></env:Detail>"
		}
		return []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope"><env:Body><env:Fault>` +
			`<env:Code><env:Value>` + faultCode + `</env:Value></env:Code>` +
			`<env:Reason><env:Text xml:lang="en">` + esc(faultString) + `</env:Text></env:Reason>` +
			detailXML +
			`</env:Fault></env:Body></env:Envelope>`)
	}

	var detailXML string
	if detail != "" {
		detailXML = "<detail>" + esc(detail) + "</detail>"
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><soap:Fault>` +
		`<faultcode>` + faultCode + `</faultcode>` +
		`<faultstring>` + esc(faultString) + `</faultstring>` +
		detailXML +
		`</soap:Fault></soap:Body></soap:Envelope>`)
}

// escapeXMLText escapes a string for inclusion as XML character data.
func escapeXMLText(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

// applySOAPFaultFormat rewrites an error ImmediateResponse into a SOAP Fault when
// the request belongs to a SOAP API. The HTTP status code and existing headers are
// preserved (so 401 WWW-Authenticate / 429 Retry-After transport semantics keep
// working); only the body and Content-Type are replaced. Responses are returned
// unchanged when:
//   - the API kind is not SoapApi,
//   - the status is not an error (< 400), or
//   - the policy already produced an XML body (a hand-crafted fault).
func applySOAPFaultFormat(immResp policy.ImmediateResponse, execCtx *PolicyExecutionContext) policy.ImmediateResponse {
	if execCtx == nil || execCtx.sharedCtx == nil || string(execCtx.sharedCtx.APIKind) != apiKindSoapApi {
		return immResp
	}
	if immResp.StatusCode < 400 {
		return immResp
	}
	// Respect a policy that already crafted an XML/SOAP response body.
	for k, v := range immResp.Headers {
		if strings.EqualFold(k, "content-type") && strings.Contains(strings.ToLower(v), "xml") {
			return immResp
		}
	}

	version := soapVersion11
	if execCtx.requestHeaderCtx != nil && execCtx.requestHeaderCtx.Headers != nil {
		if cts := execCtx.requestHeaderCtx.Headers.Get("content-type"); len(cts) > 0 {
			version = soapVersionFromContentType(cts[0])
		}
	}

	return soapFaultImmediateResponse(immResp, version)
}

// soapFaultImmediateResponse converts immResp's body into a SOAP Fault envelope of the
// given version, moving the original body (if any) into the fault detail element.
func soapFaultImmediateResponse(immResp policy.ImmediateResponse, version string) policy.ImmediateResponse {
	faultString := http.StatusText(immResp.StatusCode)
	if faultString == "" {
		faultString = fmt.Sprintf("HTTP %d", immResp.StatusCode)
	}

	headers := make(map[string]string, len(immResp.Headers)+1)
	for k, v := range immResp.Headers {
		if strings.EqualFold(k, "content-type") {
			continue // replaced below
		}
		headers[k] = v
	}
	headers["content-type"] = soapFaultContentType(version)

	immResp.Headers = headers
	immResp.Body = buildSoapFault(version, soapFaultCode(version, immResp.StatusCode), faultString, string(immResp.Body))
	return immResp
}

// rawRequestContentType extracts the content-type header from a raw ext_proc
// RequestHeaders message. Used for engine-generated errors that occur before an
// execution context exists (e.g. no policy chain found).
func rawRequestContentType(req *extprocv3.ProcessingRequest) string {
	headers, ok := req.Request.(*extprocv3.ProcessingRequest_RequestHeaders)
	if !ok || headers.RequestHeaders == nil || headers.RequestHeaders.Headers == nil {
		return ""
	}
	for _, h := range headers.RequestHeaders.Headers.Headers {
		if strings.EqualFold(h.Key, "content-type") {
			if len(h.RawValue) > 0 {
				return string(h.RawValue)
			}
			return h.Value
		}
	}
	return ""
}
