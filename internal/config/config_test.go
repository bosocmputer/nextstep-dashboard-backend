package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadRequiresProductionConfiguration(t *testing.T) {
	_, err := Load(func(key string) (string, bool) {
		values := map[string]string{"APP_ENV": "production"}
		value, ok := values[key]
		return value, ok
	})

	if err == nil {
		t.Fatal("expected missing production configuration to fail")
	}
	for _, name := range []string{
		"DATABASE_URL",
		"PUBLIC_BASE_URL",
		"ADMIN_PASSWORD_HASH",
		"SESSION_HMAC_KEY",
		"ENCRYPTION_MASTER_KEY",
		"ENCRYPTION_KEY_ID",
		"SML_ALLOWED_CIDRS",
		"LINE_LOGIN_CHANNEL_ID",
		"LINE_MESSAGING_CHANNEL_ACCESS_TOKEN",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func TestLoadAcceptsSafeProductionConfiguration(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	values := map[string]string{
		"APP_ENV":                             "production",
		"HTTP_ADDR":                           ":8080",
		"DATABASE_URL":                        "postgres://nextstep@example.internal/nextstep?sslmode=verify-full",
		"PUBLIC_BASE_URL":                     "https://dashboard.nextstep-soft.com",
		"ADMIN_USERNAME":                      "superadmin",
		"ADMIN_PASSWORD_HASH":                 "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"SESSION_HMAC_KEY":                    secret,
		"ENCRYPTION_MASTER_KEY":               secret,
		"ENCRYPTION_KEY_ID":                   "key-2026-01",
		"SML_ALLOWED_CIDRS":                   "10.0.0.0/8,192.168.0.0/16",
		"SML_ALLOWED_HOSTS":                   "sml-shop.example.com",
		"SML_ALLOW_PUBLIC_ENDPOINTS":          "true",
		"SML_ALLOWED_PORTS":                   "80,443,8080,8092",
		"LINE_LOGIN_CHANNEL_ID":               "2010662588",
		"LINE_MESSAGING_CHANNEL_ACCESS_TOKEN": strings.Repeat("x", 64),
		"DATABASE_MAX_CONNECTIONS":            "24",
		"DATABASE_MIN_CONNECTIONS":            "3",
		"SNAPSHOT_FIRST_ENABLED":              "true",
		"SNAPSHOT_FIRST_TENANT_IDS":           "a904bc92-a89b-463b-bc2a-565f09cbef44",
		"SMART_SCHEDULE_PERIODS_ENABLED":      "true",
		"SMART_SCHEDULE_PERIOD_TENANT_IDS":    "a904bc92-a89b-463b-bc2a-565f09cbef44",
		"SUMMARY_QUERY_ENABLED":               "true",
		"GENERATION_CACHE_ENABLED":            "true",
		"STALE_REVALIDATION_ENABLED":          "true",
		"HEAVY_CHUNK_ENABLED":                 "true",
		"HEAVY_CHUNK_TENANT_REPORTS":          "11111111-1111-1111-1111-111111111111/stock_balance",
		"SCHEDULE_CHUNK_ENABLED":              "false",
	}

	cfg, err := Load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PublicBaseURL.String() != values["PUBLIC_BASE_URL"] {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL.String())
	}
	if len(cfg.EncryptionMasterKey) != 32 {
		t.Fatalf("EncryptionMasterKey length = %d", len(cfg.EncryptionMasterKey))
	}
	if cfg.ReportWorkerConcurrency != 4 {
		t.Fatalf("ReportWorkerConcurrency = %d", cfg.ReportWorkerConcurrency)
	}
	if cfg.BackupPolicy != "PRE_MIGRATION_ONLY" {
		t.Fatalf("BackupPolicy = %q", cfg.BackupPolicy)
	}
	if cfg.DeliveryWorkerConcurrency != 4 {
		t.Fatalf("DeliveryWorkerConcurrency = %d", cfg.DeliveryWorkerConcurrency)
	}
	if cfg.DatabaseMaxConnections != 24 || cfg.DatabaseMinConnections != 3 {
		t.Fatalf("database pool = %d/%d, want 3/24", cfg.DatabaseMinConnections, cfg.DatabaseMaxConnections)
	}
	if cfg.LineLoginChannelID != values["LINE_LOGIN_CHANNEL_ID"] {
		t.Fatalf("LineLoginChannelID = %q", cfg.LineLoginChannelID)
	}
	if cfg.LineMessagingAccessToken != values["LINE_MESSAGING_CHANNEL_ACCESS_TOKEN"] {
		t.Fatal("LineMessagingAccessToken was not loaded")
	}
	if len(cfg.SMLAllowedHosts) != 1 || cfg.SMLAllowedHosts[0] != "sml-shop.example.com" {
		t.Fatalf("SMLAllowedHosts = %#v", cfg.SMLAllowedHosts)
	}
	if !cfg.SMLAllowPublicEndpoints {
		t.Fatal("SMLAllowPublicEndpoints was not loaded")
	}
	if got := cfg.SMLAllowedPorts; len(got) != 4 || got[0] != 80 || got[1] != 443 || got[2] != 8080 || got[3] != 8092 {
		t.Fatalf("SMLAllowedPorts = %#v", got)
	}
	if !cfg.SnapshotFirstEnabled || len(cfg.SnapshotFirstTenantIDs) != 1 {
		t.Fatalf("snapshot first config = enabled:%v tenants:%v", cfg.SnapshotFirstEnabled, cfg.SnapshotFirstTenantIDs)
	}
	if !cfg.SmartSchedulePeriodsEnabled || len(cfg.SmartSchedulePeriodTenantIDs) != 1 {
		t.Fatalf("smart schedule config = enabled:%v tenants:%v", cfg.SmartSchedulePeriodsEnabled, cfg.SmartSchedulePeriodTenantIDs)
	}
	if !cfg.SummaryQueryEnabled || !cfg.GenerationCacheEnabled || !cfg.StaleRevalidationEnabled || !cfg.HeavyChunkEnabled || len(cfg.HeavyChunkTenantReports) != 1 || cfg.ScheduleChunkEnabled {
		t.Fatalf("dashboard feature flags were not loaded: %+v", cfg)
	}
}

func TestLoadEnablesBoundedSummaryQueriesByDefault(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	values := map[string]string{
		"DATABASE_URL":          "postgres://nextstep@localhost/nextstep?sslmode=disable",
		"PUBLIC_BASE_URL":       "http://localhost:6324",
		"ADMIN_PASSWORD_HASH":   "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"SESSION_HMAC_KEY":      secret,
		"ENCRYPTION_MASTER_KEY": secret,
		"ENCRYPTION_KEY_ID":     "key-2026-01",
		"SML_ALLOWED_CIDRS":     "10.0.0.0/8",
	}

	cfg, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.SummaryQueryEnabled {
		t.Fatal("SummaryQueryEnabled = false, want safe bounded summaries enabled for every tenant by default")
	}

	values["SUMMARY_QUERY_ENABLED"] = "false"
	cfg, err = Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatalf("Load() with emergency kill switch error = %v", err)
	}
	if cfg.SummaryQueryEnabled {
		t.Fatal("SummaryQueryEnabled = true after explicit false, want emergency kill switch to remain available")
	}
}

