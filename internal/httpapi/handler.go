package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Readiness interface {
	Ping(context.Context) error
}

type AdminAuthenticator interface {
	Login(context.Context, string, string, string) (auth.LoginResult, error)
	Authenticate(context.Context, string) (auth.AuthenticatedAdmin, error)
	ValidateCSRF(auth.AuthenticatedAdmin, string) error
	Logout(context.Context, auth.AuthenticatedAdmin) error
	RotatePassword(context.Context, auth.AuthenticatedAdmin, string, string) error
}

type Dependencies struct {
	Readiness       Readiness
	AdminAuth       AdminAuthenticator
	Tenants         TenantAPI
	SMLConnections  SMLAPI
	Recipients      RecipientAPI
	Schedules       ScheduleAPI
	FlexPreviews    SchedulePreviewAPI
	ScheduleTests   ScheduleTestSendAPI
	Operations      OperationsAPI
	ViewerAuth      ViewerAPI
	ViewerReports   ViewerReportAPI
	RefreshPolicies RefreshPolicyAPI
	SecureCookies   bool
	Logger          *slog.Logger
}

type problemEnvelope struct {
	Error problem `json:"error"`
}

type problem struct {
	Code        string       `json:"code"`
	Message     string       `json:"message"`
	RequestID   string       `json:"requestId"`
	Retryable   bool         `json:"retryable"`
	FieldErrors []fieldError `json:"fieldErrors,omitempty"`
}

type fieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type requestIDContextKey struct{}

const (
	adminSessionCookie  = "nextstep_admin_session"
	adminCSRFCookie     = "nextstep_admin_csrf"
	viewerSessionCookie = "nextstep_viewer_session"
	viewerCSRFCookie    = "nextstep_viewer_csrf"
)

func NewHandler(dependencies Dependencies) http.Handler {
	router := chi.NewRouter()
	router.Use(securityHeaders(dependencies.SecureCookies))
	router.Use(requestIDMiddleware)
	if dependencies.Logger != nil {
		router.Use(requestLogMiddleware(dependencies.Logger))
	}
	router.Use(recoverMiddleware)
	router.Get("/api/v1/health/live", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.Get("/api/v1/health/ready", func(response http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		if err := dependencies.Readiness.Ping(ctx); err != nil {
			writeProblem(response, request, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service is not ready.", true)
			return
		}
		writeJSON(response, http.StatusOK, map[string]string{"status": "ready"})
	})
	if dependencies.AdminAuth != nil {
		registerAdminAuthRoutes(router, dependencies.AdminAuth, dependencies.SecureCookies)
		registerAdminReportRoutes(router, dependencies.AdminAuth)
		if dependencies.Tenants != nil {
			registerTenantRoutes(router, dependencies.AdminAuth, dependencies.Tenants)
		}
		if dependencies.RefreshPolicies != nil {
			registerRefreshPolicyRoutes(router, dependencies.AdminAuth, dependencies.RefreshPolicies)
		}
		if dependencies.SMLConnections != nil {
			registerSMLRoutes(router, dependencies.AdminAuth, dependencies.SMLConnections)
		}
		if dependencies.Recipients != nil {
			registerRecipientRoutes(router, dependencies.AdminAuth, dependencies.Recipients)
		}
		if dependencies.Schedules != nil {
			registerScheduleRoutes(router, dependencies.AdminAuth, dependencies.Schedules, dependencies.FlexPreviews, dependencies.ScheduleTests)
		}
		if dependencies.Operations != nil {
			registerOperationsRoutes(router, dependencies.AdminAuth, dependencies.Operations)
		}
	}
	if dependencies.ViewerAuth != nil {
		registerViewerRoutes(router, dependencies.ViewerAuth, dependencies.SecureCookies)
		if dependencies.ViewerReports != nil {
			registerViewerReportRoutes(router, dependencies.ViewerAuth, dependencies.ViewerReports)
		}
	}
	router.NotFound(func(response http.ResponseWriter, request *http.Request) {
		writeProblem(response, request, http.StatusNotFound, "NOT_FOUND", "The requested resource was not found.", false)
	})
	return router
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (recorder *responseRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }

func (recorder *responseRecorder) WriteHeader(status int) {
	if recorder.status != 0 {
		return
	}
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *responseRecorder) Write(body []byte) (int, error) {
	if recorder.status == 0 {
		recorder.WriteHeader(http.StatusOK)
	}
	written, err := recorder.ResponseWriter.Write(body)
	recorder.bytes += written
	return written, err
}

func requestLogMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			started := time.Now()
			recorder := &responseRecorder{ResponseWriter: response}
			next.ServeHTTP(recorder, request)
			if request.URL.Path == "/api/v1/health/live" || request.URL.Path == "/api/v1/health/ready" {
				return
			}
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			route := chi.RouteContext(request.Context()).RoutePattern()
			if route == "" {
				route = "UNMATCHED"
			}
			arguments := []any{
				"method", request.Method, "route", route, "status", status, "bytes", recorder.bytes,
				"durationMs", time.Since(started).Milliseconds(), "requestId", requestID(request),
			}
			switch {
			case status >= 500:
				logger.Error("HTTP request completed", arguments...)
			case status >= 400:
				logger.Warn("HTTP request completed", arguments...)
			default:
				logger.Info("HTTP request completed", arguments...)
			}
		})
	}
}

