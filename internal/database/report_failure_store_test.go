package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPreRequestFailuresOpenSharedHostCircuitAndRemoteUnknownIsAtomic(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	defer func() {
		_, _ = pool.Exec(context.Background(), `delete from tenants where id = $1`, tenantID)
	}()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at)
		values ($1, $2, 'Host circuit', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "host-circuit-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_sml_connections (
		  tenant_id, endpoint_url, database_name, config_file_name,
		  username_ciphertext, username_nonce, password_ciphertext, password_nonce,
		  encryption_key_id, readiness_status, last_tested_at
		) values ($1, 'https://shared-sml.example.com:8443/service', 'DATA', 'SMLConfigDATA.xml',
		          decode('11','hex'), decode('12','hex'), decode('13','hex'), decode('14','hex'),
		          'key', 'READY', $2)`, tenantID, now); err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status,
		  period_preset, period_from, period_to, claimed_by, claimed_at,
		  lease_expires_at, queued_at, expires_at, result_kind, priority
		) values ($1, $2, 'cash_bank_receipts', 'DASHBOARD', $3, 'RUNNING',
		          'YESTERDAY', '2026-07-14', '2026-07-14', 'worker-a', $4,
		          $5, $4, $6, 'SUMMARY', 85)`, runID, tenantID, "host-failure-"+runID.String(), now, now.Add(time.Minute), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	store := NewReportStore(pool)
	if err := store.RetryPreRequestFailure(ctx, runID, "worker-a", "SML_UNREACHABLE", now.Add(30*time.Second), now); err != nil {
		t.Fatalf("RetryPreRequestFailure() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `
		update report_runs set status = 'RUNNING', claimed_by = 'worker-a', claimed_at = $2,
		lease_expires_at = $3, attempt = 2 where id = $1`, runID, now.Add(30*time.Second), now.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	preRequestEvidence := failure.Complete(failure.Evidence{
		Version: 1, Level: failure.LevelConfirmed, Category: failure.CategoryJavaWSConnectivity,
		Stage: failure.StageConnectJavaWS, TransportPhase: failure.PhaseBeforeRequestSent,
		OccurredAt: now.Add(31 * time.Second), Retryable: true, SafeErrorCode: "SML_UNREACHABLE",
	})
	if err := store.FailPreRequestFailure(ctx, runID, "worker-a", preRequestEvidence, now.Add(31*time.Second)); err != nil {
		t.Fatalf("FailPreRequestFailure() error = %v", err)
	}
	var failures int
	var hostOpen bool
	if err := pool.QueryRow(ctx, `
		select circuit.consecutive_failures, circuit.open_until > $2
		from sml_host_circuits circuit
		join tenant_sml_connections connection on connection.endpoint_host_key = circuit.host_key
		where connection.tenant_id = $1`, tenantID, now.Add(31*time.Second)).Scan(&failures, &hostOpen); err != nil {
		t.Fatal(err)
	}
	if failures != 2 || !hostOpen {
		t.Fatalf("host failures=%d open=%v", failures, hostOpen)
	}

	remoteRunID := uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status,
		  period_preset, period_from, period_to, claimed_by, claimed_at,
		  lease_expires_at, queued_at, expires_at, result_kind, priority, data_source_version
		) values ($1, $2, 'stock_balance', 'BACKGROUND', $3, 'RUNNING',
		          'AS_OF_RUN', '2026-07-15', '2026-07-15', 'worker-a', $4,
		          $5, $4, $6, 'SUMMARY', 20, 1)`, remoteRunID, tenantID, "remote-unknown-"+remoteRunID.String(), now.Add(time.Minute), now.Add(2*time.Minute), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	failedAt := now.Add(61 * time.Second)
	remoteEvidence := failure.Complete(failure.Evidence{
		Version: 1, Level: failure.LevelConfirmed, Category: failure.CategoryJavaWSConnectivity,
		Stage: failure.StageWaitResponse, TransportPhase: failure.PhaseRequestSentResultUnknown,
		OccurredAt: failedAt, Retryable: false, RemoteStateUnknown: true, SafeErrorCode: "SML_TIMEOUT",
	})
	if err := store.FailRemoteUnknown(ctx, remoteRunID, "worker-a", remoteEvidence, failedAt, failedAt.Add(10*time.Minute)); err != nil {
		t.Fatalf("FailRemoteUnknown() error = %v", err)
	}
	var status, code, stage, phase string
	var tenantOpen bool
	if err := pool.QueryRow(ctx, `
		select run.status, run.safe_error_code, run.failure_stage, run.failure_transport_phase,
		       circuit.open_until > $3
		from report_runs run join tenant_sml_circuits circuit on circuit.tenant_id = run.tenant_id
		where run.id = $1 and run.tenant_id = $2`, remoteRunID, tenantID, failedAt).Scan(&status, &code, &stage, &phase, &tenantOpen); err != nil {
		t.Fatal(err)
	}
	if status != string(report.StatusFailed) || code != "SML_TIMEOUT" || stage != string(failure.StageWaitResponse) || phase != string(failure.PhaseRequestSentResultUnknown) || !tenantOpen {
		t.Fatalf("remote run=%s/%s stage=%s phase=%s tenantOpen=%v", status, code, stage, phase, tenantOpen)
	}
	detail, err := NewOperationsStore(pool).GetReportRunDetail(ctx, remoteRunID, failedAt)
	if err != nil || detail.Run.FailureEvidence == nil || detail.Run.FailureEvidence.TransportPhase != failure.PhaseRequestSentResultUnknown || detail.Impact.ReportsTotal != 1 || detail.Impact.ReportsFailed != 1 || detail.Impact.Notification != failure.NotificationNotApplicable {
		t.Fatalf("GetReportRunDetail() = %+v, %v", detail, err)
	}
	if _, err := pool.Exec(ctx, `update tenant_sml_connections set version = version + 1 where tenant_id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	detail, err = NewOperationsStore(pool).GetReportRunDetail(ctx, remoteRunID, failedAt)
	if err != nil || !detail.ConnectionChangedSinceFailure {
		t.Fatalf("connection change detail = %+v, %v", detail, err)
	}
}
