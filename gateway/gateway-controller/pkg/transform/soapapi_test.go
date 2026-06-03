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

package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
)

// makeSoapAPIStoredConfig builds a minimal SoapAPI StoredConfig for transformer tests.
func makeSoapAPIStoredConfig(apiPolicies []api.Policy) *models.StoredConfig {
	var specPolicies *[]api.Policy
	if apiPolicies != nil {
		specPolicies = &apiPolicies
	}

	apiData := api.SoapAPIData{
		DisplayName: "StockQuote API",
		Context:     "/stockquote/v1.0",
		Version:     "v1.0",
		Policies:    specPolicies,
		Upstream: struct {
			Main    api.Upstream  `json:"main" yaml:"main"`
			Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
		}{
			Main: api.Upstream{Url: ptrStr("http://stockquote-service:8080/services/StockQuote")},
		},
	}

	soapAPI := api.SoapAPI{
		Kind:     api.SoapAPIKindSoapApi,
		Metadata: api.Metadata{Name: "stockquote-v1.0"},
		Spec:     apiData,
	}

	return &models.StoredConfig{
		UUID:          "stockquote-v1.0",
		Kind:          string(api.SoapAPIKindSoapApi),
		Configuration: soapAPI,
	}
}

// TestSoapAPITransformer_PassthroughRoutes verifies a SOAP API produces a single upstream
// cluster and two routes at the service context — POST (SOAP) and GET (?wsdl) — carrying
// the SoapApi kind metadata.
func TestSoapAPITransformer_PassthroughRoutes(t *testing.T) {
	transformer := NewSoapAPITransformer(testRouterCfg(), &config.Config{}, nil)

	rdc, err := transformer.Transform(makeSoapAPIStoredConfig(nil))
	require.NoError(t, err)

	assert.Equal(t, "SoapApi", rdc.Metadata.Kind)
	assert.Equal(t, "/stockquote/v1.0", rdc.Context)

	// Exactly one upstream cluster (the backend SOAP service).
	require.Len(t, rdc.UpstreamClusters, 1)

	// Wildcard POST + GET routes at the context, on the default main vhost.
	postKey := xds.GenerateRouteName("POST", "/stockquote/v1.0", "v1.0", "/*", "main.local")
	getKey := xds.GenerateRouteName("GET", "/stockquote/v1.0", "v1.0", "/*", "main.local")

	require.Contains(t, rdc.Routes, postKey)
	require.Contains(t, rdc.Routes, getKey)
	assert.Len(t, rdc.Routes, 2)

	assert.Equal(t, "POST", rdc.Routes[postKey].Method)
	assert.Equal(t, "GET", rdc.Routes[getKey].Method)
	assert.Equal(t, "/*", rdc.Routes[postKey].OperationPath)
}

// TestSoapAPITransformer_APILevelPolicyInChain verifies API-level policies are applied to
// every SOAP service route.
func TestSoapAPITransformer_APILevelPolicyInChain(t *testing.T) {
	defs := map[string]models.PolicyDefinition{
		"api-key-auth|v1.0.0": {Name: "api-key-auth", Version: "v1.0.0"},
	}
	transformer := NewSoapAPITransformer(testRouterCfg(), &config.Config{}, defs)

	cfg := makeSoapAPIStoredConfig([]api.Policy{{Name: "api-key-auth", Version: "v1"}})
	rdc, err := transformer.Transform(cfg)
	require.NoError(t, err)

	postKey := xds.GenerateRouteName("POST", "/stockquote/v1.0", "v1.0", "/*", "main.local")
	assert.True(t, findPolicyInChain(rdc, postKey, "api-key-auth"),
		"API-level policy should be present in the SOAP POST route chain")
}

// TestSoapAPITransformer_WrongKind verifies a non-SOAP configuration is rejected.
func TestSoapAPITransformer_WrongKind(t *testing.T) {
	transformer := NewSoapAPITransformer(testRouterCfg(), &config.Config{}, nil)
	_, err := transformer.Transform(makeRestAPIStoredConfig(nil, nil))
	require.Error(t, err)
}
