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
	"os"
	"path/filepath"
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

const localReplyErrorDoc = `
responses:
  "404":
    content:
      application/json:
        example: { code: 404, message: "No API matched this request", category: "{{category}}" }
  "503":
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "Backend is unavailable" }
  "504":
    x-status-code-override: 502
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "Upstream did not respond in time" }
`

// errorHandlingTranslator builds a Translator with [error_handling] enabled
// and the given error-responses document.
func errorHandlingTranslator(t *testing.T, doc string) *Translator {
	t.Helper()
	path := filepath.Join(t.TempDir(), "error-responses.yaml")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	cfg := testConfig()
	cfg.ErrorHandling = config.ErrorHandlingConfig{
		Enabled:          true,
		ConfigFile:       path,
		DefaultMediaType: "application/json",
	}
	return NewTranslator(createTestLogger(), testRouterConfig(), nil, cfg)
}

func TestBuildLocalReplyConfig_Disabled(t *testing.T) {
	translator := NewTranslator(createTestLogger(), testRouterConfig(), nil, testConfig())
	assert.Nil(t, translator.buildLocalReplyConfig(),
		"no local_reply_config when error handling is disabled")
}

func TestBuildLocalReplyConfig_InvalidFileDisablesCustomization(t *testing.T) {
	cfg := testConfig()
	cfg.ErrorHandling = config.ErrorHandlingConfig{
		Enabled:          true,
		ConfigFile:       filepath.Join(t.TempDir(), "missing.yaml"),
		DefaultMediaType: "application/json",
	}
	translator := NewTranslator(createTestLogger(), testRouterConfig(), nil, cfg)
	assert.Nil(t, translator.buildLocalReplyConfig())

	// The no-route response falls back to the built-in body.
	status, contentType, body := translator.noRouteResponse()
	assert.Equal(t, 404, status)
	assert.Equal(t, "application/json", contentType)
	assert.Equal(t, `{"error":"Not Found"}`, body)
}

func TestBuildLocalReplyConfig_Mappers(t *testing.T) {
	translator := errorHandlingTranslator(t, localReplyErrorDoc)
	lrc := translator.buildLocalReplyConfig()
	require.NotNil(t, lrc)
	require.Len(t, lrc.Mappers, 2)

	// Mapper 1: BACKEND_UNAVAILABLE flags → 503 entry.
	m503 := lrc.Mappers[0]
	assert.Equal(t, []string{"UH", "UF", "UO"}, m503.GetFilter().GetResponseFlagFilter().GetFlags())
	assert.Nil(t, m503.StatusCode, "503 entry has no override — status untouched")
	assert.Contains(t, m503.GetBody().GetInlineString(), `"code":"503"`)
	assert.Contains(t, m503.GetBody().GetInlineString(), "Backend is unavailable")
	assert.Equal(t, "application/json", m503.GetBodyFormatOverride().GetContentType())
	assert.Equal(t, "%LOCAL_REPLY_BODY%",
		m503.GetBodyFormatOverride().GetTextFormatSource().GetInlineString())
	// Fault-flag header (Phase 3 discriminator) present.
	require.Len(t, m503.GetHeadersToAdd(), 1)
	assert.Equal(t, FaultFlagHeaderName, m503.GetHeadersToAdd()[0].GetHeader().GetKey())
	assert.Equal(t, "%RESPONSE_FLAGS%", m503.GetHeadersToAdd()[0].GetHeader().GetValue())

	// Mapper 2: UT → 504 entry with x-status-code-override 502.
	m504 := lrc.Mappers[1]
	assert.Equal(t, []string{"UT"}, m504.GetFilter().GetResponseFlagFilter().GetFlags())
	require.NotNil(t, m504.StatusCode)
	assert.Equal(t, uint32(502), m504.GetStatusCode().GetValue())
	// The statusCode placeholder reflects the overridden status.
	assert.Contains(t, m504.GetBody().GetInlineString(), `"code":"502"`)
}

func TestBuildLocalReplyConfig_PartialConfig(t *testing.T) {
	// Only 503 configured (no 504, no default) → one mapper.
	translator := errorHandlingTranslator(t, `
responses:
  "503":
    content:
      application/json:
        example: { message: "only 503" }
`)
	lrc := translator.buildLocalReplyConfig()
	require.NotNil(t, lrc)
	require.Len(t, lrc.Mappers, 1)
	assert.Equal(t, []string{"UH", "UF", "UO"}, lrc.Mappers[0].GetFilter().GetResponseFlagFilter().GetFlags())
}

