package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/google/uuid"
)

type fakeScheduleAPI struct {
	item            schedule.Schedule
	createErr       error
	updateErr       error
	activateErr     error
	createCall      int
	updateCall      int
	archiveCall     int
	restoreCall     int
	includeArchived bool
	listFilter      schedule.ListFilter
}

type fakeSchedulePreviewAPI struct {
	item  line.FlexPreview
	err   error
	calls int
}

type fakeScheduleTestSendAPI struct {
	execution schedule.Execution
	err       error
	calls     int
}

func (fake *fakeScheduleTestSendAPI) Enqueue(context.Context, []byte, string, string, uuid.UUID, uuid.UUID) (schedule.Execution, error) {
	fake.calls++
	return fake.execution, fake.err
}

func (fake *fakeSchedulePreviewAPI) Preview(context.Context, uuid.UUID, line.FlexPreviewInput) (line.FlexPreview, error) {
	fake.calls++
	return fake.item, fake.err
}

func (fake *fakeScheduleAPI) Create(context.Context, []byte, string, string, uuid.UUID, schedule.Input) (schedule.Schedule, error) {
	fake.createCall++
	return fake.item, fake.createErr
}

func (fake *fakeScheduleAPI) List(_ context.Context, filter schedule.ListFilter) (schedule.Page, error) {
	fake.includeArchived = filter.IncludeArchived
	fake.listFilter = filter
	return schedule.Page{Data: []schedule.Schedule{fake.item}}, nil
}

func (fake *fakeScheduleAPI) Get(context.Context, uuid.UUID, uuid.UUID) (schedule.Schedule, error) {
	return fake.item, nil
}

func (fake *fakeScheduleAPI) Update(context.Context, []byte, string, uuid.UUID, uuid.UUID, schedule.Input, int) (schedule.Schedule, error) {
	fake.updateCall++
	return fake.item, fake.updateErr
}

func (fake *fakeScheduleAPI) Activate(context.Context, []byte, string, uuid.UUID, uuid.UUID) (schedule.Schedule, error) {
	return fake.item, fake.activateErr
}

func (fake *fakeScheduleAPI) Pause(context.Context, []byte, string, uuid.UUID, uuid.UUID) (schedule.Schedule, error) {
	return fake.item, nil
}

func (fake *fakeScheduleAPI) Archive(context.Context, []byte, string, uuid.UUID, uuid.UUID, int) (schedule.Schedule, error) {
	fake.archiveCall++
	return fake.item, nil
}

func (fake *fakeScheduleAPI) Restore(context.Context, []byte, string, uuid.UUID, uuid.UUID, int) (schedule.Schedule, error) {
	fake.restoreCall++
	return fake.item, nil
}

func scheduleJSON(version bool) string {
	suffix := ""
	if version {
		suffix = `,"version":1`
	}
	return `{
		"name":"Morning","daysOfWeek":[1,3,5],"localTime":"09:30","timezone":"Asia/Bangkok",
		"periodPreset":"YESTERDAY","reportKeys":["sales_goods_services"],
		"recipientIds":["` + uuid.NewString() + `"]` + suffix + `}`
}

func TestAdminCreatesScheduleWithMutationGuards(t *testing.T) {
	tenantID, scheduleID := uuid.New(), uuid.New()
	api := &fakeScheduleAPI{item: schedule.Schedule{ID: scheduleID, TenantID: tenantID, Input: schedule.Input{Name: "Morning", PeriodPreset: report.Yesterday}, Status: schedule.StatusDraft}}
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Schedules: api,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID.String()+"/schedules", strings.NewReader(scheduleJSON(false)))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	request.Header.Set("Idempotency-Key", "schedule-create-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || api.createCall != 1 || !strings.Contains(response.Body.String(), scheduleID.String()) {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.createCall, response.Body.String())
	}
}

func TestAdminSchedulePatchAcceptsVersionedFullInput(t *testing.T) {
	tenantID, scheduleID := uuid.New(), uuid.New()
	api := &fakeScheduleAPI{item: schedule.Schedule{ID: scheduleID, TenantID: tenantID, Version: 2}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Schedules: api})
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/tenants/"+tenantID.String()+"/schedules/"+scheduleID.String(), strings.NewReader(scheduleJSON(true)))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || api.updateCall != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.updateCall, response.Body.String())
	}
}

