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
	"strconv"
	"strings"
)

// PlaceholderValues carries the request-scoped values substituted into
// example bodies (${statusCode}, ${message}, ... — design §4.4). Values are
// escaped for the target media type at substitution time, so callers pass
// them raw.
type PlaceholderValues struct {
	StatusCode int
	Message    string
	ErrorCode  string
	Category   Category
	RequestID  string
	APIName    string
	APIVersion string
}

// mediaFamily classifies a media type by how its body is serialized/escaped.
type mediaFamily int

const (
	familyJSON mediaFamily = iota
	familyXML
	familyText
)

func familyOf(mediaType string) mediaFamily {
	mt := strings.ToLower(mediaType)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		return familyJSON
	case mt == "application/xml" || mt == "text/xml" || strings.HasSuffix(mt, "+xml"):
		return familyXML
	default:
		return familyText
	}
}

// renderExample renders a configured example into the response body for the
// given media type, substituting whitelisted placeholders in string values.
//
// A string example is treated as the literal body (placeholder values are
// escaped for the media type). A structured example (map/slice) has
// placeholders substituted in its string leaves and is then serialized —
// JSON marshalling escapes the substituted values, so no manual escaping is
// needed there.
func renderExample(example any, mediaType string, vals PlaceholderValues) ([]byte, error) {
	family := familyOf(mediaType)

	if s, ok := example.(string); ok {
		var escape func(string) string
		switch family {
		case familyJSON:
			escape = escapeJSONString
		case familyXML:
			escape = escapeXMLString
		default:
			escape = func(v string) string { return v }
		}
		return []byte(substitutePlaceholders(s, vals, escape)), nil
	}

	substituted := substituteAny(example, vals)
	body, err := json.Marshal(substituted)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize example: %w", err)
	}
	return body, nil
}

// substituteAny walks a structured example and substitutes placeholders in
// every string leaf. Values are inserted raw — the subsequent serialization
// escapes them.
func substituteAny(value any, vals PlaceholderValues) any {
	switch v := value.(type) {
	case string:
		return substitutePlaceholders(v, vals, func(s string) string { return s })
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, nested := range v {
			out[key] = substituteAny(nested, vals)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, nested := range v {
			out[i] = substituteAny(nested, vals)
		}
		return out
	default:
		return value
	}
}

// substitutePlaceholders replaces whitelisted ${placeholder} tokens in s.
// Unknown placeholders are left untouched (they are rejected at parse time
// anyway). The escape function is applied to the substituted values only,
// never to the surrounding template text.
func substitutePlaceholders(s string, vals PlaceholderValues, escape func(string) string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return placeholderPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-1])
		var value string
		switch name {
		case "statusCode":
			value = strconv.Itoa(vals.StatusCode)
		case "message":
			value = vals.Message
		case "errorCode":
			value = vals.ErrorCode
		case "category":
			value = string(vals.Category)
		case "requestId":
			value = vals.RequestID
		case "apiName":
			value = vals.APIName
		case "apiVersion":
			value = vals.APIVersion
		default:
			return match
		}
		return escape(value)
	})
}

// escapeJSONString escapes a value for inclusion inside a JSON string literal.
func escapeJSONString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(encoded[1 : len(encoded)-1]) // strip surrounding quotes
}

// escapeXMLString escapes a value for inclusion as XML character data.
func escapeXMLString(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
