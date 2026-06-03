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

package handlers

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wso2/api-platform/common/eventhub"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/middleware"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/utils"
)

// CreateSoapAPI implements ServerInterface.CreateSoapAPI
// (POST /soap-apis)
func (s *APIServer) CreateSoapAPI(c *gin.Context) {
	log := middleware.GetLogger(c, s.logger)

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Error("Failed to read request body", slog.Any("error", err))
		c.JSON(http.StatusBadRequest, api.ErrorResponse{
			Status:  "error",
			Message: "Failed to read request body",
		})
		return
	}

	correlationID := middleware.GetCorrelationID(c)

	result, err := s.deploymentService.DeployAPIConfiguration(utils.APIDeploymentParams{
		Data:          body,
		ContentType:   c.GetHeader("Content-Type"),
		Kind:          "SoapApi",
		APIID:         "",
		Origin:        models.OriginGatewayAPI,
		CorrelationID: correlationID,
		Logger:        log,
	})
	if err != nil {
		log.Error("Failed to deploy SOAP API configuration", slog.Any("error", err))
		if storage.IsConflictError(err) {
			c.JSON(http.StatusConflict, api.ErrorResponse{
				Status:  "error",
				Message: err.Error(),
			})
			return
		}
		if mapRenderError(c, "create", err) {
			return
		}
		c.JSON(http.StatusBadRequest, api.ErrorResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	cfg := result.StoredConfig

	c.JSON(http.StatusCreated, buildResourceResponseFromStored(cfg.SourceConfiguration, cfg))

	if result.IsStale {
		return
	}

	if s.controlPlaneClient != nil && s.controlPlaneClient.IsConnected() && s.systemConfig.Controller.ControlPlane.DeploymentPushEnabled {
		go s.waitForDeploymentAndPush(cfg.UUID, correlationID, log)
	}
}

// ListSoapAPIs implements ServerInterface.ListSoapAPIs
// (GET /soap-apis)
func (s *APIServer) ListSoapAPIs(c *gin.Context, params api.ListSoapAPIsParams) {
	if (params.DisplayName != nil && *params.DisplayName != "") ||
		(params.Version != nil && *params.Version != "") ||
		(params.Context != nil && *params.Context != "") ||
		(params.Status != nil && *params.Status != "") {
		s.SearchDeployments(c, string(api.SoapAPIKindSoapApi))
		return
	}

	configs, err := s.db.GetAllConfigsByKind(string(api.SoapAPIKindSoapApi))
	if err != nil {
		s.logger.Error("Failed to list SOAP APIs", slog.Any("error", err))
		c.JSON(http.StatusInternalServerError, api.ErrorResponse{
			Status:  "error",
			Message: "Failed to list SOAP API configurations",
		})
		return
	}

	items := make([]any, 0, len(configs))
	for _, cfg := range configs {
		items = append(items, buildResourceResponseFromStored(cfg.SourceConfiguration, cfg))
	}

	c.JSON(http.StatusOK, gin.H{
		"status":   "success",
		"count":    len(items),
		"soapApis": items,
	})
}

// GetSoapAPIById implements ServerInterface.GetSoapAPIById
// (GET /soap-apis/{id})
func (s *APIServer) GetSoapAPIById(c *gin.Context, id string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id

	cfg, err := s.db.GetConfigByKindAndHandle(models.KindSoapApi, handle)
	if err != nil {
		if storage.IsDatabaseUnavailableError(err) {
			c.JSON(http.StatusServiceUnavailable, api.ErrorResponse{
				Status:  "error",
				Message: "Database storage not available",
			})
			return
		}
		log.Warn("SOAP API configuration not found", slog.String("handle", handle))
		c.JSON(http.StatusNotFound, api.ErrorResponse{
			Status:  "error",
			Message: fmt.Sprintf("SOAP API configuration with handle '%s' not found", handle),
		})
		return
	}

	c.JSON(http.StatusOK, buildResourceResponseFromStored(cfg.SourceConfiguration, cfg))
}

// UpdateSoapAPI implements ServerInterface.UpdateSoapAPI
// (PUT /soap-apis/{id})
func (s *APIServer) UpdateSoapAPI(c *gin.Context, id string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Error("Failed to read request body", slog.Any("error", err))
		c.JSON(http.StatusBadRequest, api.ErrorResponse{
			Status:  "error",
			Message: "Failed to read request body",
		})
		return
	}

	existing, err := s.db.GetConfigByKindAndHandle(models.KindSoapApi, handle)
	if err != nil {
		log.Warn("SOAP API configuration not found", slog.String("handle", handle))
		c.JSON(http.StatusNotFound, api.ErrorResponse{
			Status:  "error",
			Message: fmt.Sprintf("SOAP API configuration with handle '%s' not found", handle),
		})
		return
	}

	correlationID := middleware.GetCorrelationID(c)

	result, err := s.deploymentService.DeployAPIConfiguration(utils.APIDeploymentParams{
		Data:          body,
		ContentType:   c.GetHeader("Content-Type"),
		Kind:          "SoapApi",
		APIID:         existing.UUID,
		Origin:        models.OriginGatewayAPI,
		CorrelationID: correlationID,
		Logger:        log,
	})
	if err != nil {
		log.Error("Failed to update SOAP API configuration", slog.Any("error", err))
		if storage.IsConflictError(err) {
			c.JSON(http.StatusConflict, api.ErrorResponse{
				Status:  "error",
				Message: err.Error(),
			})
			return
		}
		if mapRenderError(c, "update", err) {
			return
		}
		c.JSON(http.StatusBadRequest, api.ErrorResponse{
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	updated := result.StoredConfig
	log.Info("SOAP API configuration updated",
		slog.String("id", updated.UUID),
		slog.String("handle", handle))

	c.JSON(http.StatusOK, buildResourceResponseFromStored(updated.SourceConfiguration, updated))
}

// DeleteSoapAPI implements ServerInterface.DeleteSoapAPI
// (DELETE /soap-apis/{id})
func (s *APIServer) DeleteSoapAPI(c *gin.Context, id string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id
	correlationID := middleware.GetCorrelationID(c)

	cfg, err := s.db.GetConfigByKindAndHandle(models.KindSoapApi, handle)
	if err != nil {
		log.Warn("SOAP API configuration not found", slog.String("handle", handle))
		c.JSON(http.StatusNotFound, api.ErrorResponse{
			Status:  "error",
			Message: fmt.Sprintf("SOAP API configuration with handle '%s' not found", handle),
		})
		return
	}

	if err := s.db.DeleteConfig(cfg.UUID); err != nil {
		log.Error("Failed to delete SOAP API config from database", slog.Any("error", err))
		c.JSON(http.StatusInternalServerError, api.ErrorResponse{
			Status:  "error",
			Message: "Failed to delete configuration",
		})
		return
	}

	if err := s.db.RemoveAPIKeysAPI(cfg.UUID); err != nil {
		log.Warn("Failed to remove API keys from database",
			slog.String("handle", handle),
			slog.Any("error", err))
	}

	s.publishWebSubEvent(eventhub.EventTypeAPI, "DELETE", cfg.UUID, correlationID, log)

	log.Info("SOAP API configuration deleted",
		slog.String("id", cfg.UUID),
		slog.String("handle", handle))

	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"message": "SOAP API configuration deleted successfully",
		"id":      handle,
	})
}

// CreateSoapAPIKey implements ServerInterface.CreateSoapAPIKey
// (POST /soap-apis/{id}/api-keys)
func (s *APIServer) CreateSoapAPIKey(c *gin.Context, id string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id
	correlationID := middleware.GetCorrelationID(c)

	user, ok := s.extractAuthenticatedUser(c, "CreateSoapAPIKey", correlationID)
	if !ok {
		return
	}

	var request api.APIKeyCreationRequest
	if err := s.bindRequestBody(c, &request); err != nil {
		log.Error("Failed to parse request body for SOAP API key creation",
			slog.Any("error", err),
			slog.String("handle", handle))
		c.JSON(http.StatusBadRequest, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("Invalid request body: %v", err)})
		return
	}

	params := utils.APIKeyCreationParams{
		Kind:          models.KindSoapApi,
		Handle:        handle,
		Request:       request,
		User:          user,
		CorrelationID: correlationID,
		Logger:        log,
	}

	result, err := s.apiKeyService.CreateAPIKey(params)
	if err != nil {
		if storage.IsNotFoundError(err) {
			c.JSON(http.StatusNotFound, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("SOAP API '%s' not found", handle)})
		} else if storage.IsConflictError(err) {
			c.JSON(http.StatusConflict, api.ErrorResponse{Status: "error", Message: err.Error()})
		} else {
			log.Error("Failed to create SOAP API key", slog.String("handle", handle), slog.Any("error", err))
			c.JSON(http.StatusInternalServerError, api.ErrorResponse{Status: "error", Message: "Failed to create API key"})
		}
		return
	}

	c.JSON(http.StatusCreated, result.Response)
}