func TestAdminListsArchivesAndSupportsVersionedArchiveRestore(t *testing.T) {
	tenantID, scheduleID := uuid.New(), uuid.New()
	api := &fakeScheduleAPI{item: schedule.Schedule{ID: scheduleID, TenantID: tenantID, Version: 3}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Schedules: api})

	list := httptest.NewRequest(http.MethodGet, "/api/v1/admin/tenants/"+tenantID.String()+"/schedules?includeArchived=true&status=ACTIVE&search=morning", nil)
	list.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK || !api.includeArchived || api.listFilter.Status == nil || *api.listFilter.Status != schedule.StatusActive || api.listFilter.Search != "morning" {
		t.Fatalf("list status=%d filter=%+v body=%s", listResponse.Code, api.listFilter, listResponse.Body.String())
	}

	for _, operation := range []struct {
		method string
		path   string
		calls  *int
	}{
		{http.MethodDelete, "/api/v1/admin/tenants/" + tenantID.String() + "/schedules/" + scheduleID.String() + "?version=3", &api.archiveCall},
		{http.MethodPost, "/api/v1/admin/tenants/" + tenantID.String() + "/schedules/" + scheduleID.String() + "/restore?version=3", &api.restoreCall},
	} {
		request := httptest.NewRequest(operation.method, operation.path, nil)
		request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
		request.Header.Set("X-CSRF-Token", "admin-csrf")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || *operation.calls != 1 {
			t.Fatalf("%s status=%d calls=%d body=%s", operation.path, response.Code, *operation.calls, response.Body.String())
		}
	}
}

func TestAdminScheduleActivationReturnsActionableBlockers(t *testing.T) {
	tenantID, scheduleID := uuid.New(), uuid.New()
	api := &fakeScheduleAPI{activateErr: &schedule.ReadinessError{Blockers: []string{schedule.BlockerSMLNotReady, schedule.BlockerLineNotConfigured}}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Schedules: api})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID.String()+"/schedules/"+scheduleID.String()+"/activate", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(body, schedule.BlockerSMLNotReady) || !strings.Contains(body, schedule.BlockerLineNotConfigured) {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
}

func TestAdminRendersSchedulePreviewWithCSRFGuard(t *testing.T) {
	tenantID := uuid.New()
	previewAPI := &fakeSchedulePreviewAPI{item: line.FlexPreview{TenantName: "ร้านตัวอย่าง", PayloadBytes: 1024}}
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{},
		Schedules: &fakeScheduleAPI{}, FlexPreviews: previewAPI,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID.String()+"/schedules/preview", strings.NewReader(`{
		"periodPreset":"YESTERDAY","reportKeys":["sales_goods_services"]
	}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || previewAPI.calls != 1 || !strings.Contains(response.Body.String(), "ร้านตัวอย่าง") || !strings.Contains(response.Body.String(), `"dateFrom"`) || strings.Contains(response.Body.String(), `"DateFrom"`) {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, previewAPI.calls, response.Body.String())
	}
}

func TestAdminSchedulePreviewReturnsFieldValidation(t *testing.T) {
	tenantID := uuid.New()
	previewAPI := &fakeSchedulePreviewAPI{err: &line.FlexPreviewValidationError{Field: "reportKeys", Code: "INVALID_REPORTS"}}
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{},
		Schedules: &fakeScheduleAPI{}, FlexPreviews: previewAPI,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID.String()+"/schedules/preview", strings.NewReader(`{
		"periodPreset":"YESTERDAY","reportKeys":[]
	}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "INVALID_REPORTS") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminExplicitlyEnqueuesScheduleTestSend(t *testing.T) {
	tenantID, scheduleID, executionID := uuid.New(), uuid.New(), uuid.New()
	testAPI := &fakeScheduleTestSendAPI{execution: schedule.Execution{ID: executionID, TenantID: tenantID, ScheduleID: scheduleID, Status: schedule.ExecutionCollecting}}
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{},
		Schedules: &fakeScheduleAPI{}, ScheduleTests: testAPI,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants/"+tenantID.String()+"/schedules/"+scheduleID.String()+"/test-send", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("X-CSRF-Token", "admin-csrf")
	request.Header.Set("Idempotency-Key", "schedule-test-send-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || testAPI.calls != 1 || !strings.Contains(response.Body.String(), executionID.String()) {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, testAPI.calls, response.Body.String())
	}
}
