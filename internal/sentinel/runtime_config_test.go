package sentinel

import "testing"

func TestRuntimeConfigDefaultsToPreMigrationBackupPolicy(t *testing.T) {
	values := map[string]string{"DATABASE_URL": "postgres://example.test/nextstep", "PUBLIC_BASE_URL": "https://dashboard.example.test", "OPERATIONAL_ALERTS_MODE": "off"}
	config, err := LoadRuntimeConfig(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if config.BackupPolicy != BackupPolicyPreMigrationOnly {
		t.Fatalf("BackupPolicy = %q", config.BackupPolicy)
	}
	if config.TelegramTenantContextMode != TelegramTenantContextOff {
		t.Fatalf("TelegramTenantContextMode = %q", config.TelegramTenantContextMode)
	}
}

func TestRuntimeConfigRejectsUnknownBackupPolicy(t *testing.T) {
	values := map[string]string{"DATABASE_URL": "postgres://example.test/nextstep", "PUBLIC_BASE_URL": "https://dashboard.example.test", "OPERATIONAL_ALERTS_MODE": "off", "BACKUP_POLICY": "DISABLED"}
	if _, err := LoadRuntimeConfig(func(name string) (string, bool) { value, ok := values[name]; return value, ok }); err == nil {
		t.Fatal("unknown backup policy was accepted")
	}
}

func TestRuntimeConfigAcceptsOnlyKnownTelegramTenantContextModes(t *testing.T) {
	for raw, expected := range map[string]TelegramTenantContextMode{
		"off":          TelegramTenantContextOff,
		"private_chat": TelegramTenantContextPrivateChat,
	} {
		values := map[string]string{
			"DATABASE_URL": "postgres://example.test/nextstep", "PUBLIC_BASE_URL": "https://dashboard.example.test",
			"OPERATIONAL_ALERTS_MODE": "off", "TELEGRAM_TENANT_CONTEXT_MODE": raw,
		}
		config, err := LoadRuntimeConfig(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
		if err != nil || config.TelegramTenantContextMode != expected {
			t.Fatalf("mode %q: config=%+v err=%v", raw, config, err)
		}
	}
	values := map[string]string{
		"DATABASE_URL": "postgres://example.test/nextstep", "PUBLIC_BASE_URL": "https://dashboard.example.test",
		"OPERATIONAL_ALERTS_MODE": "off", "TELEGRAM_TENANT_CONTEXT_MODE": "group",
	}
	if _, err := LoadRuntimeConfig(func(name string) (string, bool) { value, ok := values[name]; return value, ok }); err == nil {
		t.Fatal("unknown Telegram tenant context mode was accepted")
	}
}
