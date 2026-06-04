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
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/utils"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
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

	// soap-dispatch system policy: resolves the logical SOAP operation (SOAPAction
	// header / Content-Type action parameter / body QName) and publishes it to the
	// shared policy context + analytics metadata. Attached to the POST (SOAP
	// invocation) route ONLY — the GET (?wsdl) route carries no body and must not
	// be rejected by envelope validation.
	soapDispatch := soapDispatchPolicyInstance(apiData)

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

		// Policy chain: [soap-dispatch (POST only)] + API-level policies, with
		// system policies (e.g. analytics) injected in front.
		chain := t.rest.buildPolicyChain(apiPolicies, apiData.Policies, nil)
		if method == "POST" {
			chain = append([]policyenginev1.PolicyInstance{soapDispatch}, chain...)
		}
		injected := utils.InjectSystemPolicies(chain, t.systemConfig, nil)
		rdc.PolicyChains[routeKey] = sdkChainToModel(injected)
	}

	return rdc, nil
}

// soapDispatchPolicyInstance builds the soap-dispatch system policy instance carrying
// the API's declared operations and SOAP version as parameters, so the policy can map
// raw SOAPAction values / body elements to logical operation names at runtime.
func soapDispatchPolicyInstance(apiData api.SoapAPIData) policyenginev1.PolicyInstance {
	params := map[string]interface{}{
		"validateEnvelope": true,
	}
	if apiData.SoapVersion != nil {
		params["soapVersion"] = string(*apiData.SoapVersion)
	}
	if apiData.Operations != nil && len(*apiData.Operations) > 0 {
		ops := make([]interface{}, 0, len(*apiData.Operations))
		for _, op := range *apiData.Operations {
			entry := map[string]interface{}{"name": op.Name}
			if op.SoapAction != nil {
				entry["soapAction"] = *op.SoapAction
			}
			ops = append(ops, entry)
		}
		params["operations"] = ops
	}
	return policyenginev1.PolicyInstance{
		Name:       constants.SOAP_DISPATCH_SYSTEM_POLICY_NAME,
		Version:    constants.SOAP_DISPATCH_SYSTEM_POLICY_VERSION,
		Enabled:    true,
		Parameters: params,
	}
}
