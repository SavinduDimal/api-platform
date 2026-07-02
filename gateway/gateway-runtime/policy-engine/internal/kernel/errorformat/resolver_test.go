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
	"encoding/json"
	"strings"
	"testing"
)

func mustParse(t *testing.T, doc string) *ErrorResponses {
	t.Helper()
	parsed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	return parsed
}

func globalDoc(t *testing.T) *ErrorResponses {
	return mustParse(t, `
responses:
  "401":
    content:
      application/json:
        example: { code: 401, message: "Global auth required", requestId: "{{requestId}}" }
      application/xml:
        example: "<error><message>Global auth required</message></error>"
  "429":
    content:
      application/json:
        example: { code: 429, message: "Global throttled" }
  "504":
    x-status-code-override: 502
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "{{message}}" }
  default:
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "{{message}}", category: "{{category}}" }
`)
}

func TestResolve_ExactStatusMatch(t *testing.T) {
	r := NewResolver(globalDoc(t), "application/json")
	res, ok := r.Resolve(401, "", nil, PlaceholderValues{RequestID: "req-1"})
	if !ok {
		t.Fatal("expected a match for 401")
	}
	if res.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", res.StatusCode)
	}
	if res.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", res.ContentType)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v (%s)", err, res.Body)
	}
	if body["message"] != "Global auth required" {
		t.Errorf("message = %v", body["message"])
	}
	if body["requestId"] != "req-1" {
		t.Errorf("requestId placeholder not substituted: %v", body["requestId"])
	}
}

func TestResolve_DefaultFallback(t *testing.T) {
	r := NewResolver(globalDoc(t), "application/json")
	res, ok := r.Resolve(403, "", nil, PlaceholderValues{})
	if !ok {
		t.Fatal("expected the default entry to match 403")
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "403" {
		t.Errorf("statusCode placeholder = %v, want 403", body["code"])
	}
	if body["message"] != "Forbidden" {
		t.Errorf("message should default to the HTTP status text, got %v", body["message"])
	}
	if body["category"] != string(AuthorizationFailure) {
		t.Errorf("category placeholder = %v, want %s", body["category"], AuthorizationFailure)
	}
}

func TestResolve_UnmatchedPassthrough(t *testing.T) {
	doc := mustParse(t, `
responses:
  "401":
    content:
      application/json:
        example: { message: "auth" }
`)
	r := NewResolver(doc, "application/json")
	if _, ok := r.Resolve(500, "", nil, PlaceholderValues{}); ok {
		t.Error("500 should not match a document with only a 401 entry")
	}

	rNil := NewResolver(nil, "application/json")
	if _, ok := rNil.Resolve(401, "", nil, PlaceholderValues{}); ok {
		t.Error("nil global without per-API should never match")
	}
}

func TestResolve_StatusOverride(t *testing.T) {
	r := NewResolver(globalDoc(t), "application/json")
	res, ok := r.Resolve(504, "", nil, PlaceholderValues{})
	if !ok {
		t.Fatal("expected a match for 504")
	}
	if res.StatusCode != 502 {
		t.Errorf("StatusCode = %d, want 502 (x-status-code-override)", res.StatusCode)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatal(err)
	}
	// Placeholders reflect the overridden (returned) status.
	if body["code"] != "502" {
		t.Errorf("statusCode placeholder = %v, want 502", body["code"])
	}
	if body["message"] != "Bad Gateway" {
		t.Errorf("message = %v, want Bad Gateway", body["message"])
	}
}

func TestResolve_ContentNegotiation(t *testing.T) {
	r := NewResolver(globalDoc(t), "application/json")
	tests := []struct {
		accept   string
		expected string
	}{
		{"application/xml", "application/xml"},
		{"application/json", "application/json"},
		{"text/html, application/xml;q=0.9", "application/xml"},
		{"application/*", "application/json"}, // sorted-first among application/*
		{"*/*", "application/json"},           // default media type
		{"", "application/json"},              // no Accept → default media type
		{"text/html", "application/json"},     // no match → default media type
		{"garbage;;;", "application/json"},    // unparsable → default media type
	}
	for _, tt := range tests {
		res, ok := r.Resolve(401, tt.accept, nil, PlaceholderValues{})
		if !ok {
			t.Fatalf("Resolve(401, %q) did not match", tt.accept)
		}
		if res.ContentType != tt.expected {
			t.Errorf("Accept %q negotiated %q, want %q", tt.accept, res.ContentType, tt.expected)
		}
	}
}

func TestResolve_DefaultMediaTypeNotInContent(t *testing.T) {
	doc := mustParse(t, `
responses:
  "401":
    content:
      application/xml:
        example: "<error/>"
`)
	r := NewResolver(doc, "application/json")
	res, ok := r.Resolve(401, "", nil, PlaceholderValues{})
	if !ok {
		t.Fatal("expected a match")
	}
	if res.ContentType != "application/xml" {
		t.Errorf("should fall back to the entry's only media type, got %q", res.ContentType)
	}
}

func TestResolve_PerAPIPrecedence(t *testing.T) {
	global := globalDoc(t)
	perAPI := mustParse(t, `
responses:
  "401":
    content:
      application/json:
        example: { message: "Per-API auth required" }
`)
	r := NewResolver(global, "application/json")

	// Per-API 401 wins over global 401.
	res, ok := r.Resolve(401, "", perAPI, PlaceholderValues{})
	if !ok {
		t.Fatal("expected a match")
	}
	if !strings.Contains(string(res.Body), "Per-API auth required") {
		t.Errorf("per-API entry should win, got %s", res.Body)
	}

	// Per-API has no 429 → global 429 applies (per-status resolution).
	res, ok = r.Resolve(429, "", perAPI, PlaceholderValues{})
	if !ok {
		t.Fatal("expected a match")
	}
	if !strings.Contains(string(res.Body), "Global throttled") {
		t.Errorf("global 429 should apply when per-API has no 429, got %s", res.Body)
	}

	// Per-API works even without any global config.
	rNil := NewResolver(nil, "application/json")
	res, ok = rNil.Resolve(401, "", perAPI, PlaceholderValues{})
	if !ok {
		t.Fatal("per-API should match with nil global")
	}
	if !strings.Contains(string(res.Body), "Per-API auth required") {
		t.Errorf("unexpected body %s", res.Body)
	}
}

func TestResolve_PerAPIDefaultShadowsGlobalExact(t *testing.T) {
	global := globalDoc(t)
	perAPI := mustParse(t, `
responses:
  default:
    content:
      application/json:
        example: { message: "Per-API catch-all" }
`)
	r := NewResolver(global, "application/json")
	res, ok := r.Resolve(429, "", perAPI, PlaceholderValues{})
	if !ok {
		t.Fatal("expected a match")
	}
	// The per-API document resolves as a whole before the global one: its
	// default is more specific to this API than the global exact entry.
	if !strings.Contains(string(res.Body), "Per-API catch-all") {
		t.Errorf("per-API default should shadow global exact entry, got %s", res.Body)
	}
}

func TestResolve_PerAPIOverride(t *testing.T) {
	perAPI := mustParse(t, `
responses:
  "504":
    x-status-code-override: 503
    content:
      application/json:
        example: { message: "per-api remap" }
`)
	r := NewResolver(globalDoc(t), "application/json")
	res, ok := r.Resolve(504, "", perAPI, PlaceholderValues{})
	if !ok {
		t.Fatal("expected a match")
	}
	if res.StatusCode != 503 {
		t.Errorf("per-API x-status-code-override should win, got %d", res.StatusCode)
	}
}

func TestResolve_ExamplesPlural(t *testing.T) {
	doc := mustParse(t, `
responses:
  "401":
    content:
      application/json:
        examples:
          verbose: { value: { message: "verbose denied" } }
          default: { value: { message: "default denied" } }
`)
	r := NewResolver(doc, "application/json")
	res, ok := r.Resolve(401, "", nil, PlaceholderValues{})
	if !ok {
		t.Fatal("expected a match")
	}
	if !strings.Contains(string(res.Body), "default denied") {
		t.Errorf("the named 'default' example should be preferred, got %s", res.Body)
	}
}

func TestRenderExample_JSONEscaping(t *testing.T) {
	body, err := renderExample(
		map[string]any{"message": "{{message}}"},
		"application/json",
		PlaceholderValues{Message: `quote " backslash \ <tag>`},
	)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("injection broke JSON validity: %v (%s)", err, body)
	}
	if decoded["message"] != `quote " backslash \ <tag>` {
		t.Errorf("value not preserved: %v", decoded["message"])
	}
}

