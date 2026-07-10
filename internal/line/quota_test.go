package line

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuotaClientFetchesSharedOAUsageWithoutExposingToken(t *testing.T) {
	token := testAccessToken()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization header is invalid")
		}
		switch request.URL.Path {
		case "/quota":
			_, _ = response.Write([]byte(`{"type":"limited","value":5000,"futureField":true}`))
		case "/consumption":
			_, _ = response.Write([]byte(`{"totalUsage":4200}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := NewQuotaClient(token, server.URL+"/quota", server.URL+"/consumption", time.Second)

	usage, err := client.Fetch(context.Background())
	if err != nil || usage.Limit == nil || *usage.Limit != 5000 || usage.Consumed != 4200 {
		t.Fatalf("Fetch() = %+v, %v", usage, err)
	}
}

func TestQuotaClientSupportsUnlimitedPlansAndRejectsUnsafeResponses(t *testing.T) {
	for _, test := range []struct {
		name        string
		quotaBody   string
		usageBody   string
		status      int
		wantErr     bool
		wantNoLimit bool
	}{
		{name: "unlimited", quotaBody: `{"type":"none"}`, usageBody: `{"totalUsage":12}`, status: http.StatusOK, wantNoLimit: true},
		{name: "unknown quota type", quotaBody: `{"type":"future"}`, usageBody: `{"totalUsage":12}`, status: http.StatusOK, wantErr: true},
		{name: "negative usage", quotaBody: `{"type":"limited","value":100}`, usageBody: `{"totalUsage":-1}`, status: http.StatusOK, wantErr: true},
		{name: "provider failure", quotaBody: `{}`, usageBody: `{}`, status: http.StatusUnauthorized, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.WriteHeader(test.status)
				if request.URL.Path == "/quota" {
					_, _ = response.Write([]byte(test.quotaBody))
				} else {
					_, _ = response.Write([]byte(test.usageBody))
				}
			}))
			defer server.Close()
			client := NewQuotaClient(testAccessToken(), server.URL+"/quota", server.URL+"/consumption", time.Second)

			usage, err := client.Fetch(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("Fetch() = %+v, %v", usage, err)
			}
			if !test.wantErr && test.wantNoLimit && usage.Limit != nil {
				t.Fatalf("Limit = %v, want nil", usage.Limit)
			}
		})
	}
}
