package httpapi

import (
	"net/http"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/go-chi/chi/v5"
)

const (
	maximumScheduleReports  = 10
	maximumFlexPayloadBytes = 30 * 1024
)

type adminReportDefinition struct {
	ReportKey     report.Key    `json:"reportKey"`
	Version       string        `json:"version"`
	Label         string        `json:"label"`
	Category      string        `json:"category"`
	CategoryLabel string        `json:"categoryLabel"`
	Status        report.Status `json:"status"`
}

func registerAdminReportRoutes(router chi.Router, adminAuth AdminAuthenticator) {
	router.Get("/api/v1/admin/reports", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		definitions := report.Definitions()
		items := make([]adminReportDefinition, 0, len(definitions))
		for _, definition := range definitions {
			items = append(items, adminReportDefinition{
				ReportKey: definition.Key, Version: definition.Version, Label: definition.LabelTH,
				Category: definition.Category, CategoryLabel: definition.CategoryLabelTH, Status: definition.Status,
			})
		}
		response.Header().Set("Cache-Control", "no-store")
		writeJSON(response, http.StatusOK, map[string]any{
			"data":   items,
			"limits": map[string]int{"maxScheduleReports": maximumScheduleReports, "maxFlexPayloadBytes": maximumFlexPayloadBytes},
		})
	})
}
