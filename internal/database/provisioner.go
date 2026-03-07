// Package database provides idempotent PostgreSQL database provisioning for mn-cli.
//
// During app deployment, mn-cli calls EnsureDatabase to create a PostgreSQL
// login role and database for an app, then stores the generated credentials
// in OpenBao vault at the app's vault path.
//
// # Vault contract
//
// Admin credentials are read from base.yaml's database.admin_vault_path
// (default: mn/data/platform/db01). The secret at that path must contain:
//
//	admin_user     → PostgreSQL superuser or role with CREATEDB + CREATEROLE
//	admin_password → admin password
//	host           → PG host (optional – overrides base.database.default_host)
//	port           → PG port as string (optional – overrides base.database.default_port)
//
// App credentials written by this package to the app vault path:
//
//	database_url      → postgresql://role:pass@host:port/dbname?sslmode=...
//	database_host     → host
//	database_port     → port as string
//	database_name     → database name
//	database_user     → login role name
//	database_password → generated password (preserved across re-deploys unless rotated)
//
// # Idempotency
//
// The provisioner is safe to run on every deploy:
//   - Role+database already exist → password re-applied from vault (no rotation)
//   - Role+database missing → created, password generated and stored
//   - Force-rotate → new password generated even if one exists in vault
package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
)

// Config defines the desired state of a PostgreSQL database and login role.
type Config struct {
	// Admin connection parameters (sourced from vault admin_vault_path).
	AdminHost     string
	AdminPort     int
	AdminUser     string
	AdminPassword string

	// Desired app database state.
	DatabaseName string   // database to create/verify (e.g. "public_api")
	Role         string   // login role to create/verify (e.g. "api")
	Extensions   []string // PG extensions to enable inside the database (e.g. "uuid-ossp")
	SSLMode      string   // sslmode for the generated DATABASE_URL: disable|require|verify-ca|verify-full
}

// Credentials holds the resolved database access details for the app.
type Credentials struct {
	Host         string
	Port         int
	DatabaseName string
	User         string
	Password     string
	// URL is the ready-to-use DATABASE_URL in postgresql:// scheme.
	URL string
}

// Result is returned by EnsureDatabase.
type Result struct {
	Credentials Credentials
	// Created is true when the role or database did not previously exist.
	Created bool
	// Rotated is true when a new password was generated (either first run or --force-rotate).
	Rotated bool
}