// ListSoapAPIKeys implements ServerInterface.ListSoapAPIKeys
// (GET /soap-apis/{id}/api-keys)
func (s *APIServer) ListSoapAPIKeys(c *gin.Context, id string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id
	correlationID := middleware.GetCorrelationID(c)

	user, ok := s.extractAuthenticatedUser(c, "ListSoapAPIKeys", correlationID)
	if !ok {
		return
	}

	params := utils.ListAPIKeyParams{
		Kind:          models.KindSoapApi,
		Handle:        handle,
		User:          user,
		CorrelationID: correlationID,
		Logger:        log,
	}

	result, err := s.apiKeyService.ListAPIKeys(params)
	if err != nil {
		if storage.IsNotFoundError(err) {
			c.JSON(http.StatusNotFound, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("SOAP API '%s' not found", handle)})
		} else {
			log.Error("Failed to list SOAP API keys", slog.String("handle", handle), slog.Any("error", err))
			c.JSON(http.StatusInternalServerError, api.ErrorResponse{Status: "error", Message: "Failed to list API keys"})
		}
		return
	}

	c.JSON(http.StatusOK, result.Response)
}

// RevokeSoapAPIKey implements ServerInterface.RevokeSoapAPIKey
// (DELETE /soap-apis/{id}/api-keys/{apiKeyName})
func (s *APIServer) RevokeSoapAPIKey(c *gin.Context, id string, apiKeyName string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id
	correlationID := middleware.GetCorrelationID(c)

	user, ok := s.extractAuthenticatedUser(c, "RevokeSoapAPIKey", correlationID)
	if !ok {
		return
	}

	params := utils.APIKeyRevocationParams{
		Kind:          models.KindSoapApi,
		Handle:        handle,
		APIKeyName:    apiKeyName,
		User:          user,
		CorrelationID: correlationID,
		Logger:        log,
	}

	result, err := s.apiKeyService.RevokeAPIKey(params)
	if err != nil {
		if storage.IsNotFoundError(err) {
			c.JSON(http.StatusNotFound, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("SOAP API '%s' not found", handle)})
		} else {
			log.Error("Failed to revoke SOAP API key", slog.String("handle", handle), slog.String("key", apiKeyName), slog.Any("error", err))
			c.JSON(http.StatusInternalServerError, api.ErrorResponse{Status: "error", Message: "Failed to revoke API key"})
		}
		return
	}

	c.JSON(http.StatusOK, result.Response)
}

