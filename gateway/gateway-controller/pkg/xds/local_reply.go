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
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	accesslog "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	celfilter "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/filters/cel/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	anypb "google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/errorconfig"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
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

// celAccessLogFilterName is the extension name of Envoy's CEL access-log
// filter (a standard extension in stock envoyproxy/envoy builds).
const celAccessLogFilterName = "envoy.access_loggers.extension_filters.cel"

// perAPILocalReply carries one API's error-response customization for
// Envoy-generated errors, matched to the API's routes.
type perAPILocalReply struct {
	apiUUID    string
	routeNames []string
	doc        *errorconfig.ErrorResponses
}

// buildLocalReplyConfig builds the HCM local_reply_config from the per-API
// and global error-response customization, or nil when nothing applies.
//
// Mapper order matters — Envoy applies the FIRST matching mapper — so
// per-API mappers precede the global ones, giving per-API > global
// precedence for Envoy-generated errors, mirroring the engine's precedence
// for policy errors.
//
// Per-API mappers scope by route: an AndFilter of the response-flag filter
// and a CEL filter on xds.route_name (the route is always matched before an
// upstream flag can be set). A route-injected request header was considered
// and rejected: route header finalization happens too late for UH replies
// and would leak an internal header to backends on every successful request.
//
// Bodies are rendered STATICALLY at xDS build time (ResolveStatic): unlike
// the policy engine's request-time rendering, there is no Accept negotiation
// and only build-time placeholders (${statusCode}, ${message},
// ${category}) have values. Envoy's own %COMMAND% operators (e.g.
// %RESPONSE_FLAGS% in the fault-flag header) are a third, Envoy-side
// rendering context.
func (t *Translator) buildLocalReplyConfig(perAPI []perAPILocalReply) *hcm.LocalReplyConfig {
	defaultMediaType := t.defaultErrorMediaType()
	var mappers []*hcm.ResponseMapper

	// Per-API mappers first (per-API > global). These apply whenever the API
	// defines errorResponses, independent of the global [error_handling]
	// flag — matching the engine-side per-API behavior.
	for _, apiEntry := range perAPI {
		if apiEntry.doc == nil || len(apiEntry.routeNames) == 0 {
			continue
		}
		routeFilter, err := celRouteNameFilter(apiEntry.routeNames)
		if err != nil {
			t.logger.Error("Failed to build per-API local-reply route filter; falling back to global customization",
				slog.String("api_uuid", apiEntry.apiUUID),
				slog.Any("error", err))
			continue
		}
		for _, mapping := range localReplyMappings {
			res, ok := apiEntry.doc.ResolveStatic(mapping.status, defaultMediaType)
			if !ok {
				continue
			}
			filter := &accesslog.AccessLogFilter{
				FilterSpecifier: &accesslog.AccessLogFilter_AndFilter{
					AndFilter: &accesslog.AndFilter{
						Filters: []*accesslog.AccessLogFilter{responseFlagFilter(mapping.flags), routeFilter},
					},
				},
			}
			mappers = append(mappers, newLocalReplyMapper(filter, mapping.status, res))
		}
	}

	// Global mappers follow, catching every API without its own entry.
	if t.errorResponses != nil {
		for _, mapping := range localReplyMappings {
			res, ok := t.errorResponses.ResolveStatic(mapping.status, defaultMediaType)
			if !ok {
				continue
			}
			mappers = append(mappers, newLocalReplyMapper(responseFlagFilter(mapping.flags), mapping.status, res))
		}
	}

	if len(mappers) == 0 {
		return nil
	}
	return &hcm.LocalReplyConfig{Mappers: mappers}
}

// responseFlagFilter builds an access-log filter matching Envoy response flags.
func responseFlagFilter(flags []string) *accesslog.AccessLogFilter {
	return &accesslog.AccessLogFilter{
		FilterSpecifier: &accesslog.AccessLogFilter_ResponseFlagFilter{
			ResponseFlagFilter: &accesslog.ResponseFlagFilter{Flags: flags},
		},
	}
}

// celRouteNameFilter builds a CEL extension filter matching any of the given
// route names (`xds.route_name in [...]`). Route names are sorted for a
// deterministic xDS snapshot.
func celRouteNameFilter(routeNames []string) (*accesslog.AccessLogFilter, error) {
	names := make([]string, len(routeNames))
	copy(names, routeNames)
	sort.Strings(names)

	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = quoteCELString(name)
	}
	expression := fmt.Sprintf("xds.route_name in [%s]", strings.Join(quoted, ", "))

	celAny, err := anypb.New(&celfilter.ExpressionFilter{Expression: expression})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CEL expression filter: %w", err)
	}
	return &accesslog.AccessLogFilter{
		FilterSpecifier: &accesslog.AccessLogFilter_ExtensionFilter{
			ExtensionFilter: &accesslog.ExtensionFilter{
				Name:       celAccessLogFilterName,
				ConfigType: &accesslog.ExtensionFilter_TypedConfig{TypedConfig: celAny},
			},
		},
	}, nil
}

// quoteCELString renders a string as a CEL string literal.
func quoteCELString(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(s) + `"`
}

// newLocalReplyMapper assembles a ResponseMapper: the statically rendered
// body, its content type, the fault-flag marker header, and a status rewrite
// when x-status-code-override differs from Envoy's original status.
func newLocalReplyMapper(filter *accesslog.AccessLogFilter, originalStatus int, res errorconfig.StaticResolution) *hcm.ResponseMapper {
	mapper := &hcm.ResponseMapper{
		Filter: filter,
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
	if res.StatusCode != originalStatus {
		mapper.StatusCode = wrapperspb.UInt32(uint32(res.StatusCode))
	}
	return mapper
}

// defaultErrorMediaType returns the configured default media type for
// error-response rendering, falling back to application/json.
func (t *Translator) defaultErrorMediaType() string {
	if t.config != nil && t.config.ErrorHandling.DefaultMediaType != "" {
		return t.config.ErrorHandling.DefaultMediaType
	}
	return "application/json"
}

// errorResponsesDoc extracts an API's errorResponses as the errorconfig
// model, or nil when the API defines none. The generated OpenAPI type is
// bridged via its JSON form (already validated at deploy time).
func (t *Translator) errorResponsesDoc(cfg *models.StoredConfig) *errorconfig.ErrorResponses {
	var er *api.ErrorResponses
	switch c := cfg.Configuration.(type) {
	case api.RestAPI:
		er = c.Spec.ErrorResponses
	case *api.RestAPI:
		if c != nil {
			er = c.Spec.ErrorResponses
		}
	case api.SoapAPI:
		er = c.Spec.ErrorResponses
	case *api.SoapAPI:
		if c != nil {
			er = c.Spec.ErrorResponses
		}
	}
	if er == nil || len(er.Responses) == 0 {
		return nil
	}
	data, err := json.Marshal(er)
	if err != nil {
		t.logger.Warn("Failed to serialize errorResponses for local-reply customization",
			slog.String("api_uuid", cfg.UUID), slog.Any("error", err))
		return nil
	}
	doc, err := errorconfig.Parse(data)
	if err != nil {
		t.logger.Warn("Ignoring invalid errorResponses for local-reply customization",
			slog.String("api_uuid", cfg.UUID), slog.Any("error", err))
		return nil
	}
	return doc
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
