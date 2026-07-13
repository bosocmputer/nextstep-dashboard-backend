package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type fakeRecipientAPI struct {
	item        recipient.Recipient
	revokeErr   error
	revokeCalls int
}

func (fake *fakeRecipientAPI) CreateInvitation(context.Context, []byte, string, string, uuid.UUID, string) (recipient.Recipient, error) {
	return fake.item, nil
}

func (fake *fakeRecipientAPI) List(context.Context, uuid.UUID, int, string) (recipient.RecipientPage, error) {
	return recipient.RecipientPage{Data: []recipient.Recipient{fake.item}}, nil
}

func (fake *fakeRecipientAPI) GetForTenant(context.Context, uuid.UUID, uuid.UUID) (recipient.Recipient, error) {
	return fake.item, nil
}

func (fake *fakeRecipientAPI) ReplacePermissions(context.Context, []byte, string, uuid.UUID, uuid.UUID, []report.Key, int) (recipient.Recipient, error) {
	return fake.item, nil
}

func (fake *fakeRecipientAPI) Revoke(context.Context, []byte, string, uuid.UUID, uuid.UUID) error {
	fake.revokeCalls++
	return fake.revokeErr
}

func TestAdminRevokesTenantRecipientWithCSRFGuard(t *testing.T) {
	tenantID, recipientID := uuid.New(), uuid.New()
	api := &fakeRecipientAPI{}
	handler := NewHandler(Dependencies{
		Readiness:  readinessFunc(func(context.Context) error { return nil }),
		AdminAuth:  &fakeAdminAuth{},
		Recipients: api,
	})
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/tenants/"+tenantID.String()+"/recipients/"+recipientID.String(), nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || api.revokeCalls != 1 || response.Body.Len() != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.revokeCalls, response.Body.String())
	}
}

func TestAdminRecipientRevokeReturnsActiveScheduleDependencies(t *testing.T) {
	tenantID, recipientID := uuid.New(), uuid.New()
	api := &fakeRecipientAPI{revokeErr: &recipient.RecipientInUseError{ScheduleNames: []string{"รายงานเช้า"}}}
	handler := NewHandler(Dependencies{
		Readiness:  readinessFunc(func(context.Context) error { return nil }),
		AdminAuth:  &fakeAdminAuth{},
		Recipients: api,
	})
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/tenants/"+tenantID.String()+"/recipients/"+recipientID.String(), nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"RECIPIENT_IN_USE"`) || !strings.Contains(response.Body.String(), "รายงานเช้า") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
