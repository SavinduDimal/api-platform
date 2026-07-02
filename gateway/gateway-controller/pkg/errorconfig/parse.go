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
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ErrorResponses is the in-memory form of an error-response configuration
// document: the subset of the OpenAPI 3.x Responses Object the gateway
// understands. This mirrors the policy engine's errorformat model (separate
// Go modules) — keep the two in sync.
type ErrorResponses struct {
	// Responses is keyed by a 3-digit HTTP status code ("401") or "default".
	Responses map[string]ResponseObject
}

// ResponseObject is the subset of the OpenAPI Response Object used for error
// customization.
type ResponseObject struct {
	Description    string
	StatusOverride *int
	Content        map[string]MediaType
}

// MediaType is the subset of the OpenAPI Media Type Object used for error
// customization. The gateway renders Example (or a named entry from
// Examples); Schema is descriptive/validation-only.
type MediaType struct {
	Schema   any
	Example  any
	Examples map[string]any
}

// rawDocument mirrors the on-disk YAML shape: an OpenAPI-style document with
// a top-level `responses` map. A `components` section is tolerated but
// otherwise ignored.
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
// (YAML or JSON) into the in-memory model. The validation rules match the
// policy engine's errorformat.Parse.
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
