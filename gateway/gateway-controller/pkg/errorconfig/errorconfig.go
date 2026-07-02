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

// Package errorconfig defines the controller-side view of the gateway's
// error-response customization: the error-category taxonomy and the
// validation rules for OpenAPI-shaped error-response configuration
// (per-API `errorResponses` and the global error-handling file).
//
// The category list and the validation rules mirror the policy-engine's
// internal/kernel/errorformat package (the engine and controller are
// separate Go modules). The table in
// docs/gateway/error-response-customization-design.md §4.1 is the source of
// truth — keep the two packages in sync.
package errorconfig

import (
	"encoding/json"
	"fmt"
	"mime"
	"regexp"
	"strconv"
	"strings"
)

// Category is a stable classification of a gateway error. Users configure
// responses by HTTP status code; categories classify each error internally
// and select the default status code used for the lookup.
type Category string

const (
	// AuthenticationFailure - auth policy rejected the caller's credentials.
	AuthenticationFailure Category = "AUTHENTICATION_FAILURE"
	// AuthorizationFailure - scope/role/subscription denied.
	AuthorizationFailure Category = "AUTHORIZATION_FAILURE"
	// Throttled - rate limit or quota exceeded.
	Throttled Category = "THROTTLED"
	// RequestMalformed - bad request body or envelope.
	RequestMalformed Category = "REQUEST_MALFORMED"
	// RouteNotFound - no API matched the request.
	RouteNotFound Category = "ROUTE_NOT_FOUND"
	// InternalError - policy execution failure or no policy chain.
	InternalError Category = "INTERNAL_ERROR"
	// BackendUnavailable - no healthy upstream / connection failure.
	BackendUnavailable Category = "BACKEND_UNAVAILABLE"
	// BackendTimeout - upstream did not respond in time.
	BackendTimeout Category = "BACKEND_TIMEOUT"
	// PayloadTooLarge - request exceeded the configured size limit.
	PayloadTooLarge Category = "PAYLOAD_TOO_LARGE"
	// BackendError - the backend itself returned a 4xx/5xx (passthrough by
	// default; reshaping is opt-in per API).
	BackendError Category = "BACKEND_ERROR"
)

// CategoryForStatus returns the default category for an HTTP status code.
// Unmapped 4xx codes classify as RequestMalformed and unmapped 5xx codes as
// InternalError.
func CategoryForStatus(code int) Category {
	switch code {
	case 401:
		return AuthenticationFailure
	case 403:
		return AuthorizationFailure
	case 429:
		return Throttled
	case 400:
		return RequestMalformed
	case 404:
		return RouteNotFound
	case 413:
		return PayloadTooLarge
	case 503:
		return BackendUnavailable
	case 504:
		return BackendTimeout
	}
	if code >= 500 {
		return InternalError
	}
	return RequestMalformed
}

// DefaultStatus returns the HTTP status code a category maps to when nothing
// overrides it. BackendError has no default status (the backend's own status
// passes through), signalled by 0.
func (c Category) DefaultStatus() int {
	switch c {
	case AuthenticationFailure:
		return 401
	case AuthorizationFailure:
		return 403
	case Throttled:
		return 429
	case RequestMalformed:
		return 400
	case RouteNotFound:
		return 404
	case InternalError:
		return 500
	case BackendUnavailable:
		return 503
	case BackendTimeout:
		return 504
	case PayloadTooLarge:
		return 413
	case BackendError:
		return 0
	}
	return 0
}

// MaxExampleBytes caps the size of a single configured example body
// (JSON-serialized size at validation time).
const MaxExampleBytes = 16 * 1024

// allowedPlaceholders is the whitelist of {{placeholder}} names that may
// appear in example bodies (design §4.4).
var allowedPlaceholders = map[string]bool{
	"statusCode": true,
	"message":    true,
	"errorCode":  true,
	"category":   true,
	"requestId":  true,
	"apiName":    true,
	"apiVersion": true,
}

var placeholderPattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

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
// {{placeholder}} names in its string values.
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
				return fmt.Errorf("unknown placeholder {{%s}}: allowed placeholders are statusCode, message, errorCode, category, requestId, apiName, apiVersion", match[1])
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