type ViewerReportAPI interface {
	Create(context.Context, uuid.UUID, uuid.UUID, report.Key, string, viewer.CreateReportRunInput) (report.Run, error)
	Get(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID) (report.Run, error)
	GetDashboard(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID) (report.Dashboard, error)
	ExecutiveOverview(context.Context, uuid.UUID, uuid.UUID) (viewer.ExecutiveOverview, error)
	CreateDashboardRefresh(context.Context, uuid.UUID, uuid.UUID, string, *viewer.DashboardRefreshInput) (viewer.DashboardRefresh, error)
	GetDashboardRefresh(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (viewer.DashboardRefresh, error)
	GetDashboardRefreshResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (viewer.DashboardRefreshResult, error)
	ListRows(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID, string, int) (viewer.ReportRows, error)
	Cancel(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID) (report.Run, error)
	ExactSnapshot(context.Context, uuid.UUID, uuid.UUID, report.Key, viewer.CreateReportRunInput) (viewer.DashboardSnapshot, error)
	Revalidate(context.Context, uuid.UUID, uuid.UUID, report.Key, viewer.CreateReportRunInput) (viewer.ReportRevalidation, error)
	RevalidateOverview(context.Context, uuid.UUID, uuid.UUID, viewer.DashboardRefreshInput) (viewer.OverviewRevalidation, error)
}

type ViewerAPI interface {
	Exchange(context.Context, string, string, string) (viewer.ExchangeResult, error)
	Authenticate(context.Context, string) (viewer.AuthenticatedViewer, error)
	ValidateCSRF(viewer.AuthenticatedViewer, string) error
	Logout(context.Context, viewer.AuthenticatedViewer) error
	ListTenants(context.Context, uuid.UUID) ([]viewer.TenantAccess, error)
	ListReports(context.Context, uuid.UUID, uuid.UUID) ([]viewer.ReportAccess, error)
	CanAccessReport(context.Context, uuid.UUID, uuid.UUID, report.Key) (bool, error)
}

func writeValidationProblem(response http.ResponseWriter, request *http.Request, validationError *tenant.ValidationError) {
	requestID, _ := request.Context().Value(requestIDContextKey{}).(string)
	writeJSON(response, http.StatusUnprocessableEntity, problemEnvelope{Error: problem{
		Code:      "VALIDATION_ERROR",
		Message:   "Request validation failed.",
		RequestID: requestID,
		Retryable: false,
		FieldErrors: []fieldError{{
			Field:   validationError.Field,
			Code:    validationError.Code,
			Message: validationError.Message,
		}},
	}})
}

func securityHeaders(useHSTS bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			response.Header().Set("X-Content-Type-Options", "nosniff")
			response.Header().Set("X-Frame-Options", "DENY")
			response.Header().Set("X-XSS-Protection", "0")
			response.Header().Set("Referrer-Policy", "no-referrer")
			response.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			response.Header().Set("Cache-Control", "no-store")
			if useHSTS {
				response.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(response, request)
		})
	}
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer func() {
			if recover() != nil {
				writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "An unexpected error occurred.", false)
			}
		}()
		next.ServeHTTP(response, request)
	})
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if requestID == "" || len(requestID) > 128 {
			requestID = uuid.NewString()
		}
		response.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(request.Context(), requestIDContextKey{}, requestID)
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

