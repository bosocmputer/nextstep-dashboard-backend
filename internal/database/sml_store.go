package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SMLConnectionStore struct {
	pool *pgxpool.Pool
}

func NewSMLConnectionStore(pool *pgxpool.Pool) *SMLConnectionStore {
	return &SMLConnectionStore{pool: pool}
}

func (store *SMLConnectionStore) Get(ctx context.Context, tenantID uuid.UUID) (sml.StoredConnection, error) {
	row := store.pool.QueryRow(ctx, `
		select tenant_id, endpoint_url, config_file_name, database_name,
		       username_ciphertext, username_nonce, password_ciphertext, password_nonce,
		       encryption_key_id, version, readiness_status, last_tested_at,
		       coalesce(last_safe_error_code, ''), created_at, updated_at
		from tenant_sml_connections
		where tenant_id = $1`, tenantID)
	connection, err := scanSMLConnection(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return sml.StoredConnection{}, sml.ErrConnectionNotConfigured
	}
	if err != nil {
		return sml.StoredConnection{}, fmt.Errorf("get SML connection: %w", err)
	}
	return connection, nil
}

func (store *SMLConnectionStore) Put(ctx context.Context, actorHash []byte, requestID string, connection sml.StoredConnection, expectedVersion int, now time.Time) (sml.StoredConnection, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return sml.StoredConnection{}, fmt.Errorf("begin SML connection update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existingVersion int
	err = tx.QueryRow(ctx, `select version from tenant_sml_connections where tenant_id = $1 for update`, connection.TenantID).Scan(&existingVersion)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if expectedVersion != 0 {
			return sml.StoredConnection{}, sml.ErrConnectionVersionConflict
		}
		_, err = tx.Exec(ctx, `
			insert into tenant_sml_connections (
			  tenant_id, endpoint_url, config_file_name, database_name,
			  username_ciphertext, username_nonce, password_ciphertext, password_nonce,
			  encryption_key_id, version, readiness_status, created_at, updated_at
			) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 'UNTESTED', $10, $10)`,
			connection.TenantID, connection.EndpointURL, connection.ConfigFileName, connection.DatabaseName,
			connection.Username.Ciphertext, connection.Username.Nonce,
			connection.Password.Ciphertext, connection.Password.Nonce,
			connection.Username.KeyID, now,
		)
		if err != nil {
			return sml.StoredConnection{}, fmt.Errorf("insert SML connection: %w", err)
		}
	case err != nil:
		return sml.StoredConnection{}, fmt.Errorf("lock SML connection: %w", err)
	default:
		if expectedVersion != existingVersion {
			return sml.StoredConnection{}, sml.ErrConnectionVersionConflict
		}
		_, err = tx.Exec(ctx, `
			update tenant_sml_connections
			set endpoint_url = $2, config_file_name = $3, database_name = $4,
			    username_ciphertext = $5, username_nonce = $6,
			    password_ciphertext = $7, password_nonce = $8,
			    encryption_key_id = $9, version = version + 1,
			    readiness_status = 'UNTESTED', last_tested_at = null,
			    last_safe_error_code = null, updated_at = $10
			where tenant_id = $1`,
			connection.TenantID, connection.EndpointURL, connection.ConfigFileName, connection.DatabaseName,
			connection.Username.Ciphertext, connection.Username.Nonce,
			connection.Password.Ciphertext, connection.Password.Nonce,
			connection.Username.KeyID, now,
		)
		if err != nil {
			return sml.StoredConnection{}, fmt.Errorf("update SML connection: %w", err)
		}
	}

	updated, err := scanSMLConnection(tx.QueryRow(ctx, `
		select tenant_id, endpoint_url, config_file_name, database_name,
		       username_ciphertext, username_nonce, password_ciphertext, password_nonce,
		       encryption_key_id, version, readiness_status, last_tested_at,
		       coalesce(last_safe_error_code, ''), created_at, updated_at
		from tenant_sml_connections where tenant_id = $1`, connection.TenantID))
	if err != nil {
		return sml.StoredConnection{}, fmt.Errorf("read updated SML connection: %w", err)
	}
	auditJSON, _ := json.Marshal(map[string]any{
		"endpointUrl": updated.EndpointURL, "configFileName": updated.ConfigFileName,
		"databaseName": updated.DatabaseName, "version": updated.Version,
		"readinessStatus": updated.Readiness,
	})
	if err := insertAudit(ctx, tx, connection.TenantID, actorHash, "SML_CONNECTION_REPLACED", "SML_CONNECTION", connection.TenantID.String(), requestID, nil, auditJSON, now); err != nil {
		return sml.StoredConnection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return sml.StoredConnection{}, fmt.Errorf("commit SML connection update: %w", err)
	}
	return updated, nil
}

func (store *SMLConnectionStore) MarkTested(ctx context.Context, actorHash []byte, requestID string, tenantID uuid.UUID, status sml.Readiness, safeCode string, testedAt time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin SML test result: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		update tenant_sml_connections
		set readiness_status = $2, last_tested_at = $3,
		    last_safe_error_code = nullif($4, ''), updated_at = $3
		where tenant_id = $1`, tenantID, status, testedAt, safeCode)
	if err != nil {
		return fmt.Errorf("update SML test result: %w", err)
	}
	if result.RowsAffected() != 1 {
		return sml.ErrConnectionNotConfigured
	}
	auditJSON, _ := json.Marshal(map[string]any{"readinessStatus": status, "safeErrorCode": safeCode})
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SML_CONNECTION_TESTED", "SML_CONNECTION", tenantID.String(), requestID, nil, auditJSON, testedAt); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit SML test result: %w", err)
	}
	return nil
}

func scanSMLConnection(row rowScanner) (sml.StoredConnection, error) {
	var connection sml.StoredConnection
	var keyID string
	err := row.Scan(
		&connection.TenantID, &connection.EndpointURL, &connection.ConfigFileName, &connection.DatabaseName,
		&connection.Username.Ciphertext, &connection.Username.Nonce,
		&connection.Password.Ciphertext, &connection.Password.Nonce,
		&keyID, &connection.Version, &connection.Readiness, &connection.LastTestedAt,
		&connection.LastSafeErrorCode, &connection.CreatedAt, &connection.UpdatedAt,
	)
	connection.Username.KeyID = keyID
	connection.Password.KeyID = keyID
	return connection, err
}
