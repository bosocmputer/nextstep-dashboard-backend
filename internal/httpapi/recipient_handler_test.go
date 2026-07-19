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
	item         recipient.Recipient
	revokeErr    error
	revokeCalls  int
	reissueCalls int
	queryInput   recipient.QueryInput
}

func (fake *fakeRecipientAPI) CreateInvitation(context.Context, []byte, string, string, uuid.UUID, string) (recipient.Recipient, error) {
	return fake.item, nil
}

func (fake *fakeRecipientAPI) ReissueInvitation(context.Context, []byte, string, string, uuid.UUID, uuid.UUID) (recipient.Recipient, error) {
	fake.reissueCalls++
	return fake.item, nil
}

func (fake *fakeRecipientAPI) List(context.Context, uuid.UUID, int, string) (recipient.RecipientPage, error) {
	return recipient.RecipientPage{Data: []recipient.Recipient{fake.item}}, nil
}

func (fake *fakeRecipientAPI) GetForTenant(context.Context, uuid.UUID, uuid.UUID) (recipient.Recipient, error) {
	return fake.item, nil
}

func (fake *fakeRecipientAPI) PermissionDependencies(context.Context, uuid.UUID, uuid.UUID) (recipient.PermissionDependencies, error) {
	return recipient.PermissionDependencies{RecipientID: fake.item.ID, PermissionsVersion: fake.item.PermissionsVersion, Items: []recipient.PermissionDependency{}}, nil
}

func (fake *fakeRecipientAPI) ScheduleRecipientOptions(context.Context, uuid.UUID, recipient.ScheduleRecipientOptionsInput) (recipient.ScheduleRecipientOptions, error) {
	return recipient.ScheduleRecipientOptions{Data: []recipient.ScheduleRecipientOption{}}, nil
}

func (fake *fakeRecipientAPI) Query(_ context.Context, _ uuid.UUID, input recipient.QueryInput) (recipient.QueryResult, error) {
	fake.queryInput = input
	return recipient.QueryResult{Data: []recipient.Recipient{fake.item}, Page: input.Page, PageSize: input.PageSize, Total: 1}, nil
}

func TestAdminQueriesRecipientsWithExactPaginationAndFilters(t *testing.T) {
	tenantID := uuid.New()
	api := &fakeRecipientAPI{item: recipient.Recipient{ID: uuid.New(), DisplayName: "ผู้บริหาร"}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Recipients: api})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID.String()+"/recipients/query", strings.NewReader(`{"search":"ผู้","status":"ACTIVE","permissionState":"WITH_REPORTS","page":2,"pageSize":25}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || api.queryInput.Page != 2 || api.queryInput.PageSize != 25 || api.queryInput.Status != recipient.StatusActive || api.queryInput.PermissionState != "WITH_REPORTS" || !strings.Contains(response.Body.String(), `"total":1`) {
		t.Fatalf("status=%d input=%+v body=%s", response.Code, api.queryInput, response.Body.String())
	}
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

func TestAdminReissuesPendingRecipientInvitationWithMutationGuards(t *testing.T) {
	tenantID, recipientID := uuid.New(), uuid.New()
	api := &fakeRecipientAPI{item: recipient.Recipient{ID: recipientID, Status: recipient.StatusPending, InvitationURL: "https://dashboard.nextstep-soft.com/app/invite?ref=new"}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Recipients: api})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID.String()+"/recipients/"+recipientID.String()+"/invitation", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	request.Header.Set("Idempotency-Key", "recipient-reissue-test")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || api.reissueCalls != 1 || !strings.Contains(response.Body.String(), `"invitationUrl":"https://dashboard.nextstep-soft.com/app/invite?ref=new"`) {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.reissueCalls, response.Body.String())
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