func TestBuildLocalReplyConfig_DefaultEntryCoversAll(t *testing.T) {
	translator := errorHandlingTranslator(t, `
responses:
  default:
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "{{message}}" }
`)
	lrc := translator.buildLocalReplyConfig()
	require.NotNil(t, lrc)
	require.Len(t, lrc.Mappers, 2)
	assert.Contains(t, lrc.Mappers[0].GetBody().GetInlineString(), `"code":"503"`)
	assert.Contains(t, lrc.Mappers[1].GetBody().GetInlineString(), `"code":"504"`)
}

// TestCreateListener_LocalReplyConfig locks in the generated HCM: the
// local_reply_config is present when error handling is enabled and absent
// otherwise.
func TestCreateListener_LocalReplyConfig(t *testing.T) {
	extractHCM := func(t *testing.T, translator *Translator) *hcm.HttpConnectionManager {
		t.Helper()
		listener, _, err := translator.createListener(nil, false)
		require.NoError(t, err)
		for _, chain := range listener.FilterChains {
			for _, filter := range chain.Filters {
				if filter.Name == wellknown.HTTPConnectionManager {
					manager := &hcm.HttpConnectionManager{}
					require.NoError(t, filter.GetTypedConfig().UnmarshalTo(manager))
					return manager
				}
			}
		}
		t.Fatal("HTTP connection manager not found on listener")
		return nil
	}

	t.Run("enabled", func(t *testing.T) {
		manager := extractHCM(t, errorHandlingTranslator(t, localReplyErrorDoc))
		require.NotNil(t, manager.LocalReplyConfig)
		assert.Len(t, manager.LocalReplyConfig.Mappers, 2)
	})

	t.Run("disabled", func(t *testing.T) {
		manager := extractHCM(t, NewTranslator(createTestLogger(), testRouterConfig(), nil, testConfig()))
		assert.Nil(t, manager.LocalReplyConfig, "disabled config must not change the HCM")
	})
}

func TestNoRouteResponse_Custom(t *testing.T) {
	translator := errorHandlingTranslator(t, localReplyErrorDoc)
	status, contentType, body := translator.noRouteResponse()
	assert.Equal(t, 404, status)
	assert.Equal(t, "application/json", contentType)
	assert.Contains(t, body, "No API matched this request")
	assert.Contains(t, body, `"category":"ROUTE_NOT_FOUND"`)
}

// TestTranslateConfigs_NoRouteDirectResponse verifies the catch-all route in
// the generated route configuration carries the customized 404 body (and the
// built-in body when customization is disabled).
func TestTranslateConfigs_NoRouteDirectResponse(t *testing.T) {
	soapAPI := api.SoapAPI{
		Kind:     api.SoapAPIKindSoapApi,
		Metadata: api.Metadata{Name: "calc-v1"},
		Spec: api.SoapAPIData{
			DisplayName: "Calc",
			Context:     "/calc/v1",
			Version:     "v1.0",
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

	findNoRoute := func(t *testing.T, translator *Translator) *routev3.Route {
		t.Helper()
		resources, err := translator.TranslateConfigs([]*models.StoredConfig{stored}, "test")
		require.NoError(t, err)
		routeResources := resources[resourcev3.RouteType]
		require.Len(t, routeResources, 1)
		rc, ok := routeResources[0].(*routev3.RouteConfiguration)
		require.True(t, ok)
		for _, vh := range rc.VirtualHosts {
			for _, r := range vh.Routes {
				if r.Name == "no-api-found" {
					return r
				}
			}
		}
		t.Fatal("no-api-found catch-all route not found")
		return nil
	}

	t.Run("customized", func(t *testing.T) {
		r := findNoRoute(t, errorHandlingTranslator(t, localReplyErrorDoc))
		dr := r.GetDirectResponse()
		require.NotNil(t, dr)
		assert.Equal(t, uint32(404), dr.GetStatus())
		assert.Contains(t, dr.GetBody().GetInlineString(), "No API matched this request")
	})

	t.Run("built-in fallback", func(t *testing.T) {
		r := findNoRoute(t, NewTranslator(createTestLogger(), testRouterConfig(), nil, testConfig()))
		dr := r.GetDirectResponse()
		require.NotNil(t, dr)
		assert.Equal(t, uint32(404), dr.GetStatus())
		assert.Equal(t, `{"error":"Not Found"}`, dr.GetBody().GetInlineString())
	})
}
