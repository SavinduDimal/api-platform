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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDoc = `
responses:
  "404":
    content:
      application/json:
        example: { code: 404, message: "No API matched", category: "{{category}}" }
  "503":
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "{{message}}" }
      application/xml:
        example: "<error><message>{{message}}</message></error>"
  "504":
    x-status-code-override: 502
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "Upstream did not respond in time" }
  default:
    content:
      application/json:
        example: { code: "{{statusCode}}", message: "{{message}}", requestId: "{{requestId}}" }
`

func parseTestDoc(t *testing.T) *ErrorResponses {
	t.Helper()
	parsed, err := Parse([]byte(testDoc))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	return parsed
}

func TestParseAndLoadFile(t *testing.T) {
	parsed := parseTestDoc(t)
	if len(parsed.Responses) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(parsed.Responses))
	}
	if o := parsed.Responses["504"].StatusOverride; o == nil || *o != 502 {
		t.Errorf("x-status-code-override not parsed: %v", o)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "error-responses.yaml")
	if err := os.WriteFile(path, []byte(testDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err != nil {
		t.Errorf("LoadFile() failed: %v", err)
	}
	if _, err := LoadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("LoadFile() should fail on a missing file")
	}

	if _, err := Parse([]byte(`responses: { "6xx": { content: { application/json: { example: "x" } } } }`)); err == nil {
		t.Error("Parse() should reject an invalid status key")
	}
}

func TestResolveStatic_ExactMatch(t *testing.T) {
	er := parseTestDoc(t)
	res, ok := er.ResolveStatic(503, "application/json")
	if !ok {
		t.Fatal("expected a match for 503")
	}
	if res.StatusCode != 503 || res.ContentType != "application/json" {
		t.Errorf("unexpected resolution: %+v", res)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("body not valid JSON: %v (%s)", err, res.Body)
	}
	if body["code"] != "503" || body["message"] != "Service Unavailable" {
		t.Errorf("static placeholders not substituted: %v", body)
	}
}

func TestResolveStatic_Override(t *testing.T) {
	er := parseTestDoc(t)
	res, ok := er.ResolveStatic(504, "application/json")
	if !ok {
		t.Fatal("expected a match for 504")
	}
	if res.StatusCode != 502 {
		t.Errorf("StatusCode = %d, want 502", res.StatusCode)
	}
	if !strings.Contains(res.Body, `"code":"502"`) {
		t.Errorf("statusCode placeholder should reflect the override: %s", res.Body)
	}
}

func TestResolveStatic_DefaultFallbackAndCategory(t *testing.T) {
	er := parseTestDoc(t)
	// 404 exact entry carries the category placeholder.
	res, ok := er.ResolveStatic(404, "application/json")
	if !ok {
		t.Fatal("expected a match for 404")
	}
	if !strings.Contains(res.Body, string(RouteNotFound)) {
		t.Errorf("category placeholder not substituted: %s", res.Body)
	}

	// 413 has no exact entry → document default; request-scoped
	// placeholders render empty in the static context.
	res, ok = er.ResolveStatic(413, "application/json")
	if !ok {
		t.Fatal("expected the default entry to match 413")
	}
	if !strings.Contains(res.Body, `"requestId":""`) {
		t.Errorf("request-scoped placeholder should render empty: %s", res.Body)
	}
	if !strings.Contains(res.Body, "Request Entity Too Large") {
		t.Errorf("message should be the HTTP status text: %s", res.Body)
	}
}

func TestResolveStatic_MediaTypeSelection(t *testing.T) {
	er := parseTestDoc(t)
	// Default media type not in the entry → sorted-first content key.
	res, ok := er.ResolveStatic(503, "text/plain")
	if !ok {
		t.Fatal("expected a match")
	}
	if res.ContentType != "application/json" {
		t.Errorf("expected sorted-first media type, got %s", res.ContentType)
	}

	// XML entry renders the string template with XML escaping rules.
	resXML, ok := er.ResolveStatic(503, "application/xml")
	if !ok {
		t.Fatal("expected a match")
	}
	if resXML.Body != "<error><message>Service Unavailable</message></error>" {
		t.Errorf("unexpected XML body: %s", resXML.Body)
	}
}

func TestResolveStatic_NoMatch(t *testing.T) {
	doc, err := Parse([]byte(`
responses:
  "503":
    content:
      application/json:
        example: { message: "x" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.ResolveStatic(404, "application/json"); ok {
		t.Error("404 should not match a document with only a 503 entry")
	}

	var nilDoc *ErrorResponses
	if _, ok := nilDoc.ResolveStatic(503, "application/json"); ok {
		t.Error("nil document should never match")
	}
}
