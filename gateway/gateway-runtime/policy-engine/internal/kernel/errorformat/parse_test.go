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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validDocument = `
responses:
  "401":
    description: Authentication failure
    content:
      application/json:
        schema: { $ref: "#/components/schemas/Error" }
        example: { code: 401, message: "Authentication required", requestId: "${requestId}" }
      application/xml:
        example: "<error><code>401</code><message>Authentication required</message></error>"
  "429":
    description: Rate limit exceeded
    content:
      application/json:
        example: { code: 429, message: "Too many requests" }
  "504":
    description: Upstream timeout
    x-status-code-override: 502
    content:
      application/json:
        example: { code: 502, message: "Upstream did not respond in time" }
  default:
    description: Fallback
    content:
      application/json:
        example: { code: "${statusCode}", message: "${message}", requestId: "${requestId}" }
components:
  schemas:
    Error:
      type: object
      properties:
        code:    { type: integer }
        message: { type: string }
`

func TestParseValidDocument(t *testing.T) {
	parsed, err := Parse([]byte(validDocument))
	if err != nil {
		t.Fatalf("Parse() returned error: %v", err)
	}
	if len(parsed.Responses) != 4 {
		t.Fatalf("expected 4 response entries, got %d", len(parsed.Responses))
	}

	auth, ok := parsed.Responses["401"]
	if !ok {
		t.Fatal("expected a '401' entry")
	}
	if auth.Description != "Authentication failure" {
		t.Errorf("unexpected description: %q", auth.Description)
	}
	if auth.StatusOverride != nil {
		t.Errorf("401 entry should have no status override")
	}
	if len(auth.Content) != 2 {
		t.Errorf("expected 2 media types for 401, got %d", len(auth.Content))
	}
	if auth.Content["application/json"].Example == nil {
		t.Error("expected an example for 401 application/json")
	}
	if auth.Content["application/json"].Schema == nil {
		t.Error("schema should be retained (descriptive)")
	}

	timeout := parsed.Responses["504"]
	if timeout.StatusOverride == nil || *timeout.StatusOverride != 502 {
		t.Errorf("expected 504 entry to carry x-status-code-override 502, got %v", timeout.StatusOverride)
	}

	if _, ok := parsed.Responses["default"]; !ok {
		t.Error("expected a 'default' entry")
	}
}

func TestParseInvalidDocuments(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name:    "not yaml",
			doc:     "{{{{",
			wantErr: "failed to parse YAML",
		},
		{
			name:    "empty responses",
			doc:     "responses: {}",
			wantErr: "at least one entry",
		},
		{
			name:    "no responses key",
			doc:     "components: {}",
			wantErr: "at least one entry",
		},
		{
			name: "bad status key",
			doc: `
responses:
  "6xx":
    content:
      application/json:
        example: { message: "x" }
`,
			wantErr: "invalid response key",
		},
		{
			name: "status key out of range",
			doc: `
responses:
  "600":
    content:
      application/json:
        example: { message: "x" }
`,
			wantErr: "invalid response key",
		},
		{
			name: "override out of range",
			doc: `
responses:
  "504":
    x-status-code-override: 99
    content:
      application/json:
        example: { message: "x" }
`,
			wantErr: "invalid x-status-code-override",
		},
		{
			name: "missing content",
			doc: `
responses:
  "401":
    description: no content
`,
			wantErr: "at least one media type",
		},
		{
			name: "unknown media type",
			doc: `
responses:
  "401":
    content:
      application/octet-stream:
        example: "x"
`,
			wantErr: "unsupported media type",
		},
		{
			name: "schema only, no example",
			doc: `
responses:
  "401":
    content:
      application/json:
        schema: { type: object }
`,
			wantErr: "must define 'example' or 'examples'",
		},
		{
			name: "unknown placeholder",
			doc: `
responses:
  "401":
    content:
      application/json:
        example: { message: "${bogusPlaceholder}" }
`,
			wantErr: "unknown placeholder",
		},
		{
			name: "nested unknown placeholder",
			doc: `
responses:
  "401":
    content:
      application/json:
        example:
          error:
            details: ["ok", "${alsoBogus}"]
`,
			wantErr: "unknown placeholder",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil {
				t.Fatalf("Parse() succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Parse() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseExampleSizeCap(t *testing.T) {
	doc := `
responses:
  "500":
    content:
      text/plain:
        example: "` + strings.Repeat("a", MaxExampleBytes+1) + `"
`
	_, err := Parse([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("expected size-cap error, got %v", err)
	}
}

func TestParseExamplesPlural(t *testing.T) {
	doc := `
responses:
  "401":
    content:
      application/json:
        examples:
          short: { value: { message: "denied" } }
          verbose: { value: { message: "denied", requestId: "${requestId}" } }
`
	parsed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() returned error: %v", err)
	}
	mt := parsed.Responses["401"].Content["application/json"]
	if len(mt.Examples) != 2 {
		t.Errorf("expected 2 named examples, got %d", len(mt.Examples))
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error-responses.yaml")
	if err := os.WriteFile(path, []byte(validDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() returned error: %v", err)
	}
	if len(parsed.Responses) != 4 {
		t.Errorf("expected 4 response entries, got %d", len(parsed.Responses))
	}

	if _, err := LoadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("LoadFile() on a missing file should return an error")
	}

	badPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPath, []byte("responses: {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(badPath); err == nil || !strings.Contains(err.Error(), badPath) {
		t.Errorf("LoadFile() on an invalid file should mention the path, got %v", err)
	}
}
