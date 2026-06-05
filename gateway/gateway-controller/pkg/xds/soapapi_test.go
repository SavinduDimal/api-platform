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
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// soapURL is a helper for taking the address of a string literal.
func soapURL(s string) *string { return &s }

// TestTranslator_TranslateSoapAPIConfig verifies a SOAP API produces one upstream cluster
// and two routes at the service context (POST for SOAP, GET for ?wsdl passthrough).
func TestTranslator_TranslateSoapAPIConfig(t *testing.T) {
	logger := createTestLogger()
	routerCfg := testRouterConfig()
	cfg := testConfig()
	translator := NewTranslator(logger, routerCfg, nil, cfg)

	soapAPI := api.SoapAPI{
		Kind:     api.SoapAPIKindSoapApi,
		Metadata: api.Metadata{Name: "stockquote-v1.0"},
		Spec: api.SoapAPIData{
			DisplayName: "StockQuote API",
			Context:     "/stockquote/v1.0",
			Version:     "v1.0",
			Upstream: struct {
				Main    api.Upstream  `json:"main" yaml:"main"`
				Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
			}{
				Main: api.Upstream{Url: soapURL("http://stockquote-service:8080/services/StockQuote")},
			},
		},
	}

	stored := &models.StoredConfig{
		UUID:          "stockquote-v1.0",
		Kind:          string(api.SoapAPIKindSoapApi),
		Configuration: soapAPI,
	}

	routes, clusters, err := translator.translateSoapAPIConfig(stored, []*models.StoredConfig{stored})
	require.NoError(t, err)

	// Exactly one backend cluster.
	require.Len(t, clusters, 1)

	// Two routes: POST + GET, both on the default "localhost" vhost.
	require.Len(t, routes, 2)

	methods := map[string]bool{}
	for _, r := range routes {
		require.NotNil(t, r.GetMatch())
		require.NotEmpty(t, r.GetMatch().GetHeaders(), "route should match on :method header")
		hm := r.GetMatch().GetHeaders()[0]
		assert.Equal(t, ":method", hm.GetName())
		methods[hm.GetStringMatch().GetExact()] = true

		// Each route forwards to the single backend cluster (static cluster, not cluster_header).
		assert.Equal(t, clusters[0].GetName(), r.GetRoute().GetCluster())
	}

	assert.True(t, methods["POST"], "expected a POST route for SOAP invocations")
	assert.True(t, methods["GET"], "expected a GET route for ?wsdl passthrough")
}

// TestTranslator_TranslateSoapAPIConfig_PerOperationRoutes verifies that declared
// operations with a non-empty soapAction get dedicated routes matching the SOAPAction
// header, while empty-soapAction operations do not.
func TestTranslator_TranslateSoapAPIConfig_PerOperationRoutes(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())

	addAction := "urn:Add"
	emptyAction := ""
	soapAPI := api.SoapAPI{
		Kind:     api.SoapAPIKindSoapApi,
		Metadata: api.Metadata{Name: "calc-v1"},
		Spec: api.SoapAPIData{
			DisplayName: "Calc",
			Context:     "/calc/v1",
			Version:     "v1.0",
			Operations: &[]api.SoapOperation{
				{Name: "Add", SoapAction: &addAction},
				{Name: "DocLiteralOp", SoapAction: &emptyAction},
			},
			Upstream: struct {
				Main    api.Upstream  `json:"main" yaml:"main"`
				Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
			}{
				Main: api.Upstream{Url: soapURL("http://backend:8080/svc")},
			},
		},
	}

	stored := &models.StoredConfig{
		UUID:          "calc-v1",
		Kind:          string(api.SoapAPIKindSoapApi),
		Configuration: soapAPI,
	}

	routes, clusters, err := translator.translateSoapAPIConfig(stored, nil)
	require.NoError(t, err)
	require.Len(t, clusters, 1)

	// 2 base routes (POST + GET) + 1 per-op route for "Add" (empty soapAction skipped).
	require.Len(t, routes, 3)

	perOpName := GenerateSoapOperationRouteName("/calc/v1", "v1.0", "localhost", "urn:Add")
	var perOpRoute *struct {
		headers int
		regex   string
	}
	for _, r := range routes {
		if r.GetName() == perOpName {
			hs := r.GetMatch().GetHeaders()
			var soapActionRegex string
			for _, h := range hs {
				if h.GetName() == "soapaction" {
					soapActionRegex = h.GetStringMatch().GetSafeRegex().GetRegex()
				}
			}
			perOpRoute = &struct {
				headers int
				regex   string
			}{headers: len(hs), regex: soapActionRegex}
		}
	}
	require.NotNil(t, perOpRoute, "expected a per-operation route named %s", perOpName)

	// :method + soapaction matchers → sorter ranks it above the generic POST route.
	assert.Equal(t, 2, perOpRoute.headers)
	// Quoted and unquoted SOAPAction values both accepted.
	assert.Equal(t, `^"?urn:Add"?$`, perOpRoute.regex)
}

