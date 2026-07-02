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

package xds

import (
	accesslog "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// FaultFlagHeaderName carries Envoy's %RESPONSE_FLAGS% on customized local
// replies. A non-empty value marks the response as Envoy-generated — the
// discriminator Phase 3 (backend error reshaping) needs to tell a genuine
// backend 5xx from an Envoy local reply.
const FaultFlagHeaderName = "x-wso2-response-fault-flag"

// localReplyMapping ties a set of Envoy response flags to the HTTP status
// code Envoy emits for them; the status selects the error-response entry.
type localReplyMapping struct {
	status int
	flags  []string
}

// localReplyMappings enumerates the Envoy-generated errors customized via
// local_reply_config. Mappers match on RESPONSE FLAGS — never on status
// codes — because ext_proc immediate responses (already shaped by the policy
// engine, including per-API overrides) are also delivered as local replies
// and must not be rewritten here; upstream flags are only set for genuine
// upstream events.
//
// Note: 413 (payload too large) has no dedicated response flag, so it is not
// customizable via local replies in this slice.
var localReplyMappings = []localReplyMapping{
	// BACKEND_UNAVAILABLE: no healthy upstream, connect failure, overflow.
	{status: 503, flags: []string{"UH", "UF", "UO"}},
	// BACKEND_TIMEOUT: upstream response timeout.
	{status: 504, flags: []string{"UT"}},
}

// buildLocalReplyConfig builds the HCM local_reply_config from the global
// error-response customization, or nil when nothing applies.
//
// Bodies are rendered STATICALLY at xDS build time (ResolveStatic): unlike
// the policy engine's request-time rendering, there is no Accept negotiation
// and only build-time placeholders ({{statusCode}}, {{message}},
// {{category}}) have values. Envoy's own %COMMAND% operators (e.g.
// %RESPONSE_FLAGS% in the fault-flag header) are a third, Envoy-side
// rendering context.
func (t *Translator) buildLocalReplyConfig() *hcm.LocalReplyConfig {
	if t.errorResponses == nil {
		return nil
	}

	defaultMediaType := t.defaultErrorMediaType()

	var mappers []*hcm.ResponseMapper
	for _, mapping := range localReplyMappings {
		res, ok := t.errorResponses.ResolveStatic(mapping.status, defaultMediaType)
		if !ok {
			continue
		}

		mapper := &hcm.ResponseMapper{
			Filter: &accesslog.AccessLogFilter{
				FilterSpecifier: &accesslog.AccessLogFilter_ResponseFlagFilter{
					ResponseFlagFilter: &accesslog.ResponseFlagFilter{Flags: mapping.flags},
				},
			},
			Body: &core.DataSource{
				Specifier: &core.DataSource_InlineString{InlineString: res.Body},
			},
			BodyFormatOverride: &core.SubstitutionFormatString{
				Format: &core.SubstitutionFormatString_TextFormatSource{
					TextFormatSource: &core.DataSource{
						Specifier: &core.DataSource_InlineString{InlineString: "%LOCAL_REPLY_BODY%"},
					},
				},
				ContentType: res.ContentType,
			},
			HeadersToAdd: []*core.HeaderValueOption{
				{
					Header: &core.HeaderValue{
						Key:   FaultFlagHeaderName,
						Value: "%RESPONSE_FLAGS%",
					},
				},
			},
		}
		if res.StatusCode != mapping.status {
			mapper.StatusCode = wrapperspb.UInt32(uint32(res.StatusCode))
		}
		mappers = append(mappers, mapper)
	}

	if len(mappers) == 0 {
		return nil
	}
	return &hcm.LocalReplyConfig{Mappers: mappers}
}

// defaultErrorMediaType returns the configured default media type for
// error-response rendering, falling back to application/json.
func (t *Translator) defaultErrorMediaType() string {
	if t.config != nil && t.config.ErrorHandling.DefaultMediaType != "" {
		return t.config.ErrorHandling.DefaultMediaType
	}
	return "application/json"
}

// noRouteResponse returns the status, content type, and body for the
// catch-all no-route DirectResponse: the configured 404 entry (or the
// document default) when error customization is enabled, else the built-in
// {"error":"Not Found"}.
func (t *Translator) noRouteResponse() (int, string, string) {
	if res, ok := t.errorResponses.ResolveStatic(404, t.defaultErrorMediaType()); ok {
		return res.StatusCode, res.ContentType, res.Body
	}
	return 404, "application/json", `{"error":"Not Found"}`
}