// UpdateSoapAPIKey implements ServerInterface.UpdateSoapAPIKey
// (PUT /soap-apis/{id}/api-keys/{apiKeyName})
func (s *APIServer) UpdateSoapAPIKey(c *gin.Context, id string, apiKeyName string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id
	correlationID := middleware.GetCorrelationID(c)

	user, ok := s.extractAuthenticatedUser(c, "UpdateSoapAPIKey", correlationID)
	if !ok {
		return
	}

	var request api.APIKeyCreationRequest
	if err := s.bindRequestBody(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("Invalid request body: %v", err)})
		return
	}

	if request.ApiKey == nil || strings.TrimSpace(*request.ApiKey) == "" {
		c.JSON(http.StatusBadRequest, api.ErrorResponse{Status: "error", Message: "apiKey is required"})
		return
	}

	params := utils.APIKeyUpdateParams{
		Kind:          models.KindSoapApi,
		Handle:        handle,
		APIKeyName:    apiKeyName,
		Request:       request,
		User:          user,
		CorrelationID: correlationID,
		Logger:        log,
	}

	result, err := s.apiKeyService.UpdateAPIKey(params)
	if err != nil {
		if storage.IsOperationNotAllowedError(err) {
			c.JSON(http.StatusBadRequest, api.ErrorResponse{Status: "error", Message: err.Error()})
		} else if storage.IsNotFoundError(err) {
			c.JSON(http.StatusNotFound, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("SOAP API or API key '%s' not found", apiKeyName)})
		} else if storage.IsConflictError(err) {
			c.JSON(http.StatusConflict, api.ErrorResponse{Status: "error", Message: err.Error()})
		} else {
			log.Error("Failed to update SOAP API key", slog.String("handle", handle), slog.String("key", apiKeyName), slog.Any("error", err))
			c.JSON(http.StatusInternalServerError, api.ErrorResponse{Status: "error", Message: "Failed to update API key"})
		}
		return
	}

	c.JSON(http.StatusOK, result.Response)
}

// RegenerateSoapAPIKey implements ServerInterface.RegenerateSoapAPIKey
// (POST /soap-apis/{id}/api-keys/{apiKeyName}/regenerate)
func (s *APIServer) RegenerateSoapAPIKey(c *gin.Context, id string, apiKeyName string) {
	log := middleware.GetLogger(c, s.logger)
	handle := id
	correlationID := middleware.GetCorrelationID(c)

	user, ok := s.extractAuthenticatedUser(c, "RegenerateSoapAPIKey", correlationID)
	if !ok {
		return
	}

	var request api.APIKeyRegenerationRequest
	if err := s.bindRequestBody(c, &request); err != nil {
		c.JSON(http.StatusBadRequest, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("Invalid request body: %v", err)})
		return
	}

	params := utils.APIKeyRegenerationParams{
		Kind:          models.KindSoapApi,
		Handle:        handle,
		APIKeyName:    apiKeyName,
		Request:       request,
		User:          user,
		CorrelationID: correlationID,
		Logger:        log,
	}

	result, err := s.apiKeyService.RegenerateAPIKey(params)
	if err != nil {
		if storage.IsNotFoundError(err) {
			c.JSON(http.StatusNotFound, api.ErrorResponse{Status: "error", Message: fmt.Sprintf("SOAP API or API key '%s' not found", apiKeyName)})
		} else {
			log.Error("Failed to regenerate SOAP API key", slog.String("handle", handle), slog.String("key", apiKeyName), slog.Any("error", err))
			c.JSON(http.StatusInternalServerError, api.ErrorResponse{Status: "error", Message: "Failed to regenerate API key"})
		}
		return
	}

	c.JSON(http.StatusOK, result.Response)
}
