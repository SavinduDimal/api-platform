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
	"fmt"
	"mime"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxExampleBytes caps the size of a single rendered example body
// (JSON-serialized size at validation time).
const MaxExampleBytes = 16 * 1024

// allowedPlaceholders is the whitelist of ${placeholder} names that may
// appear in example bodies (design §4.4). Anything else is rejected at parse
// time so typos fail fast instead of rendering literally.
//
// The syntax is ${name} — NOT {{name}} — because API artifacts are rendered
// through Go text/template (artifact templating: {{ env "..." }} etc.)
// before YAML parsing; {{name}} in a per-API errorResponses block would be
// rejected there as an unknown template function.
var allowedPlaceholders = map[string]bool{
	"statusCode": true,
	"message":    true,
	"errorCode":  true,
	"category":   true,
	"requestId":  true,
	"apiName":    true,
	"apiVersion": true,
}

var placeholderPattern = regexp.MustCompile(`\$\{\s*([^{}$]+?)\s*\}`)

// rawDocument mirrors the on-disk YAML shape: an OpenAPI-style document with
// a top-level `responses` map. A `components` section is tolerated (schemas
// referenced via $ref are descriptive only) but otherwise ignored.
type rawDocument struct {
	Responses  map[string]rawResponse `yaml:"responses"`
	Components map[string]any         `yaml:"components"`
}

type rawResponse struct {
	Description        string                  `yaml:"description"`
	StatusCodeOverride *int                    `yaml:"x-status-code-override"`
	Content            map[string]rawMediaType `yaml:"content"`
}

type rawMediaType struct {
	Schema   any            `yaml:"schema"`
	Example  any            `yaml:"example"`
	Examples map[string]any `yaml:"examples"`
}

// LoadFile reads and parses an error-response configuration file.
func LoadFile(path string) (*ErrorResponses, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read error-response config %q: %w", path, err)
	}
	parsed, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("invalid error-response config %q: %w", path, err)
	}
	return parsed, nil
}

// Parse parses and validates an OpenAPI-shaped error-response document
// (YAML or JSON) into the in-memory model.
func Parse(data []byte) (*ErrorResponses, error) {
	var doc rawDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if len(doc.Responses) == 0 {
		return nil, fmt.Errorf("document must define at least one entry under 'responses'")
	}

	result := &ErrorResponses{Responses: make(map[string]ResponseObject, len(doc.Responses))}
	for key, raw := range doc.Responses {
		if err := ValidateStatusKey(key); err != nil {
			return nil, err
		}
		obj, err := buildResponseObject(key, raw)
		if err != nil {
			return nil, err
		}
		result.Responses[key] = obj
	}
	return result, nil
}

func buildResponseObject(key string, raw rawResponse) (ResponseObject, error) {
	var zero ResponseObject
	if raw.StatusCodeOverride != nil {
		if err := ValidateStatusOverride(*raw.StatusCodeOverride); err != nil {
			return zero, fmt.Errorf("responses.%s: %w", key, err)
		}
	}
	if len(raw.Content) == 0 {
		return zero, fmt.Errorf("responses.%s: must define at least one media type under 'content'", key)
	}

	content := make(map[string]MediaType, len(raw.Content))
	for mediaType, rawMT := range raw.Content {
		if err := ValidateMediaTypeName(mediaType); err != nil {
			return zero, fmt.Errorf("responses.%s.content: %w", key, err)
		}
		if rawMT.Example == nil && len(rawMT.Examples) == 0 {
			return zero, fmt.Errorf("responses.%s.content.%s: must define 'example' or 'examples' (schema alone is descriptive only)", key, mediaType)
		}
		if rawMT.Example != nil {
			if err := ValidateExample(rawMT.Example); err != nil {
				return zero, fmt.Errorf("responses.%s.content.%s.example: %w", key, mediaType, err)
			}
		}
		for name, ex := range rawMT.Examples {
			if err := ValidateExample(ex); err != nil {
				return zero, fmt.Errorf("responses.%s.content.%s.examples.%s: %w", key, mediaType, name, err)
			}
		}
		content[mediaType] = MediaType{Schema: rawMT.Schema, Example: rawMT.Example, Examples: rawMT.Examples}
	}

	return ResponseObject{
		Description:    raw.Description,
		StatusOverride: raw.StatusCodeOverride,
		Content:        content,
	}, nil
}

// ValidateStatusKey checks a responses-map key: a 3-digit HTTP status code
// in 100–599, or the literal "default".
func ValidateStatusKey(key string) error {
	if key == "default" {
		return nil
	}
	if len(key) != 3 {
		return fmt.Errorf("invalid response key %q: must be a 3-digit status code (100-599) or 'default'", key)
	}
	code, err := strconv.Atoi(key)
	if err != nil || code < 100 || code > 599 {
		return fmt.Errorf("invalid response key %q: must be a 3-digit status code (100-599) or 'default'", key)
	}
	return nil
}

// ValidateStatusOverride checks an x-status-code-override value.
func ValidateStatusOverride(code int) error {
	if code < 100 || code > 599 {
		return fmt.Errorf("invalid x-status-code-override %d: must be in 100-599", code)
	}
	return nil
}

// ValidateMediaTypeName checks that a content key is a well-formed,
// renderable media type: JSON, XML (including +json/+xml suffixes), or
// plain text.
func ValidateMediaTypeName(name string) error {
	mediaType, _, err := mime.ParseMediaType(name)
	if err != nil {
		return fmt.Errorf("invalid media type %q: %w", name, err)
	}
	switch mediaType {
	case "application/json", "application/xml", "text/xml", "text/plain", "application/soap+xml":
		return nil
	}
	if strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") {
		return nil
	}
	return fmt.Errorf("unsupported media type %q: must be JSON, XML, or text/plain", name)
}

// ValidateExample checks an example body: bounded size and only whitelisted
// ${placeholder} names in its string values.
func ValidateExample(example any) error {
	serialized, err := json.Marshal(example)
	if err != nil {
		return fmt.Errorf("example is not serializable: %w", err)
	}
	if len(serialized) > MaxExampleBytes {
		return fmt.Errorf("example exceeds maximum size of %d bytes (got %d)", MaxExampleBytes, len(serialized))
	}
	return validatePlaceholders(example)
}

func validatePlaceholders(value any) error {
	switch v := value.(type) {
	case string:
		for _, match := range placeholderPattern.FindAllStringSubmatch(v, -1) {
			if !allowedPlaceholders[match[1]] {
				return fmt.Errorf("unknown placeholder ${%s}: allowed placeholders are statusCode, message, errorCode, category, requestId, apiName, apiVersion", match[1])
			}
		}
	case map[string]any:
		for _, nested := range v {
			if err := validatePlaceholders(nested); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range v {
			if err := validatePlaceholders(nested); err != nil {
				return err
			}
		}
	}
	return nil
}
