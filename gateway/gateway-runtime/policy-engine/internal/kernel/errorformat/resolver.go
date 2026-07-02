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
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Resolver resolves an error status code to a customized response using the
// global error-response configuration and an optional per-API override. It
// is built once at engine start and is safe for concurrent use (the
// underlying documents are immutable after parse).
type Resolver struct {
	global           *ErrorResponses
	defaultMediaType string
}

// NewResolver creates a Resolver. global may be nil (no global customization
// — only per-API documents passed to Resolve will match). defaultMediaType
// is used when the request Accept header matches none of an entry's media
// types; empty falls back to "application/json".
func NewResolver(global *ErrorResponses, defaultMediaType string) *Resolver {
	if defaultMediaType == "" {
		defaultMediaType = "application/json"
	}
	return &Resolver{global: global, defaultMediaType: defaultMediaType}
}

// Resolution is the outcome of a successful Resolve: the (possibly
// overridden) status code, the negotiated content type, and the rendered
// body.
type Resolution struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// Resolve looks up a customized response for an error status code.
//
// Entry precedence (per status code): the per-API document resolves first
// (its exact status key, then its own "default"), then the global document
// (exact, then "default"). A miss everywhere returns ok=false and the caller
// keeps the built-in response.
//
// Within the matched entry: x-status-code-override replaces the returned
// status; the media type is negotiated from the request Accept header
// against the entry's content keys, falling back to the configured default
// media type, then the entry's (deterministic) first media type; the media
// type's example is rendered with placeholder substitution.
func (r *Resolver) Resolve(status int, accept string, perAPI *ErrorResponses, vals PlaceholderValues) (Resolution, bool) {
	entry, ok := lookupEntry(perAPI, status)
	if !ok {
		entry, ok = lookupEntry(r.global, status)
	}
	if !ok {
		return Resolution{}, false
	}

	statusOut := status
	if entry.StatusOverride != nil {
		statusOut = *entry.StatusOverride
	}

	mediaType, ok := negotiateMediaType(accept, entry.Content, r.defaultMediaType)
	if !ok {
		return Resolution{}, false
	}

	example, ok := selectExample(entry.Content[mediaType])
	if !ok {
		return Resolution{}, false
	}

	// Placeholders reflect what the client receives.
	vals.StatusCode = statusOut
	if vals.Message == "" {
		vals.Message = http.StatusText(statusOut)
	}
	if vals.Category == "" {
		vals.Category = CategoryForStatus(status)
	}

	body, err := renderExample(example, mediaType, vals)
	if err != nil {
		return Resolution{}, false
	}
	return Resolution{StatusCode: statusOut, ContentType: mediaType, Body: body}, true
}

// lookupEntry finds the response entry for a status within one document:
// exact status key first, then the document's "default".
func lookupEntry(doc *ErrorResponses, status int) (ResponseObject, bool) {
	if doc == nil {
		return ResponseObject{}, false
	}
	if entry, ok := doc.Responses[strconv.Itoa(status)]; ok {
		return entry, true
	}
	if entry, ok := doc.Responses["default"]; ok {
		return entry, true
	}
	return ResponseObject{}, false
}

// negotiateMediaType picks the media type to render from an entry's content
// map: the first Accept header entry (in order of appearance; q-values are
// not weighted in v1) that matches a content key exactly or by "type/*" /
// "*/*" wildcard, then the configured default media type, then the first
// content key in sorted order.
func negotiateMediaType(accept string, content map[string]MediaType, defaultMediaType string) (string, bool) {
	if len(content) == 0 {
		return "", false
	}

	keys := make([]string, 0, len(content))
	for key := range content {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		accepted, _, err := mime.ParseMediaType(part)
		if err != nil {
			continue
		}
		switch {
		case accepted == "*/*":
			if _, ok := content[defaultMediaType]; ok {
				return defaultMediaType, true
			}
			return keys[0], true
		case strings.HasSuffix(accepted, "/*"):
			prefix := strings.TrimSuffix(accepted, "*")
			for _, key := range keys {
				if strings.HasPrefix(key, prefix) {
					return key, true
				}
			}
		default:
			if _, ok := content[accepted]; ok {
				return accepted, true
			}
		}
	}

	if _, ok := content[defaultMediaType]; ok {
		return defaultMediaType, true
	}
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
