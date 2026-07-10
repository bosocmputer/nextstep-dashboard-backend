package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/go-chi/chi/v5"
)

func registerViewerRoutes(router chi.Router, viewerAuth ViewerAPI, secureCookies bool) {
	router.Post("/api/v1/viewer/line/session", func(response http.ResponseWriter, request *http.Request) {
		if !isJSONRequest(request) {
			writeProblem(response, request, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.", false)
			return
		}
		var input struct {
			IDToken             string `json:"idToken"`
			InvitationReference string `json:"invitationReference"`
			DeliveryReference   string `json:"deliveryReference"`
		}
		if err := decodeJSON(response, request, &input); err != nil || !validViewerSessionInput(input.IDToken, input.InvitationReference, input.DeliveryReference) {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "LINE session input is invalid.", false)
			return
		}
		result, err := viewerAuth.Exchange(request.Context(), input.IDToken, input.InvitationReference, input.DeliveryReference)
		if handleViewerExchangeError(response, request, err) {
			return
		}
		setViewerSessionCookie(response, result.RawToken, result.ExpiresAt, secureCookies)
		setViewerCSRFCookie(response, result.CSRFToken, result.ExpiresAt, secureCookies)
		writeJSON(response, http.StatusOK, viewerResponse(result.RecipientID.String(), result.DisplayName, result.CSRFToken, result.ExpiresAt))
	})

	router.Get("/api/v1/viewer/me", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		writeJSON(response, http.StatusOK, viewerResponse(authenticated.RecipientID.String(), authenticated.DisplayName, "", authenticated.ExpiresAt))
	})

	router.Post("/api/v1/viewer/logout", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok || !authorizeViewerCSRF(response, request, viewerAuth, authenticated) {
			return
		}
		if err := viewerAuth.Logout(request.Context(), authenticated); err != nil {
			writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to revoke the viewer session.", false)
			return
		}
		clearViewerSessionCookie(response, secureCookies)
		clearViewerCSRFCookie(response, secureCookies)
		response.WriteHeader(http.StatusNoContent)
	})

	router.Get("/api/v1/viewer/tenants", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		items, err := viewerAuth.ListTenants(request.Context(), authenticated.RecipientID)
		if err != nil {
			writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to list viewer tenants.", false)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": items})
	})

	router.Get("/api/v1/viewer/tenants/{tenantId}/reports", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		items, err := viewerAuth.ListReports(request.Context(), authenticated.RecipientID, tenantID)
		if err != nil {
			writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to list viewer reports.", false)
			return
		}
		if len(items) == 0 {
			writeProblem(response, request, http.StatusForbidden, "REPORT_ACCESS_FORBIDDEN", "This tenant is not available to the verified LINE identity.", false)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": items})
	})
}

func validViewerSessionInput(idToken, invitationReference, deliveryReference string) bool {
	if len(idToken) < 32 || len(idToken) > 8192 {
		return false
	}
	if invitationReference != "" && (len(invitationReference) < 32 || len(invitationReference) > 128) {
		return false
	}
	return deliveryReference == "" || len(deliveryReference) >= 32 && len(deliveryReference) <= 512
}

func handleViewerExchangeError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var lineError *line.SafeError
	switch {
	case errors.Is(err, viewer.ErrIdentityForbidden):
		writeProblem(response, request, http.StatusForbidden, "LINE_IDENTITY_FORBIDDEN", "This LINE identity has not been invited or is no longer active.", false)
	case errors.Is(err, viewer.ErrDeliveryReferenceForbidden):
		writeProblem(response, request, http.StatusForbidden, "DELIVERY_REFERENCE_FORBIDDEN", "This report link does not belong to the verified LINE identity.", false)
	case errors.As(err, &lineError):
		status := http.StatusUnauthorized
		message := "LINE identity verification failed."
		if lineError.Retryable {
			status = http.StatusServiceUnavailable
			message = "LINE identity verification is temporarily unavailable."
		}
		writeProblem(response, request, status, lineError.Code, message, lineError.Retryable)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to create a viewer session.", false)
	}
	return true
}

func authenticateViewer(response http.ResponseWriter, request *http.Request, viewerAuth ViewerAPI) (viewer.AuthenticatedViewer, bool) {
	cookie, err := request.Cookie(viewerSessionCookie)
	if err != nil || cookie.Value == "" {
		writeProblem(response, request, http.StatusUnauthorized, "UNAUTHORIZED", "Viewer authentication is required.", false)
		return viewer.AuthenticatedViewer{}, false
	}
	authenticated, err := viewerAuth.Authenticate(request.Context(), cookie.Value)
	if errors.Is(err, viewer.ErrSessionInvalid) {
		writeProblem(response, request, http.StatusUnauthorized, "UNAUTHORIZED", "Viewer session is invalid or expired.", false)
		return viewer.AuthenticatedViewer{}, false
	}
	if err != nil {
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to verify the viewer session.", false)
		return viewer.AuthenticatedViewer{}, false
	}
	return authenticated, true
}

func authorizeViewerCSRF(response http.ResponseWriter, request *http.Request, viewerAuth ViewerAPI, authenticated viewer.AuthenticatedViewer) bool {
	if err := viewerAuth.ValidateCSRF(authenticated, request.Header.Get("X-CSRF-Token")); err != nil {
		writeProblem(response, request, http.StatusForbidden, "CSRF_INVALID", "The request could not be verified.", false)
		return false
	}
	return true
}

func isJSONRequest(request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

func viewerResponse(recipientID, displayName, csrfToken string, expiresAt time.Time) map[string]any {
	result := map[string]any{"recipientId": recipientID, "displayName": displayName, "expiresAt": expiresAt}
	if csrfToken != "" {
		result["csrfToken"] = csrfToken
	}
	return result
}

func setViewerSessionCookie(response http.ResponseWriter, token string, expiresAt time.Time, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name: viewerSessionCookie, Value: token, Path: "/api/v1/viewer", Expires: expiresAt,
		MaxAge: int(time.Until(expiresAt).Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func setViewerCSRFCookie(response http.ResponseWriter, token string, expiresAt time.Time, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name: viewerCSRFCookie, Value: token, Path: "/", Expires: expiresAt,
		MaxAge: int(time.Until(expiresAt).Seconds()), HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func clearViewerSessionCookie(response http.ResponseWriter, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name: viewerSessionCookie, Path: "/api/v1/viewer", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func clearViewerCSRFCookie(response http.ResponseWriter, secure bool) {
	http.SetCookie(response, &http.Cookie{
		Name: viewerCSRFCookie, Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
