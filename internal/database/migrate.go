package database

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationNamePattern = regexp.MustCompile(`^(\d{6})_[a-z0-9_]+\.sql$`)

const migrationLockID int64 = 7_214_501_624

type Migration struct {
	Version       int
	Name          string
	Checksum      string
	SQL           string
	NoTransaction bool
}

func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	migrations := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := migrationNamePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("parse migration version %q: %w", entry.Name(), err)
		}
		contents, err := migrationFiles.ReadFile(filepath.ToSlash(filepath.Join("migrations", entry.Name())))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(contents)
		migrations = append(migrations, Migration{
			Version:       version,
			Name:          entry.Name(),
			Checksum:      hex.EncodeToString(sum[:]),
			SQL:           string(contents),
			NoTransaction: strings.HasPrefix(string(contents), "-- nextstep:no-transaction"),
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	for index, migration := range migrations {
		if migration.Version != index+1 {
			return nil, fmt.Errorf("migration sequence must start at 000001 without gaps; found %06d at position %d", migration.Version, index+1)
		}
	}
	return migrations, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := LoadMigrations()
	if err != nil {
		return err
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer connection.Release()

	if _, err := connection.Exec(ctx, `select pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() { _, _ = connection.Exec(context.Background(), `select pg_advisory_unlock($1)`, migrationLockID) }()

	if _, err := connection.Exec(ctx, `
		create table if not exists schema_migrations (
			version integer primary key,
			name text not null unique,
			checksum text not null,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return fmt.Errorf("ensure migration ledger: %w", err)
	}

	for _, migration := range migrations {
		var checksum string
		err := connection.QueryRow(ctx, `select checksum from schema_migrations where version = $1`, migration.Version).Scan(&checksum)
		switch {
		case err == nil:
			if checksum != migration.Checksum {
				return fmt.Errorf("migration %06d checksum does not match the applied migration", migration.Version)
			}
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read migration %06d status: %w", migration.Version, err)
		}

		if migration.NoTransaction {
			for _, statement := range nonTransactionalStatements(migration.SQL) {
				if _, err := connection.Exec(ctx, statement); err != nil {
					return fmt.Errorf("apply non-transactional migration %06d: %w", migration.Version, err)
				}
			}
			if _, err := connection.Exec(ctx, `insert into schema_migrations (version, name, checksum) values ($1, $2, $3)`, migration.Version, migration.Name, migration.Checksum); err != nil {
				return fmt.Errorf("record non-transactional migration %06d: %w", migration.Version, err)
			}
			continue
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %06d: %w", migration.Version, err)
		}
		if _, err := tx.Exec(ctx, migration.SQL); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %06d: %w", migration.Version, err)
		}
		if _, err := tx.Exec(ctx, `insert into schema_migrations (version, name, checksum) values ($1, $2, $3)`, migration.Version, migration.Name, migration.Checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %06d: %w", migration.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %06d: %w", migration.Version, err)
		}
	}
	return nil
}

func nonTransactionalStatements(sql string) []string {
	// Non-transactional migrations are intentionally restricted to simple DDL
	// such as CREATE INDEX CONCURRENTLY. Sending multiple statements in one
	// PostgreSQL request creates an implicit transaction block and is rejected.
	lines := strings.Split(sql, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		cleaned = append(cleaned, line)
	}
	parts := strings.Split(strings.Join(cleaned, "\n"), ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		if statement := strings.TrimSpace(part); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}
