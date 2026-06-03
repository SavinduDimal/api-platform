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
	"fmt"
	"strings"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/utils"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
)

// SoapAPITransformer transforms a StoredConfig (SoapApi kind) into a RuntimeDeployConfig.
//
// Phase 1 (SOAP passthrough): a SOAP API exposes a single service endpoint at the API
// context path. This produces one upstream cluster plus two routes at that context —
// POST (carries SOAP envelopes) and GET (serves ?wsdl / ?xsd retrieval) — both proxied
// unchanged to the backend SOAP service.
//
// It reuses RestAPITransformer's upstream-resolution and policy helpers so that policy
// chains, system-policy injection (e.g. analytics), and the route metadata consumed by the
// policy engine behave consistently with REST. Per-operation SOAPAction routing is added in
// a later phase.
type SoapAPITransformer struct {
	rest         *RestAPITransformer
	systemConfig *config.Config
}

// NewSoapAPITransformer creates a new SoapAPITransformer.
func NewSoapAPITransformer(
	routerConfig *config.RouterConfig,
	systemConfig *config.Config,
	policyDefinitions map[string]models.PolicyDefinition,
) *SoapAPITransformer {
	return &SoapAPITransformer{
		rest:         NewRestAPITransformer(routerConfig, systemConfig, policyDefinitions),
		systemConfig: systemConfig,
	}
}

// Transform converts a StoredConfig with SoapAPI configuration into a RuntimeDeployConfig.
func (t *SoapAPITransformer) Transform(cfg *models.StoredConfig) (*models.RuntimeDeployConfig, error) {
	soapCfg, ok := cfg.Configuration.(api.SoapAPI)
	if !ok {
		return nil, fmt.Errorf("configuration is not a SoapAPI")
	}
	apiData := soapCfg.Spec

	rdc := &models.RuntimeDeployConfig{
		Metadata: models.Metadata{
			UUID:        cfg.UUID,
			Kind:        cfg.Kind,
			Handle:      cfg.Handle,
			Version:     apiData.Version,
			DisplayName: apiData.DisplayName,
			ProjectID:   extractProjectID(cfg),
		},
		Context:             strings.ReplaceAll(apiData.Context, "$version", apiData.Version),
		PolicyChainResolver: "route-key",
		Routes:              make(map[string]*models.Route),
		PolicyChains:        make(map[string]*models.PolicyChain),
		UpstreamClusters:    make(map[string]*models.UpstreamCluster),
		SensitiveValues:     cfg.SensitiveValues,
	}

	// Effective vhost (fall back to the gateway default when not specified).
	effectiveMainVHost := t.rest.routerConfig.VHosts.Main.Default
	if apiData.Vhosts != nil && apiData.Vhosts.Main != nil && strings.TrimSpace(*apiData.Vhosts.Main) != "" {
		effectiveMainVHost = *apiData.Vhosts.Main
	}

	// Build the backend SOAP service cluster.
	mainUpstream, err := t.rest.addUpstreamCluster(rdc, "main", &apiData.Upstream.Main, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve main upstream: %w", err)
	}

	mainAutoHostRewrite := true
	if apiData.Upstream.Main.HostRewrite != nil && *apiData.Upstream.Main.HostRewrite == api.Manual {
		mainAutoHostRewrite = false
	}

	// Collect validated API-level policies (applied to all SOAP operations).
	apiPolicies := t.rest.collectAPIPolicies(apiData.Policies)

	// A SOAP API has a single service resource at the context path. Create a route per HTTP
	// method we accept: POST for SOAP invocations and GET for ?wsdl/?xsd passthrough. The "/"
	// operation path makes the route match the context exactly; the query string (?wsdl) is
	// preserved on passthrough by the upstream.
	for _, method := range []string{"POST", "GET"} {
		routeKey := xds.GenerateRouteName(method, apiData.Context, apiData.Version, "/", effectiveMainVHost)

		rdc.Routes[routeKey] = &models.Route{
			Method:          method,
			Path:            xds.ConstructFullPath(apiData.Context, apiData.Version, "/"),
			OperationPath:   "/",
			Vhost:           effectiveMainVHost,
			AutoHostRewrite: mainAutoHostRewrite,
			Upstream: models.RouteUpstream{
				ClusterKey: mainUpstream.ClusterKey,
			},
		}

		// Policy chain: API-level policies + injected system policies (e.g. analytics).
		chain := t.rest.buildPolicyChain(apiPolicies, apiData.Policies, nil)
		injected := utils.InjectSystemPolicies(chain, t.systemConfig, nil)
		rdc.PolicyChains[routeKey] = sdkChainToModel(injected)
	}

	return rdc, nil
}
