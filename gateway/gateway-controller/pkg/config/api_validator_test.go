/*
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
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

package config

import (
	"strings"
	"testing"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"gopkg.in/yaml.v3"
)

func TestNewAPIValidator(t *testing.T) {
	v := NewAPIValidator()
	if v == nil {
		t.Fatal("NewAPIValidator returned nil")
	}
	if v.pathParamRegex == nil {
		t.Error("pathParamRegex should not be nil")
	}
	if v.versionRegex == nil {
		t.Error("versionRegex should not be nil")
	}
	if v.urlFriendlyNameRegex == nil {
		t.Error("urlFriendlyNameRegex should not be nil")
	}
}

func TestAPIValidator_SetPolicyValidator(t *testing.T) {
	v := NewAPIValidator()
	pv := NewPolicyValidator(nil)

	v.SetPolicyValidator(pv)

	if v.policyValidator != pv {
		t.Error("policyValidator not set correctly")
	}
}

func TestAPIValidator_Validate_UnsupportedType(t *testing.T) {
	v := NewAPIValidator()

	// Test with unsupported type
	errors := v.Validate("invalid type")
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if !strings.Contains(errors[0].Message, "Unsupported configuration type") {
		t.Errorf("expected unsupported type error, got: %s", errors[0].Message)
	}
}

func TestAPIValidator_Validate_PointerAndValue(t *testing.T) {
	v := NewAPIValidator()

	config := createValidRestAPIConfig()

	// Test with pointer
	errorsPtr := v.Validate(config)
	if len(errorsPtr) != 0 {
		t.Errorf("expected no errors for pointer, got %d: %v", len(errorsPtr), errorsPtr)
	}

	// Test with value
	errorsVal := v.Validate(*config)
	if len(errorsVal) != 0 {
		t.Errorf("expected no errors for value, got %d: %v", len(errorsVal), errorsVal)
	}
}

func TestAPIValidator_ValidateAPIVersion(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name       string
		apiVersion api.RestAPIApiVersion
		wantError  bool
	}{
		{
			name:       "Valid API version",
			apiVersion: api.RestAPIApiVersionGatewayApiPlatformWso2Comv1alpha1,
			wantError:  false,
		},
		{
			name:       "Invalid API version",
			apiVersion: "invalid-version",
			wantError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidRestAPIConfig()
			config.ApiVersion = tt.apiVersion

			errors := v.Validate(config)
			hasVersionError := false
			for _, e := range errors {
				if e.Field == "version" {
					hasVersionError = true
					break
				}
			}
			if tt.wantError && !hasVersionError {
				t.Error("expected version error, got none")
			}
			if !tt.wantError && hasVersionError {
				t.Error("unexpected version error")
			}
		})
	}
}

func TestAPIValidator_ValidateKind(t *testing.T) {
	v := NewAPIValidator()

	// Test RestApi kind - valid
	t.Run("Valid RestApi kind", func(t *testing.T) {
		config := createValidRestAPIConfig()
		errors := v.Validate(config)
		hasKindError := false
		for _, e := range errors {
			if e.Field == "kind" {
				hasKindError = true
				break
			}
		}
		if hasKindError {
			t.Error("unexpected kind error")
		}
	})

	// Test WebSubApi kind - valid
	t.Run("Valid WebSubApi kind", func(t *testing.T) {
		config := createValidWebSubAPIConfig()
		errors := v.Validate(config)
		hasKindError := false
		for _, e := range errors {
			if e.Field == "kind" {
				hasKindError = true
				break
			}
		}
		if hasKindError {
			t.Error("unexpected kind error")
		}
	})

	// Test unsupported type
	t.Run("Unsupported type", func(t *testing.T) {
		errors := v.Validate("InvalidKind")
		if len(errors) == 0 {
			t.Error("expected error for unsupported type, got none")
		}
	})

	// Test invalid Kind on RestAPI
	t.Run("Invalid RestApi kind", func(t *testing.T) {
		config := createValidRestAPIConfig()
		config.Kind = "InvalidKind"
		errors := v.Validate(config)
		hasKindError := false
		for _, e := range errors {
			if e.Field == "kind" {
				hasKindError = true
				break
			}
		}
		if !hasKindError {
			t.Error("expected kind error for invalid RestAPI kind, got none")
		}
	})

	// Test invalid Kind on WebSubAPI
	t.Run("Invalid WebSubApi kind", func(t *testing.T) {
		config := createValidWebSubAPIConfig()
		config.Kind = "InvalidKind"
		errors := v.Validate(config)
		hasKindError := false
		for _, e := range errors {
			if e.Field == "kind" {
				hasKindError = true
				break
			}
		}
		if !hasKindError {
			t.Error("expected kind error for invalid WebSubAPI kind, got none")
		}
	})
}

func TestAPIValidator_ValidateDisplayName(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name        string
		displayName string
		wantError   bool
		errContains string
	}{
		{name: "Valid display name", displayName: "My API", wantError: false},
		{name: "Empty display name", displayName: "", wantError: true, errContains: "required"},
		{name: "Display name too long", displayName: strings.Repeat("a", 101), wantError: true, errContains: "1-100 characters"},
		{name: "Invalid characters", displayName: "Test@#$%", wantError: true, errContains: "URL-friendly"},
		{name: "Valid with special chars", displayName: "test-api_v1.0", wantError: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidRestAPIConfig()
			config.Spec.DisplayName = tt.displayName

			errors := v.Validate(config)
			hasDisplayNameError := false
			var errorMsg string
			for _, e := range errors {
				if e.Field == "spec.displayName" {
					hasDisplayNameError = true
					errorMsg = e.Message
					break
				}
			}
			if tt.wantError && !hasDisplayNameError {
				t.Error("expected displayName error, got none")
			}
			if !tt.wantError && hasDisplayNameError {
				t.Errorf("unexpected displayName error: %s", errorMsg)
			}
		})
	}
}

func TestAPIValidator_ValidateVersion(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name      string
		version   string
		wantError bool
	}{
		{name: "Valid v1.0", version: "v1.0", wantError: false},
		{name: "Valid v2.1.3", version: "v2.1.3", wantError: false},
		{name: "Valid 1.0", version: "1.0", wantError: false},
		{name: "Valid v1", version: "v1", wantError: false},
		{name: "Empty version", version: "", wantError: true},
		{name: "Invalid version", version: "invalid", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidRestAPIConfig()
			config.Spec.Version = tt.version

			errors := v.Validate(config)
			hasVersionError := false
			for _, e := range errors {
				if e.Field == "spec.version" {
					hasVersionError = true
					break
				}
			}
			if tt.wantError && !hasVersionError {
				t.Error("expected version error, got none")
			}
			if !tt.wantError && hasVersionError {
				t.Error("unexpected version error")
			}
		})
	}
}

func TestAPIValidator_ValidateContext(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name      string
		context   string
		wantError bool
		errMsg    string
	}{
		{name: "Valid context", context: "/api", wantError: false},
		{name: "Empty context", context: "", wantError: true, errMsg: "required"},
		{name: "Context without leading slash", context: "api", wantError: true, errMsg: "start with /"},
		{name: "Context with trailing slash", context: "/api/", wantError: true, errMsg: "cannot end with /"},
		{name: "Root context allowed", context: "/", wantError: false},
		{name: "Context too long", context: "/" + strings.Repeat("a", 201), wantError: true, errMsg: "1-200 characters"},
		// '|' is the internal route-name segment separator (METHOD|PATH|VHOST...) —
		// reject it so vhost grouping cannot be corrupted by user-supplied paths.
		{name: "Context with pipe character", context: "/api|x", wantError: true, errMsg: "must not contain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidRestAPIConfig()
			config.Spec.Context = tt.context

			errors := v.Validate(config)
			hasContextError := false
			for _, e := range errors {
				if e.Field == "spec.context" {
					hasContextError = true
					break
				}
			}
			if tt.wantError && !hasContextError {
				t.Errorf("expected context error, got none. Errors: %v", errors)
			}
			if !tt.wantError && hasContextError {
				t.Error("unexpected context error")
			}
		})
	}
}

func TestAPIValidator_ValidateUpstream(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name      string
		mainURL   *string
		mainRef   *string
		wantError bool
		errField  string
	}{
		{name: "Valid URL", mainURL: stringPtr("http://backend:8080"), mainRef: nil, wantError: false},
		{name: "Valid HTTPS URL", mainURL: stringPtr("https://backend:8443"), mainRef: nil, wantError: false},
		{name: "Both URL and Ref set", mainURL: stringPtr("http://x"), mainRef: stringPtr("ref"), wantError: true, errField: "spec.upstream.main"},
		{name: "Empty URL", mainURL: stringPtr(""), mainRef: nil, wantError: true, errField: "spec.upstream.main.url"},
		{name: "Invalid URL scheme", mainURL: stringPtr("ftp://x"), mainRef: nil, wantError: true, errField: "spec.upstream.main.url"},
		{name: "URL without host", mainURL: stringPtr("http:///path"), mainRef: nil, wantError: true, errField: "spec.upstream.main.url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidRestAPIConfig()
			config.Spec.Upstream.Main.Url = tt.mainURL
			config.Spec.Upstream.Main.Ref = tt.mainRef

			errors := v.Validate(config)
			hasExpectedError := false
			for _, e := range errors {
				if strings.HasPrefix(e.Field, tt.errField) {
					hasExpectedError = true
					break
				}
			}
			if tt.wantError && !hasExpectedError {
				t.Errorf("expected error for field %s, got: %v", tt.errField, errors)
			}
		})
	}
}

func TestAPIValidator_ValidateSandboxUpstream(t *testing.T) {
	v := NewAPIValidator()

	config := createValidRestAPIConfig()
	config.Spec.Upstream.Sandbox = &api.Upstream{
		Url: stringPtr("http://sandbox:8080"),
	}

	errors := v.Validate(config)
	for _, e := range errors {
		if strings.Contains(e.Field, "sandbox") {
			t.Errorf("unexpected sandbox error: %v", e)
		}
	}
}

func TestAPIValidator_ValidateOperations(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name       string
		operations []api.Operation
		wantError  bool
		errField   string
	}{
		{
			name: "Valid operations",
			operations: []api.Operation{
				{Method: api.OperationMethodGET, Path: "/items"},
				{Method: api.OperationMethodPOST, Path: "/items"},
			},
			wantError: false,
		},
		{
			name:       "Empty operations",
			operations: []api.Operation{},
			wantError:  true,
			errField:   "spec.operations",
		},
		{
			name: "Missing method",
			operations: []api.Operation{
				{Method: "", Path: "/items"},
			},
			wantError: true,
			errField:  "spec.operations[0].method",
		},
		{
			name: "Invalid method",
			operations: []api.Operation{
				{Method: "INVALID", Path: "/items"},
			},
			wantError: true,
			errField:  "spec.operations[0].method",
		},
		{
			// '|' is the internal route-name segment separator — reject it so
			// vhost grouping cannot be corrupted by user-supplied paths.
			name: "Path with pipe character",
			operations: []api.Operation{
				{Method: api.OperationMethodGET, Path: "/items|x"},
			},
			wantError: true,
			errField:  "spec.operations[0].path",
		},
		{
			name: "Missing path",
			operations: []api.Operation{
				{Method: api.OperationMethodGET, Path: ""},
			},
			wantError: true,
			errField:  "spec.operations[0].path",
		},
		{
			name: "Path without leading slash",
			operations: []api.Operation{
				{Method: api.OperationMethodGET, Path: "items"},
			},
			wantError: true,
			errField:  "spec.operations[0].path",
		},
		{
			name: "Valid path with parameters",
			operations: []api.Operation{
				{Method: api.OperationMethodGET, Path: "/items/{id}"},
			},
			wantError: false,
		},
		{
			name: "Path with unbalanced braces",
			operations: []api.Operation{
				{Method: api.OperationMethodGET, Path: "/items/{id"},
			},
			wantError: true,
			errField:  "spec.operations[0].path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidRestAPIConfig()
			config.Spec.Operations = tt.operations

			errors := v.Validate(config)
			hasExpectedError := false
			for _, e := range errors {
				if strings.HasPrefix(e.Field, tt.errField) {
					hasExpectedError = true
					break
				}
			}
			if tt.wantError && !hasExpectedError {
				t.Errorf("expected error for field %s, got: %v", tt.errField, errors)
			}
			if !tt.wantError && len(errors) > 0 {
				for _, e := range errors {
					if strings.Contains(e.Field, "operations") {
						t.Errorf("unexpected operations error: %v", e)
					}
				}
			}
		})
	}
}

func TestAPIValidator_ValidateAllHTTPMethods(t *testing.T) {
	v := NewAPIValidator()

	methods := []api.OperationMethod{
		api.OperationMethodGET,
		api.OperationMethodPOST,
		api.OperationMethodPUT,
		api.OperationMethodDELETE,
		api.OperationMethodPATCH,
		api.OperationMethodHEAD,
		api.OperationMethodOPTIONS,
	}

	for _, method := range methods {
		t.Run(string(method), func(t *testing.T) {
			config := createValidRestAPIConfig()
			config.Spec.Operations = []api.Operation{
				{Method: method, Path: "/test"},
			}

			errors := v.Validate(config)
			for _, e := range errors {
				if strings.Contains(e.Field, "method") {
					t.Errorf("unexpected method error for %s: %v", method, e)
				}
			}
		})
	}
}

func TestAPIValidator_ValidateWebSubAPI(t *testing.T) {
	v := NewAPIValidator()

	config := createValidWebSubAPIConfig()

	errors := v.Validate(config)
	if len(errors) != 0 {
		t.Errorf("expected no errors for valid WebSubApi, got: %v", errors)
	}
}

func TestAPIValidator_ValidateChannels(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name      string
		channels  map[string]api.WebSubChannel
		wantError bool
		errField  string
	}{
		{
			name: "Valid channels",
			channels: map[string]api.WebSubChannel{
				"channel1": {},
				"channel2": {},
			},
			wantError: false,
		},
		{
			name:      "Empty channels",
			channels:  map[string]api.WebSubChannel{},
			wantError: true,
			errField:  "spec.channels",
		},
		{
			name: "Channel with braces (invalid)",
			channels: map[string]api.WebSubChannel{
				"channel/{id}": {},
			},
			wantError: true,
			errField:  "spec.channels.channel/{id}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidWebSubAPIConfig()
			config.Spec.Channels = &tt.channels

			errors := v.Validate(config)
			hasExpectedError := false
			for _, e := range errors {
				if strings.HasPrefix(e.Field, tt.errField) {
					hasExpectedError = true
					break
				}
			}
			if tt.wantError && !hasExpectedError {
				t.Errorf("expected error for field %s, got: %v", tt.errField, errors)
			}
		})
	}
}

func TestAPIValidator_ValidateAsyncDisplayName(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		name        string
		displayName string
		wantError   bool
	}{
		{name: "Valid name", displayName: "MyWebSub", wantError: false},
		{name: "Empty name", displayName: "", wantError: true},
		{name: "Name too long", displayName: strings.Repeat("a", 101), wantError: true},
		{name: "Invalid characters", displayName: "test@#$", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := createValidWebSubAPIConfig()
			config.Spec.DisplayName = tt.displayName

			errors := v.Validate(config)
			hasNameError := false
			for _, e := range errors {
				if e.Field == "spec.name" {
					hasNameError = true
					break
				}
			}
			if tt.wantError && !hasNameError {
				t.Error("expected name error, got none")
			}
			if !tt.wantError && hasNameError {
				t.Error("unexpected name error")
			}
		})
	}
}

func TestAPIValidator_ValidatePathParameters(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		path     string
		expected bool
	}{
		{"/items/{id}", true},
		{"/items/{id}/sub/{subId}", true},
		{"/items", true},
		{"/items/{id", false},
		{"/items/id}", false},
		{"/items/{id}/{", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := v.validatePathParameters(tt.path)
			if result != tt.expected {
				t.Errorf("validatePathParameters(%s) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestAPIValidator_ValidatePathParametersForAsyncAPIs(t *testing.T) {
	v := NewAPIValidator()

	tests := []struct {
		path     string
		expected bool
	}{
		{"channel1", true},
		{"my-channel", true},
		{"channel/{id}", false},
		{"{channel}", false},
		{"channel}", false},
		{"channel{", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := v.validatePathParametersForAsyncAPIs(tt.path)
			if result != tt.expected {
				t.Errorf("validatePathParametersForAsyncAPIs(%s) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

// Helper functions

func createValidRestAPIConfig() *api.RestAPI {
	return &api.RestAPI{
		ApiVersion: api.RestAPIApiVersionGatewayApiPlatformWso2Comv1alpha1,
		Kind:       api.RestAPIKindRestApi,
		Metadata: api.Metadata{
			Name: "test-api",
		},
		Spec: api.APIConfigData{
			DisplayName: "Test API",
			Version:     "v1.0",
			Context:     "/test",
			Upstream: struct {
				Main    api.Upstream  `json:"main" yaml:"main"`
				Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
			}{
				Main: api.Upstream{
					Url: stringPtr("http://backend:8080"),
				},
			},
			Operations: []api.Operation{
				{Method: api.OperationMethodGET, Path: "/items"},
			},
		},
	}
}

func createValidWebSubAPIConfig() *api.WebSubAPI {
	return &api.WebSubAPI{
		ApiVersion: api.WebSubAPIApiVersionGatewayApiPlatformWso2Comv1alpha1,
		Kind:       api.WebSubAPIKindWebSubApi,
		Metadata: api.Metadata{
			Name: "test-websub",
		},
		Spec: api.WebhookAPIData{
			DisplayName: "Test WebSub",
			Version:     "v1.0",
			Context:     "/websub",
			Channels: &map[string]api.WebSubChannel{
				"channel1": {},
			},
		},
	}
}

func intPtr(i int) *int {
	return &i
}

func validErrorResponses() *api.ErrorResponses {
	return &api.ErrorResponses{
		Responses: map[string]api.ErrorResponseObject{
			"401": {
				Description: stringPtr("Custom auth message"),
				Content: map[string]api.ErrorResponseMediaType{
					"application/json": {
						Example: map[string]interface{}{"error": "Provide a valid API key", "requestId": "{{requestId}}"},
					},
				},
			},
			"504": {
				XStatusCodeOverride: intPtr(502),
				Content: map[string]api.ErrorResponseMediaType{
					"application/json": {
						Example: map[string]interface{}{"error": "Upstream slow, try later"},
					},
				},
			},
			"default": {
				Content: map[string]api.ErrorResponseMediaType{
					"application/json": {
						Example: map[string]interface{}{"code": "{{statusCode}}", "message": "{{message}}"},
					},
				},
			},
		},
	}
}

func TestValidateErrorResponses_ValidRest(t *testing.T) {
	v := NewAPIValidator()
	config := createValidRestAPIConfig()
	config.Spec.ErrorResponses = validErrorResponses()
	errors := v.Validate(config)
	if len(errors) != 0 {
		t.Errorf("expected no validation errors, got %v", errors)
	}
}

func TestValidateErrorResponses_ValidSoap(t *testing.T) {
	v := NewAPIValidator()
	config := &api.SoapAPI{
		ApiVersion: api.SoapAPIApiVersionGatewayApiPlatformWso2Comv1alpha1,
		Kind:       api.SoapAPIKindSoapApi,
		Metadata:   api.Metadata{Name: "test-soap"},
		Spec: api.SoapAPIData{
			DisplayName: "Test SOAP",
			Version:     "v1.0",
			Context:     "/soap",
		},
	}
	config.Spec.Upstream.Main = api.Upstream{Url: stringPtr("http://backend:8080/services/Test")}
	config.Spec.ErrorResponses = validErrorResponses()
	errors := v.Validate(config)
	if len(errors) != 0 {
		t.Errorf("expected no validation errors, got %v", errors)
	}
}

func TestValidateErrorResponses_NilIsValid(t *testing.T) {
	v := NewAPIValidator()
	if errors := v.validateErrorResponses(nil); len(errors) != 0 {
		t.Errorf("nil errorResponses should be valid, got %v", errors)
	}
}

func TestValidateErrorResponses_Invalid(t *testing.T) {
	v := NewAPIValidator()

	jsonBody := func(body interface{}) map[string]api.ErrorResponseMediaType {
		return map[string]api.ErrorResponseMediaType{
			"application/json": {Example: body},
		}
	}

	tests := []struct {
		name        string
		er          *api.ErrorResponses
		wantMessage string
	}{
		{
			name:        "empty responses",
			er:          &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{}},
			wantMessage: "at least one response entry",
		},
		{
			name: "invalid status key",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"6xx": {Content: jsonBody(map[string]interface{}{"m": "x"})},
			}},
			wantMessage: "invalid response key",
		},
		{
			name: "status key out of range",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"600": {Content: jsonBody(map[string]interface{}{"m": "x"})},
			}},
			wantMessage: "invalid response key",
		},
		{
			name: "override out of range",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"504": {
					XStatusCodeOverride: intPtr(99),
					Content:             jsonBody(map[string]interface{}{"m": "x"}),
				},
			}},
			wantMessage: "invalid x-status-code-override",
		},
		{
			name: "missing content",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"401": {},
			}},
			wantMessage: "at least one media type",
		},
		{
			name: "unknown media type",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"401": {Content: map[string]api.ErrorResponseMediaType{
					"application/octet-stream": {Example: "x"},
				}},
			}},
			wantMessage: "unsupported media type",
		},
		{
			name: "schema only",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"401": {Content: map[string]api.ErrorResponseMediaType{
					"application/json": {Schema: map[string]interface{}{"type": "object"}},
				}},
			}},
			wantMessage: "must define 'example' or 'examples'",
		},
		{
			name: "unknown placeholder",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"401": {Content: jsonBody(map[string]interface{}{"m": "{{bogus}}"})},
			}},
			wantMessage: "unknown placeholder",
		},
		{
			name: "oversized example",
			er: &api.ErrorResponses{Responses: map[string]api.ErrorResponseObject{
				"401": {Content: jsonBody(strings.Repeat("a", 17*1024))},
			}},
			wantMessage: "exceeds maximum size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errors := v.validateErrorResponses(tt.er)
			if len(errors) == 0 {
				t.Fatalf("expected a validation error containing %q, got none", tt.wantMessage)
			}
			found := false
			for _, e := range errors {
				if strings.Contains(e.Message, tt.wantMessage) {
					found = true
				}
			}
			if !found {
				t.Errorf("expected an error containing %q, got %v", tt.wantMessage, errors)
			}
		})
	}
}

// TestErrorResponses_YAMLRoundTrip verifies an API definition carrying
// errorResponses survives YAML deserialization + reserialization intact —
// the shape deploy/store paths rely on.
func TestErrorResponses_YAMLRoundTrip(t *testing.T) {
	doc := `
apiVersion: gateway.api-platform.wso2.com/v1alpha1
kind: RestApi
metadata:
  name: test-api-v1.0
spec:
  displayName: Test API
  version: v1.0
  context: /test/$version
  upstream:
    main:
      url: http://backend:8080
  operations:
    - method: GET
      path: /items
  errorResponses:
    responses:
      "401":
        description: Custom auth message
        content:
          application/json:
            example: { error: "Provide a valid API key" }
      "504":
        x-status-code-override: 502
        content:
          application/json:
            example: { error: "Upstream slow, try later" }
`
	var restAPI api.RestAPI
	if err := yaml.Unmarshal([]byte(doc), &restAPI); err != nil {
		t.Fatalf("failed to unmarshal API definition: %v", err)
	}

	er := restAPI.Spec.ErrorResponses
	if er == nil {
		t.Fatal("errorResponses was not deserialized")
	}
	if len(er.Responses) != 2 {
		t.Fatalf("expected 2 response entries, got %d", len(er.Responses))
	}
	if override := er.Responses["504"].XStatusCodeOverride; override == nil || *override != 502 {
		t.Errorf("x-status-code-override not preserved, got %v", override)
	}
	if er.Responses["401"].Content["application/json"].Example == nil {
		t.Error("example not preserved for 401 application/json")
	}

	v := NewAPIValidator()
	if errors := v.Validate(&restAPI); len(errors) != 0 {
		t.Errorf("round-tripped config should validate cleanly, got %v", errors)
	}

	reserialized, err := yaml.Marshal(&restAPI)
	if err != nil {
		t.Fatalf("failed to re-marshal: %v", err)
	}
	var again api.RestAPI
	if err := yaml.Unmarshal(reserialized, &again); err != nil {
		t.Fatalf("failed to unmarshal re-marshaled config: %v", err)
	}
	if again.Spec.ErrorResponses == nil || len(again.Spec.ErrorResponses.Responses) != 2 {
		t.Error("errorResponses lost in re-serialization round trip")
	}
}