func TestParseAllowedPortsAllowsWildcardForAnyPublicEndpointPort(t *testing.T) {
	ports, err := parseAllowedPorts("*")
	if err != nil {
		t.Fatalf("parseAllowedPorts(*) error = %v", err)
	}
	if len(ports) != 0 {
		t.Fatalf("parseAllowedPorts(*) = %#v, want no port restriction", ports)
	}
}

func TestParseAllowedPortsRejectsWildcardMixedWithExplicitPorts(t *testing.T) {
	if _, err := parseAllowedPorts("*,8080"); err == nil {
		t.Fatal("parseAllowedPorts(*,8080) accepted an ambiguous wildcard configuration")
	}
}

func TestLoadRejectsDatabaseMinimumAboveMaximum(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	values := map[string]string{
		"DATABASE_URL":             "postgres://nextstep@localhost/nextstep?sslmode=disable",
		"PUBLIC_BASE_URL":          "http://localhost:6324",
		"ADMIN_PASSWORD_HASH":      "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"SESSION_HMAC_KEY":         secret,
		"ENCRYPTION_MASTER_KEY":    secret,
		"ENCRYPTION_KEY_ID":        "key-2026-01",
		"SML_ALLOWED_CIDRS":        "10.0.0.0/8",
		"DATABASE_MAX_CONNECTIONS": "4",
		"DATABASE_MIN_CONNECTIONS": "5",
	}

	_, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err == nil || !strings.Contains(err.Error(), "DATABASE_MIN_CONNECTIONS") {
		t.Fatalf("Load() error = %v, want minimum/maximum validation", err)
	}
}

