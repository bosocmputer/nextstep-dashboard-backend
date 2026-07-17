package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type SMLAPI interface {
	Get(context.Context, uuid.UUID) (sml.ConnectionStatus, error)
	Replace(context.Context, []byte, string, uuid.UUID, sml.ConnectionInput) (sml.ConnectionStatus, error)
	Test(context.Context, []byte, string, uuid.UUID) (sml.ConnectionTestResult, error)
}

func registerSMLRoutes(router chi.Router, adminAuth AdminAuthenticator, smlConnections SMLAPI) {
	router.Get("/api/v1/admin/tenants/{tenantId}/sml-connection", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		status, err := smlConnections.Get(request.Context(), tenantID)
		if handleSMLError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, status)
	})

	router.Put("/api/v1/admin/tenants/{tenantId}/sml-connection", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		var input struct {
			EndpointURL    string `json:"endpointUrl"`
			ConfigFileName string `json:"configFileName"`
			DatabaseName   string `json:"databaseName"`
			Username       string `json:"username"`
			Password       string `json:"password"`
			Version        int    `json:"version"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "SML connection input is invalid.", false)
			return
		}
		status, err := smlConnections.Replace(request.Context(), admin.TokenHash, requestID(request), tenantID, sml.ConnectionInput{
			EndpointURL: input.EndpointURL, ConfigFileName: input.ConfigFileName, DatabaseName: input.DatabaseName,
			Username: input.Username, Password: input.Password, ExpectedVersion: input.Version,
		})
		if handleSMLError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, status)
	})

	router.Post("/api/v1/admin/tenants/{tenantId}/sml-connection/test", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		result, err := smlConnections.Test(request.Context(), admin.TokenHash, requestID(request), tenantID)
		if handleSMLError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, result)
	})
}

func handleSMLError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var validationError *sml.ValidationError
	var testError *sml.ConnectionTestError
	switch {
	case errors.As(err, &validationError):
		writeValidationProblem(response, request, &tenant.ValidationError{Field: validationError.Field, Code: validationError.Code, Message: "SML connection field is invalid."})
	case errors.Is(err, sml.ErrConnectionNotConfigured):
		writeProblem(response, request, http.StatusNotFound, "SML_NOT_CONFIGURED", "SML connection is not configured.", false)
	case errors.Is(err, sml.ErrConnectionVersionConflict):
		writeProblem(response, request, http.StatusConflict, "VERSION_CONFLICT", "SML connection changed since it was loaded. Reload before saving again.", false)
	case errors.As(err, &testError):
		if testError.RetryAfter != nil {
			retrySeconds := int(time.Until(*testError.RetryAfter).Seconds())
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			response.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		}
		status := http.StatusFailedDependency
		if testError.SafeCode == "SML_TEST_BUSY" {
			status = http.StatusConflict
		} else if testError.SafeCode == "SML_TEST_COOLDOWN" {
			status = http.StatusTooManyRequests
		}
		writeProblem(response, request, status, testError.SafeCode, "SML connection test failed safely.", testError.Retryable)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to process the SML connection.", false)
	}
	return true
}

func validIdempotencyHeader(response http.ResponseWriter, request *http.Request) bool {
	value := request.Header.Get("Idempotency-Key")
	if len(value) < 8 || len(value) > 200 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n\t") {
		writeProblem(response, request, http.StatusUnprocessableEntity, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key must contain 8 to 200 characters.", false)
		return false
	}
	return true
}
