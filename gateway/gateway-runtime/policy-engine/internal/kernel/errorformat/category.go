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

// Package errorformat defines the gateway's error-response taxonomy and the
// OpenAPI-shaped error-response configuration model used to customize the
// bodies and status codes of gateway-generated errors.
//
// This package is deliberately independent of internal/analytics: error
// customization is enforced at the kernel choke points, which run regardless
// of whether analytics is enabled. The category list here is mirrored in the
// gateway-controller (pkg/errorconfig); the table in
// docs/gateway/error-response-customization-design.md §4.1 is the source of
// truth for both.
package errorformat

// Category is a stable, user-facing classification of a gateway error. Users
// configure responses by HTTP status code; categories classify each error
// internally and select the default status code used for the lookup.
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
// Policies may refine this with an explicit hint; without one, the status
// code alone decides. Unmapped 4xx codes classify as RequestMalformed and
// unmapped 5xx codes as InternalError.
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

// AnalyticsFaultCategory maps a category to the analytics FaultCategory name
// for optional correlation in dashboards. This is a pure string mapping — it
// intentionally does NOT import internal/analytics, so error customization
// never depends on analytics state.
func (c Category) AnalyticsFaultCategory() string {
	switch c {
	case BackendUnavailable, BackendTimeout:
		return "TARGET_CONNECTIVITY"
	default:
		return "OTHER"
	}
}
