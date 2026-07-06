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

package errorconfig

import (
	"strings"
	"testing"
)

func TestCategoryForStatus(t *testing.T) {
	tests := []struct {
		status   int
		expected Category
	}{
		{401, AuthenticationFailure},
		{403, AuthorizationFailure},
		{429, Throttled},
		{400, RequestMalformed},
		{404, RouteNotFound},
		{413, PayloadTooLarge},
		{503, BackendUnavailable},
		{504, BackendTimeout},
		{500, InternalError},
		{502, InternalError},
		{405, RequestMalformed},
	}
	for _, tt := range tests {
		if got := CategoryForStatus(tt.status); got != tt.expected {
			t.Errorf("CategoryForStatus(%d) = %s, want %s", tt.status, got, tt.expected)
		}
	}
}

func TestDefaultStatusRoundTrip(t *testing.T) {
	categories := []Category{
		AuthenticationFailure,
		AuthorizationFailure,
		Throttled,
		RequestMalformed,
		RouteNotFound,
		InternalError,
		BackendUnavailable,
		BackendTimeout,
		PayloadTooLarge,
	}
	for _, c := range categories {
		status := c.DefaultStatus()
		if status == 0 {
			t.Errorf("%s.DefaultStatus() = 0, want a concrete status", c)
			continue
		}
		if got := CategoryForStatus(status); got != c {
			t.Errorf("CategoryForStatus(%s.DefaultStatus()=%d) = %s, want %s", c, status, got, c)
		}
	}
	if got := BackendError.DefaultStatus(); got != 0 {
		t.Errorf("BackendError.DefaultStatus() = %d, want 0 (passthrough)", got)
	}
}

func TestValidateStatusKey(t *testing.T) {
	valid := []string{"100", "401", "429", "599", "default"}
	for _, key := range valid {
		if err := ValidateStatusKey(key); err != nil {
			t.Errorf("ValidateStatusKey(%q) = %v, want nil", key, err)
		}
	}
	invalid := []string{"", "40", "0401", "600", "099", "5xx", "5XX", "abc", "Default"}
	for _, key := range invalid {
		if err := ValidateStatusKey(key); err == nil {
			t.Errorf("ValidateStatusKey(%q) = nil, want error", key)
		}
	}
}

func TestValidateStatusOverride(t *testing.T) {
	for _, code := range []int{100, 502, 599} {
		if err := ValidateStatusOverride(code); err != nil {
			t.Errorf("ValidateStatusOverride(%d) = %v, want nil", code, err)
		}
	}
	for _, code := range []int{0, 99, 600, -1} {
		if err := ValidateStatusOverride(code); err == nil {
			t.Errorf("ValidateStatusOverride(%d) = nil, want error", code)
		}
	}
}

func TestValidateMediaTypeName(t *testing.T) {
	valid := []string{
		"application/json",
		"application/xml",
		"text/xml",
		"text/plain",
		"application/soap+xml",
		"application/problem+json",
		"application/hal+json",
	}
	for _, mt := range valid {
		if err := ValidateMediaTypeName(mt); err != nil {
			t.Errorf("ValidateMediaTypeName(%q) = %v, want nil", mt, err)
		}
	}
	invalid := []string{"", "application/octet-stream", "text/html", "not a media type"}
	for _, mt := range invalid {
		if err := ValidateMediaTypeName(mt); err == nil {
			t.Errorf("ValidateMediaTypeName(%q) = nil, want error", mt)
		}
	}
}

func TestValidateExample(t *testing.T) {
	valid := []any{
		map[string]any{"code": 401, "message": "Authentication required", "requestId": "${requestId}"},
		"plain body with ${statusCode} and ${message}",
		[]any{"a", map[string]any{"b": "${apiName}"}},
	}
	for _, ex := range valid {
		if err := ValidateExample(ex); err != nil {
			t.Errorf("ValidateExample(%v) = %v, want nil", ex, err)
		}
	}

	if err := ValidateExample("${notAPlaceholder}"); err == nil || !strings.Contains(err.Error(), "unknown placeholder") {
		t.Errorf("expected unknown-placeholder error, got %v", err)
	}
	if err := ValidateExample(map[string]any{"nested": []any{"${bad}"}}); err == nil || !strings.Contains(err.Error(), "unknown placeholder") {
		t.Errorf("expected nested unknown-placeholder error, got %v", err)
	}
	if err := ValidateExample(strings.Repeat("a", MaxExampleBytes+1)); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("expected size-cap error, got %v", err)
	}
}
