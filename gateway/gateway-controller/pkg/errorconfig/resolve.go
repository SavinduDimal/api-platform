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
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// StaticResolution is the outcome of ResolveStatic: a body rendered at xDS
// build time for Envoy-generated errors (local replies, no-route 404).
type StaticResolution struct {
	StatusCode  int
	ContentType string
	Body        string
}

// ResolveStatic resolves a customized response for an Envoy-generated error
// status at configuration time. Unlike the policy engine's request-time
// resolver, everything here is rendered STATICALLY when the xDS snapshot is
// built:
//
//   - There is no request Accept header — the media type is the configured
//     default, falling back to the entry's first media type (sorted).
//   - Only build-time placeholders are substituted: ${statusCode},
//     ${message} (the HTTP status text) and ${category}. Request-scoped
//     placeholders (${requestId}, ${apiName}, ${apiVersion},
//     ${errorCode}) render as empty strings — Envoy local replies never
//     reach the policy engine, so those values do not exist. Envoy's own
//     %COMMAND% operators are a separate rendering context (see design
//     docs).
//
// Lookup is the entry's exact status key, then the document's "default".
// x-status-code-override replaces the returned status.
func (er *ErrorResponses) ResolveStatic(status int, defaultMediaType string) (StaticResolution, bool) {
	if er == nil {
		return StaticResolution{}, false
	}
	entry, ok := er.Responses[strconv.Itoa(status)]
	if !ok {
		entry, ok = er.Responses["default"]
	}
	if !ok {
		return StaticResolution{}, false
	}

	statusOut := status
	if entry.StatusOverride != nil {
		statusOut = *entry.StatusOverride
	}

	mediaType, ok := pickMediaType(entry.Content, defaultMediaType)
	if !ok {
		return StaticResolution{}, false
	}

	example, ok := selectExample(entry.Content[mediaType])
	if !ok {
		return StaticResolution{}, false
	}

	message := http.StatusText(statusOut)
	body, err := renderStatic(example, mediaType, statusOut, message, CategoryForStatus(status))
	if err != nil {
		return StaticResolution{}, false
	}
	return StaticResolution{StatusCode: statusOut, ContentType: mediaType, Body: body}, true
}

// pickMediaType chooses the media type rendered for a static body: the
// configured default media type when the entry defines it, otherwise the
// first content key in sorted order.
func pickMediaType(content map[string]MediaType, defaultMediaType string) (string, bool) {
	if len(content) == 0 {
		return "", false
	}
	if _, ok := content[defaultMediaType]; ok {
		return defaultMediaType, true
	}
	keys := make([]string, 0, len(content))
	for key := range content {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys[0], true
}

// selectExample picks the body template from a media type object: `example`
// first, then the "default" named entry of `examples`, then the first named
// entry in sorted order. An OpenAPI Example Object ({summary, value}) is
// unwrapped to its `value`.
func selectExample(mt MediaType) (any, bool) {
	if mt.Example != nil {
		return mt.Example, true
	}
	if len(mt.Examples) == 0 {
		return nil, false
	}
	name := "default"
	if _, ok := mt.Examples[name]; !ok {
		names := make([]string, 0, len(mt.Examples))
		for n := range mt.Examples {
			names = append(names, n)
		}
		sort.Strings(names)
		name = names[0]
	}
	example := mt.Examples[name]
	if obj, ok := example.(map[string]any); ok {
		if value, ok := obj["value"]; ok {
			return value, true
		}
	}
	return example, true
}

// renderStatic renders an example with the build-time placeholder values. A
// string example is the literal body (values escaped per media family); a
// structured example has its string leaves substituted and is then
// JSON-serialized (which escapes them).
func renderStatic(example any, mediaType string, statusCode int, message string, category Category) (string, error) {
	vals := map[string]string{
		"statusCode": strconv.Itoa(statusCode),
		"message":    message,
		"category":   string(category),
		"errorCode":  "",
		"requestId":  "",
		"apiName":    "",
		"apiVersion": "",
	}

	if s, ok := example.(string); ok {
		escape := staticEscapeFor(mediaType)
		return substituteStatic(s, vals, escape), nil
	}

	substituted := substituteStaticAny(example, vals)
	body, err := json.Marshal(substituted)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func substituteStaticAny(value any, vals map[string]string) any {
	switch v := value.(type) {
	case string:
		return substituteStatic(v, vals, func(s string) string { return s })
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, nested := range v {
			out[key] = substituteStaticAny(nested, vals)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, nested := range v {
			out[i] = substituteStaticAny(nested, vals)
		}
		return out
	default:
		return value
	}
}

func substituteStatic(s string, vals map[string]string, escape func(string) string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return placeholderPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-1])
		value, ok := vals[name]
		if !ok {
			return match
		}
		return escape(value)
	})
}

func staticEscapeFor(mediaType string) func(string) string {
	mt := strings.ToLower(mediaType)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		return func(s string) string {
			encoded, err := json.Marshal(s)
			if err != nil {
				return s
			}
			return string(encoded[1 : len(encoded)-1])
		}
	case mt == "application/xml" || mt == "text/xml" || strings.HasSuffix(mt, "+xml"):
		return func(s string) string {
			r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
			return r.Replace(s)
		}
	default:
		return func(s string) string { return s }
	}
}