// TestTranslateConfigs_SoapPerOpRouteInVirtualHost locks in the FULL pipeline:
// per-operation SOAP routes carry 4-segment names (METHOD|PATH|VHOST|soapAction=...)
// and must survive the vhost grouping in TranslateConfigs and appear in the shared
// route configuration, sorted above the generic POST route. Regression test for the
// grouping logic that previously required exactly 3 name segments and silently
// dropped per-operation routes.
func TestTranslateConfigs_SoapPerOpRouteInVirtualHost(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())

	addAction := "urn:Add"
	soapAPI := api.SoapAPI{
		Kind:     api.SoapAPIKindSoapApi,
		Metadata: api.Metadata{Name: "calc-v1"},
		Spec: api.SoapAPIData{
			DisplayName: "Calc",
			Context:     "/calc/v1",
			Version:     "v1.0",
			Operations: &[]api.SoapOperation{
				{Name: "Add", SoapAction: &addAction},
			},
			Upstream: struct {
				Main    api.Upstream  `json:"main" yaml:"main"`
				Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
			}{
				Main: api.Upstream{Url: soapURL("http://backend:8080/svc")},
			},
		},
	}

	stored := &models.StoredConfig{
		UUID:          "calc-v1",
		Kind:          string(api.SoapAPIKindSoapApi),
		DesiredState:  models.StateDeployed,
		Configuration: soapAPI,
	}

	resources, err := translator.TranslateConfigs([]*models.StoredConfig{stored}, "test-correlation")
	require.NoError(t, err)

	routeResources := resources[resourcev3.RouteType]
	require.Len(t, routeResources, 1)
	rc, ok := routeResources[0].(*routev3.RouteConfiguration)
	require.True(t, ok)

	perOpName := GenerateSoapOperationRouteName("/calc/v1", "v1.0", "localhost", "urn:Add")
	genericName := GenerateRouteName("POST", "/calc/v1", "v1.0", "/", "localhost")

	perOpIdx, genericIdx := -1, -1
	for _, vh := range rc.VirtualHosts {
		for i, r := range vh.Routes {
			switch r.Name {
			case perOpName:
				perOpIdx = i
			case genericName:
				genericIdx = i
			}
		}
	}

	require.NotEqual(t, -1, perOpIdx, "per-operation route %q must be present in a virtual host", perOpName)
	require.NotEqual(t, -1, genericIdx, "generic POST route %q must be present in a virtual host", genericName)
	// More header matchers → the sorter must place the per-op route above the generic one.
	assert.Less(t, perOpIdx, genericIdx, "per-operation route must sort above the generic POST route")
}

// TestTranslator_TranslateSoapAPIConfig_WrongKind verifies a non-SOAP config is rejected.
func TestTranslator_TranslateSoapAPIConfig_WrongKind(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())

	stored := &models.StoredConfig{
		UUID:          "x",
		Kind:          string(api.SoapAPIKindSoapApi),
		Configuration: api.RestAPI{Kind: api.RestAPIKindRestApi},
	}

	_, _, err := translator.translateSoapAPIConfig(stored, nil)
	require.Error(t, err)
}