func TestLoadRejectsUnsafeFeatureFlagCombinations(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	base := map[string]string{
		"DATABASE_URL":          "postgres://nextstep@localhost/nextstep?sslmode=disable",
		"PUBLIC_BASE_URL":       "http://localhost:6324",
		"ADMIN_PASSWORD_HASH":   "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"SESSION_HMAC_KEY":      secret,
		"ENCRYPTION_MASTER_KEY": secret,
		"ENCRYPTION_KEY_ID":     "key-2026-01",
		"SML_ALLOWED_CIDRS":     "10.0.0.0/8",
	}
	tests := []struct {
		name    string
		flags   map[string]string
		message string
	}{
		{name: "generation without summary", flags: map[string]string{"SUMMARY_QUERY_ENABLED": "false", "GENERATION_CACHE_ENABLED": "true"}, message: "GENERATION_CACHE_ENABLED requires SUMMARY_QUERY_ENABLED"},
		{name: "revalidation without generation", flags: map[string]string{"SUMMARY_QUERY_ENABLED": "true", "STALE_REVALIDATION_ENABLED": "true"}, message: "STALE_REVALIDATION_ENABLED requires GENERATION_CACHE_ENABLED"},
		{name: "schedule chunk without heavy chunk", flags: map[string]string{"SCHEDULE_CHUNK_ENABLED": "true"}, message: "SCHEDULE_CHUNK_ENABLED requires HEAVY_CHUNK_ENABLED"},
		{name: "heavy chunk without target", flags: map[string]string{"HEAVY_CHUNK_ENABLED": "true"}, message: "HEAVY_CHUNK_ENABLED requires at least one HEAVY_CHUNK_TENANT_REPORTS entry"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := make(map[string]string, len(base)+len(test.flags))
			for key, value := range base {
				values[key] = value
			}
			for key, value := range test.flags {
				values[key] = value
			}
			_, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Load() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestLoadRejectsUnsafeProductionValuesWithoutEchoingSecrets(t *testing.T) {
	weak := base64.StdEncoding.EncodeToString([]byte("too-short"))
	values := map[string]string{
		"APP_ENV":                             "production",
		"DATABASE_URL":                        "postgres://user:top-secret@localhost/db?sslmode=disable",
		"PUBLIC_BASE_URL":                     "http://dashboard.nextstep-soft.com",
		"ADMIN_PASSWORD_HASH":                 "top-secret-password",
		"SESSION_HMAC_KEY":                    weak,
		"ENCRYPTION_MASTER_KEY":               weak,
		"ENCRYPTION_KEY_ID":                   "key-2026-01",
		"SML_ALLOWED_CIDRS":                   "0.0.0.0/0",
		"LINE_LOGIN_CHANNEL_ID":               "not-a-channel-id",
		"LINE_MESSAGING_CHANNEL_ACCESS_TOKEN": "short-secret",
	}

	_, err := Load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil {
		t.Fatal("expected unsafe configuration to fail")
	}
	for _, secret := range []string{"top-secret", "top-secret-password", weak} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("configuration error leaked secret %q: %v", secret, err)
		}
	}
}