func writeProblem(response http.ResponseWriter, request *http.Request, status int, code, message string, retryable bool) {
	requestID, _ := request.Context().Value(requestIDContextKey{}).(string)
	writeJSON(response, status, problemEnvelope{Error: problem{
		Code:      code,
		Message:   message,
		RequestID: requestID,
		Retryable: retryable,
	}})
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func registerAdminAuthRoutes(router chi.Router, adminAuth AdminAuthenticator, secureCookies bool) {
	router.Post("/api/v1/auth/admin/login", func(response http.ResponseWriter, request *http.Request) {
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decodeJSON(response, request, &input); err != nil || len(input.Username) < 1 || len(input.Username) > 120 || len(input.Password) < 1 || len(input.Password) > auth.MaximumAdminPasswordBytes {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Login input is invalid.", false)
			return
		}
		result, err := adminAuth.Login(request.Context(), input.Username, input.Password, remoteIdentity(request))
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeProblem(response, request, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Username or password is incorrect.", false)
			return
		case errors.Is(err, auth.ErrLoginLocked):
			response.Header().Set("Retry-After", "900")
			writeProblem(response, request, http.StatusTooManyRequests, "LOGIN_LOCKED", "Too many login attempts. Try again later.", true)
			return
		case err != nil:
			writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to create an admin session.", false)
			return
		}
		setAdminSessionCookie(response, result.RawToken, result.ExpiresAt, secureCookies)
		setAdminCSRFCookie(response, result.CSRFToken, result.ExpiresAt, secureCookies)
		writeJSON(response, http.StatusOK, adminSessionResponse(result.Username, result.CSRFToken, result.ExpiresAt, result.MustRotatePassword))
	})

	router.Get("/api/v1/auth/admin/session", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := authenticateAdmin(response, request, adminAuth)
		if !ok {
			return
		}
		writeJSON(response, http.StatusOK, adminSessionResponse(admin.Username, "", admin.ExpiresAt, admin.MustRotatePassword))
	})

	router.Post("/api/v1/auth/admin/logout", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := authenticateAdmin(response, request, adminAuth)
		if !ok || !authorizeCSRF(response, request, adminAuth, admin) {
			return
		}
		if err := adminAuth.Logout(request.Context(), admin); err != nil {
			writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to revoke the admin session.", false)
			return
		}
		clearAdminSessionCookie(response, secureCookies)
		clearAdminCSRFCookie(response, secureCookies)
		response.WriteHeader(http.StatusNoContent)
	})

	router.Put("/api/v1/auth/admin/password", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := authenticateAdmin(response, request, adminAuth)
		if !ok || !authorizeCSRF(response, request, adminAuth, admin) {
			return
		}
		var input struct {
			CurrentPassword string `json:"currentPassword"`
			NewPassword     string `json:"newPassword"`
		}
		if err := decodeJSON(response, request, &input); err != nil || len(input.CurrentPassword) < 1 || len(input.CurrentPassword) > auth.MaximumAdminPasswordBytes || auth.ValidateAdminPassword(input.NewPassword) != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Password input is invalid.", false)
			return
		}
		err := adminAuth.RotatePassword(request.Context(), admin, input.CurrentPassword, input.NewPassword)
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeProblem(response, request, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Current password is incorrect.", false)
			return
		case errors.Is(err, auth.ErrPasswordUnchanged):
			writeProblem(response, request, http.StatusUnprocessableEntity, "PASSWORD_UNCHANGED", "New password must differ from the current password.", false)
			return
		case err != nil:
			writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to rotate the admin password.", false)
			return
		}
		admin.MustRotatePassword = false
		writeJSON(response, http.StatusOK, adminSessionResponse(admin.Username, request.Header.Get("X-CSRF-Token"), admin.ExpiresAt, false))
	})
}

func decodeJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 64*1024)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

func authenticateAdmin(response http.ResponseWriter, request *http.Request, adminAuth AdminAuthenticator) (auth.AuthenticatedAdmin, bool) {
	cookie, err := request.Cookie(adminSessionCookie)
	if err != nil || cookie.Value == "" {
		writeProblem(response, request, http.StatusUnauthorized, "UNAUTHORIZED", "Admin authentication is required.", false)
		return auth.AuthenticatedAdmin{}, false
	}
	admin, err := adminAuth.Authenticate(request.Context(), cookie.Value)
	if errors.Is(err, auth.ErrInvalidSession) {
		writeProblem(response, request, http.StatusUnauthorized, "UNAUTHORIZED", "Admin session is invalid or expired.", false)
		return auth.AuthenticatedAdmin{}, false
	}
	if err != nil {
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to verify the admin session.", false)
		return auth.AuthenticatedAdmin{}, false
	}
	return admin, true
}

func authorizeCSRF(response http.ResponseWriter, request *http.Request, adminAuth AdminAuthenticator, admin auth.AuthenticatedAdmin) bool {
	if err := adminAuth.ValidateCSRF(admin, request.Header.Get("X-CSRF-Token")); err != nil {
		writeProblem(response, request, http.StatusForbidden, "CSRF_INVALID", "The request could not be verified.", false)
		return false
	}
	return true
}

func adminSessionResponse(username, csrfToken string, expiresAt time.Time, mustRotate bool) map[string]any {
	response := map[string]any{
		"username":                    username,
		"expiresAt":                   expiresAt,
		"mustRotateBootstrapPassword": mustRotate,
	}
	if csrfToken != "" {
		response["csrfToken"] = csrfToken
	}
	return response
}

func setAdminSessionCookie(response http.ResponseWriter, token string, expiresAt time.Time, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    token,
		Path:     "/api/v1",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearAdminSessionCookie(response http.ResponseWriter, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    "",
		Path:     "/api/v1",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func setAdminCSRFCookie(response http.ResponseWriter, token string, expiresAt time.Time, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name:     adminCSRFCookie,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearAdminCSRFCookie(response http.ResponseWriter, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name:     adminCSRFCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func remoteIdentity(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(request.RemoteAddr)
}
