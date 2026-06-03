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