func TestRenderExample_StringTemplateJSONEscaping(t *testing.T) {
	body, err := renderExample(
		`{"message": "{{message}}"}`,
		"application/json",
		PlaceholderValues{Message: `break " out`},
	)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("string-template injection broke JSON validity: %v (%s)", err, body)
	}
	if decoded["message"] != `break " out` {
		t.Errorf("value not preserved: %v", decoded["message"])
	}
}

func TestRenderExample_XMLEscaping(t *testing.T) {
	body, err := renderExample(
		"<error><message>{{message}}</message></error>",
		"application/xml",
		PlaceholderValues{Message: `<script>&"'`},
	)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if strings.Contains(s, "<script>") {
		t.Errorf("XML value not escaped: %s", s)
	}
	if !strings.Contains(s, "&lt;script&gt;&amp;&quot;&apos;") {
		t.Errorf("unexpected escaping: %s", s)
	}
}

func TestRenderExample_TextPlain(t *testing.T) {
	body, err := renderExample(
		"error {{statusCode}}: {{message}}",
		"text/plain",
		PlaceholderValues{StatusCode: 429, Message: "slow down"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "error 429: slow down" {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestRenderExample_NestedStructure(t *testing.T) {
	body, err := renderExample(
		map[string]any{
			"error": map[string]any{
				"details": []any{"api {{apiName}}", "version {{apiVersion}}"},
			},
		},
		"application/json",
		PlaceholderValues{APIName: "StockQuote", APIVersion: "v1.0"},
	)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "api StockQuote") || !strings.Contains(s, "version v1.0") {
		t.Errorf("nested placeholders not substituted: %s", s)
	}
}
