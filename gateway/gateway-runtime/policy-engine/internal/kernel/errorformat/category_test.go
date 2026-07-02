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

package errorformat

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
		// Unmapped codes fall back by class
		{502, InternalError},
		{599, InternalError},
		{405, RequestMalformed},
		{422, RequestMalformed},
	}
	for _, tt := range tests {
		if got := CategoryForStatus(tt.status); got != tt.expected {
			t.Errorf("CategoryForStatus(%d) = %s, want %s", tt.status, got, tt.expected)
		}
	}
}

func TestDefaultStatusRoundTrip(t *testing.T) {
	// Every category with a default status must classify back to itself.
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
}

func TestBackendErrorHasNoDefaultStatus(t *testing.T) {
	if got := BackendError.DefaultStatus(); got != 0 {
		t.Errorf("BackendError.DefaultStatus() = %d, want 0 (passthrough)", got)
	}
}

func TestAnalyticsFaultCategory(t *testing.T) {
	tests := []struct {
		category Category
		expected string
	}{
		{BackendUnavailable, "TARGET_CONNECTIVITY"},
		{BackendTimeout, "TARGET_CONNECTIVITY"},
		{AuthenticationFailure, "OTHER"},
		{AuthorizationFailure, "OTHER"},
		{Throttled, "OTHER"},
		{RequestMalformed, "OTHER"},
		{RouteNotFound, "OTHER"},
		{InternalError, "OTHER"},
		{PayloadTooLarge, "OTHER"},
		{BackendError, "OTHER"},
	}
	for _, tt := range tests {
		if got := tt.category.AnalyticsFaultCategory(); got != tt.expected {
			t.Errorf("%s.AnalyticsFaultCategory() = %s, want %s", tt.category, got, tt.expected)
		}
	}
}

// TestNoAnalyticsImport guards the design-review requirement that error
// customization is independent of analytics: this package must never import
// internal/analytics (design §4.1.1).
func TestNoAnalyticsImport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("failed to read package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", entry.Name(), err)
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "internal/analytics") {
				t.Errorf("%s imports %s: errorformat must not depend on the analytics package", entry.Name(), imp.Path.Value)
			}
		}
	}
}