// EnsureDatabase idempotently provisions the PostgreSQL role and database
// described by cfg.
//
// existingPassword should be the value previously stored in vault (empty string
// on first run). If non-empty, it is reused to avoid credential churn on
// re-deploys. Pass empty string (or set ForceRotate=true via a wrapper) to
// generate a fresh password.
func EnsureDatabase(ctx context.Context, cfg Config, existingPassword string) (Result, error) {
	if cfg.AdminHost == "" {
		return Result{}, fmt.Errorf("database: AdminHost is required")
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = 5432
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "require"
	}
	if cfg.DatabaseName == "" {
		return Result{}, fmt.Errorf("database: DatabaseName is required")
	}
	if cfg.Role == "" {
		return Result{}, fmt.Errorf("database: Role is required")
	}

	// Connect as admin.
	adminDSN := buildDSN(cfg.AdminHost, cfg.AdminPort, "postgres",
		cfg.AdminUser, cfg.AdminPassword, "disable")
	db, err := sql.Open("postgres", adminDSN)
	if err != nil {
		return Result{}, fmt.Errorf("database: open admin connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		return Result{}, fmt.Errorf("database: ping %s:%d as %s: %w",
			cfg.AdminHost, cfg.AdminPort, cfg.AdminUser, err)
	}

	var result Result

	// ── Role ──────────────────────────────────────────────────────────────
	roleAlreadyExists, err := roleExists(ctx, db, cfg.Role)
	if err != nil {
		return Result{}, fmt.Errorf("database: check role %q: %w", cfg.Role, err)
	}

	password := existingPassword
	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return Result{}, fmt.Errorf("database: generate password: %w", err)
		}
		result.Rotated = true
	}

	if !roleAlreadyExists {
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`CREATE ROLE %s WITH LOGIN PASSWORD '%s'`,
				quoteIdent(cfg.Role), escapeLiteral(password)),
		); err != nil {
			return Result{}, fmt.Errorf("database: create role %q: %w", cfg.Role, err)
		}
		result.Created = true
	} else {
		// Always re-apply stored password so PG and vault stay in sync
		// (e.g. if someone changed PG directly, this self-heals on next deploy).
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`ALTER ROLE %s WITH PASSWORD '%s'`,
				quoteIdent(cfg.Role), escapeLiteral(password)),
		); err != nil {
			return Result{}, fmt.Errorf("database: set password for role %q: %w", cfg.Role, err)
		}
	}

	// ── Database ──────────────────────────────────────────────────────────
	dbAlreadyExists, err := databaseExists(ctx, db, cfg.DatabaseName)
	if err != nil {
		return Result{}, fmt.Errorf("database: check database %q: %w", cfg.DatabaseName, err)
	}

	if !dbAlreadyExists {
		// CREATE DATABASE cannot run inside a transaction, use ExecContext directly.
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`CREATE DATABASE %s OWNER %s`,
				quoteIdent(cfg.DatabaseName), quoteIdent(cfg.Role)),
		); err != nil {
			return Result{}, fmt.Errorf("database: create database %q: %w", cfg.DatabaseName, err)
		}
		result.Created = true
	} else {
		// Best-effort ownership alignment — may fail if DB is owned by another superuser.
		_, _ = db.ExecContext(ctx,
			fmt.Sprintf(`ALTER DATABASE %s OWNER TO %s`,
				quoteIdent(cfg.DatabaseName), quoteIdent(cfg.Role)))
	}

	// ── Grants ───────────────────────────────────────────────────────────
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf(`GRANT ALL PRIVILEGES ON DATABASE %s TO %s`,
			quoteIdent(cfg.DatabaseName), quoteIdent(cfg.Role)),
	); err != nil {
		return Result{}, fmt.Errorf("database: grant privileges on %q to %q: %w",
			cfg.DatabaseName, cfg.Role, err)
	}

	// Also grant CONNECT explicitly (defensive, usually included in ALL).
	_, _ = db.ExecContext(ctx,
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`,
			quoteIdent(cfg.DatabaseName), quoteIdent(cfg.Role)))

	// ── Extensions ────────────────────────────────────────────────────────
	// Connect to the target database to create extensions in the correct context.
	if len(cfg.Extensions) > 0 {
		extDSN := buildDSN(cfg.AdminHost, cfg.AdminPort, cfg.DatabaseName,
			cfg.AdminUser, cfg.AdminPassword, "disable")
		extDB, err := sql.Open("postgres", extDSN)
		if err != nil {
			return Result{}, fmt.Errorf("database: open connection to %q for extensions: %w", cfg.DatabaseName, err)
		}
		defer extDB.Close()

		for _, ext := range cfg.Extensions {
			if _, err := extDB.ExecContext(ctx,
				fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS %s`, quoteIdent(ext)),
			); err != nil {
				return Result{}, fmt.Errorf("database: enable extension %q in %q: %w",
					ext, cfg.DatabaseName, err)
			}
		}
	}

	result.Credentials = Credentials{
		Host:         cfg.AdminHost,
		Port:         cfg.AdminPort,
		DatabaseName: cfg.DatabaseName,
		User:         cfg.Role,
		Password:     password,
		URL:          buildURL(cfg.AdminHost, cfg.AdminPort, cfg.DatabaseName, cfg.Role, password, cfg.SSLMode),
	}
	return result, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func buildURL(host string, port int, dbName, user, password, sslMode string) string {
	return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=%s",
		user, password, host, port, dbName, sslMode)
}

func buildDSN(host string, port int, dbName, user, password, sslMode string) string {
	return fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=10",
		host, port, dbName, user, password, sslMode)
}

func roleExists(ctx context.Context, db *sql.DB, role string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM pg_roles WHERE rolname = $1`, role,
	).Scan(&count)
	return count > 0, err
}

func databaseExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM pg_database WHERE datname = $1`, name,
	).Scan(&count)
	return count > 0, err
}

// generatePassword creates a 32-character URL-safe random password.
// Uses crypto/rand for cryptographic strength.
func generatePassword() (string, error) {
	// 24 random bytes → 32 base64url characters (no padding chars needed).
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	// URLEncoding avoids + and / which can cause connection string parse issues.
	// We strip the trailing padding characters (=).
	s := base64.URLEncoding.EncodeToString(b)
	return strings.TrimRight(s, "="), nil
}

// quoteIdent safely double-quotes a PostgreSQL identifier.
// Role names and database names from manifests are trusted internal inputs,
// but double-quoting ensures names with hyphens or capitals work correctly.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// escapeLiteral single-quote–escapes a string for use in a SQL literal.
// Used only for passwords; a proper parameterized query cannot be used here
// because ALTER ROLE ... PASSWORD does not support $1 placeholders in lib/pq.
func escapeLiteral(s string) string {
	return strings.ReplaceAll(s, `'`, `''`)
}
