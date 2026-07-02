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

// ErrorResponses is the in-memory form of an error-response configuration
// document: the subset of the OpenAPI 3.x Responses Object the gateway
// understands (see design §4.2). It is produced by Parse/LoadFile and is
// immutable after construction.
type ErrorResponses struct {
	// Responses is keyed by a 3-digit HTTP status code ("401") or "default".
	Responses map[string]ResponseObject
}

// ResponseObject is the subset of the OpenAPI Response Object used for error
// customization.
type ResponseObject struct {
	// Description is the OpenAPI description (documentation only).
	Description string

	// StatusOverride, when set, replaces the entry's key as the returned
	// HTTP status (the x-status-code-override extension, e.g. remap 504→502).
	StatusOverride *int

	// Content maps a media type ("application/json") to the body rendered
	// for it, negotiated against the request Accept header.
	Content map[string]MediaType
}

// MediaType is the subset of the OpenAPI Media Type Object used for error
// customization. The gateway renders Example (or a named entry from
// Examples); Schema is descriptive/validation-only and never synthesized
// into a body.
type MediaType struct {
	Schema   any
	Example  any
	Examples map[string]any
}
