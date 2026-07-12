package sml

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/google/uuid"
)

type memoryConnectionStore struct {
	stored     StoredConnection
	putCount   int
	lastStatus Readiness
	lastCode   string
}

func (store *memoryConnectionStore) Get(context.Context, uuid.UUID) (StoredConnection, error) {
	if store.stored.TenantID == uuid.Nil {
		return StoredConnection{}, ErrConnectionNotConfigured
	}
	return store.stored, nil
}

func (store *memoryConnectionStore) Put(_ context.Context, _ []byte, _ string, connection StoredConnection, expectedVersion int, now time.Time) (StoredConnection, error) {
	if store.stored.TenantID != uuid.Nil && expectedVersion != store.stored.Version {
		return StoredConnection{}, ErrConnectionVersionConflict
	}
	connection.Version = store.stored.Version + 1
	connection.Readiness = ReadinessUntested
	connection.UpdatedAt = now
	store.stored = connection
	store.putCount++
	return connection, nil
}

func (store *memoryConnectionStore) MarkTested(_ context.Context, _ []byte, _ string, tenantID uuid.UUID, status Readiness, safeCode string, testedAt time.Time) error {
	store.lastStatus = status
	store.lastCode = safeCode
	store.stored.Readiness = status
	store.stored.LastSafeErrorCode = safeCode
	store.stored.LastTestedAt = &testedAt
	return nil
}

type queryFunc func(context.Context, Connection, string) ([]map[string]string, error)

func (query queryFunc) Query(ctx context.Context, connection Connection, sql string) ([]map[string]string, error) {
	return query(ctx, connection, sql)
}

func TestConnectionServiceEncryptsSecretsAndReturnsOnlyRedactedStatus(t *testing.T) {
	tenantID := uuid.MustParse("4a06e1c2-29cd-4b5a-81d4-b2a26c2e11ec")
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-2026-01", bytes.NewReader(append(bytes.Repeat([]byte{2}, 12), bytes.Repeat([]byte{3}, 12)...)))
	store := &memoryConnectionStore{}
	service := NewConnectionService(
		store,
		box,
		EndpointPolicy{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
		queryFunc(func(context.Context, Connection, string) ([]map[string]string, error) { return nil, nil }),
		func() time.Time { return now },
	)

	status, err := service.Replace(context.Background(), []byte("admin-hash"), "request-1", tenantID, ConnectionInput{
		EndpointURL:    "http://10.0.0.8:8080/SMLJavaWebService/DotNetFrameWork",
		ConfigFileName: "SMLConfigDATA.xml",
		DatabaseName:   "sml1_2026",
		Username:       "sml-user",
		Password:       "sml-password",
	})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if status.EndpointURL != "http://10.0.0.8:8080" || status.EndpointHost != "10.0.0.8:8080" || status.DatabaseName != "sml1_2026" || status.Version != 1 {
		t.Fatalf("status = %+v", status)
	}
	serialized := string(store.stored.Username.Ciphertext) + string(store.stored.Password.Ciphertext)
	if strings.Contains(serialized, "sml-user") || strings.Contains(serialized, "sml-password") {
		t.Fatal("stored ciphertext contains plaintext credential")
	}
	if got := strings.Join([]string{status.EndpointHost, status.DatabaseName, status.LastSafeErrorCode}, "|"); strings.Contains(got, "sml-user") || strings.Contains(got, "sml-password") {
		t.Fatal("redacted status exposed credential")
	}
}

func TestConnectionStatusPreservesCustomEndpointPathForSafeEditing(t *testing.T) {
	status := redactedStatus(StoredConnection{EndpointURL: "https://sml.example.com/custom/service", Version: 3})
	if status.EndpointURL != "https://sml.example.com/custom/service" || status.EndpointHost != "sml.example.com" {
		t.Fatalf("status = %+v", status)
	}
}

func TestConnectionServiceTestUsesFixedQueryAndStoresSafeFailure(t *testing.T) {
	tenantID := uuid.MustParse("4a06e1c2-29cd-4b5a-81d4-b2a26c2e11ec")
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-2026-01", bytes.NewReader(bytes.Repeat([]byte{2}, 24)))
	username, _ := box.Encrypt([]byte("sml-user"), []byte(tenantID.String()+":username"))
	password, _ := box.Encrypt([]byte("sml-password"), []byte(tenantID.String()+":password"))
	store := &memoryConnectionStore{stored: StoredConnection{
		TenantID: tenantID, EndpointURL: "http://10.0.0.8/service", ConfigFileName: "SMLConfigDATA.xml", DatabaseName: "demo",
		Username: username, Password: password, Version: 1, Readiness: ReadinessUntested,
	}}
	queriedSQL := ""
	service := NewConnectionService(store, box, EndpointPolicy{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}, queryFunc(func(_ context.Context, connection Connection, sql string) ([]map[string]string, error) {
		queriedSQL = sql
		if connection.Username != "sml-user" || connection.Password != "sml-password" {
			t.Fatalf("decrypted connection = %+v", connection)
		}
		return nil, &SafeError{Code: "SML_TIMEOUT", Retryable: true}
	}), func() time.Time { return now })

	_, err := service.Test(context.Background(), []byte("admin-hash"), "request-2", tenantID)
	var testError *ConnectionTestError
	if !errors.As(err, &testError) || testError.SafeCode != "SML_TIMEOUT" {
		t.Fatalf("Test() error = %v", err)
	}
	if queriedSQL != "select 1 as ok" || store.lastStatus != ReadinessFailed || store.lastCode != "SML_TIMEOUT" {
		t.Fatalf("query/status = %q %s %s", queriedSQL, store.lastStatus, store.lastCode)
	}
}

func TestConnectionServiceAllowsNoAuthentication(t *testing.T) {
	store := &memoryConnectionStore{}
	box, err := secret.NewBox(bytes.Repeat([]byte{9}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{8}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	service := NewConnectionService(
		store,
		box,
		EndpointPolicy{
			AllowedHosts: []string{"sml-shop.example.com"},
			LookupNetIP: func(context.Context, string, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("58.136.190.202")}, nil
			},
		},
		nil,
		time.Now,
	)
	status, err := service.Replace(context.Background(), []byte("actor"), "request", uuid.New(), ConnectionInput{
		EndpointURL: "http://sml-shop.example.com:8092", ConfigFileName: "SMLConfigDATA.xml", DatabaseName: "DEMO_DATA",
	})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if !status.IsConfigured || store.stored.EndpointURL != "http://sml-shop.example.com:8092/SMLJavaWebService/DotNetFrameWork" {
		t.Fatalf("stored connection = %+v", store.stored)
	}
}

func TestConnectionServiceRejectsPartialBasicAuthentication(t *testing.T) {
	store := &memoryConnectionStore{}
	box, _ := secret.NewBox(bytes.Repeat([]byte{9}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{8}, 128)))
	service := NewConnectionService(
		store,
		box,
		EndpointPolicy{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
		nil,
		time.Now,
	)
	_, err := service.Replace(context.Background(), []byte("actor"), "request", uuid.New(), ConnectionInput{
		EndpointURL: "http://10.0.0.8", ConfigFileName: "SMLConfigDATA.xml", DatabaseName: "DEMO_DATA", Username: "user",
	})
	if err == nil {
		t.Fatal("expected a username without a password to be rejected")
	}
}

func TestConnectionServiceRejectsBasicAuthenticationOverPublicHTTP(t *testing.T) {
	store := &memoryConnectionStore{}
	box, _ := secret.NewBox(bytes.Repeat([]byte{9}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{8}, 128)))
	service := NewConnectionService(
		store,
		box,
		EndpointPolicy{
			AllowedHosts: []string{"sml-shop.example.com"},
			LookupNetIP: func(context.Context, string, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("58.136.190.202")}, nil
			},
		},
		nil,
		time.Now,
	)
	_, err := service.Replace(context.Background(), []byte("actor"), "request", uuid.New(), ConnectionInput{
		EndpointURL: "http://sml-shop.example.com:8092", ConfigFileName: "SMLConfigDATA.xml", DatabaseName: "DEMO_DATA",
		Username: "user", Password: "password",
	})
	if err == nil {
		t.Fatal("expected Basic Auth over public HTTP to be rejected")
	}
}
