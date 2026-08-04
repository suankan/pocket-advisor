package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/suankan/pocket-advisor/internal/client/embedding"
	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/storage/postgres"
)

// createPostgres provisions the workspace's own database and role, then
// applies the Tier 2/3 schema to it, so a freshly created workspace is
// immediately usable (workspace-isolation.md §6, step 1).
//
// Idempotent: existence is checked before every CREATE, so a retry after a
// partial failure behaves identically to a first attempt.
func createPostgres(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}
	if w.DBPassword == "" {
		return fmt.Errorf("workspace %q has no postgres.credentials.password in %s", id, cfg.WorkspacesValuesPath)
	}

	admin, err := pgx.Connect(ctx, cfg.Postgres.AdminDSN)
	if err != nil {
		return fmt.Errorf("connect admin dsn: %w", err)
	}
	defer admin.Close(ctx)

	dbIdent := pgx.Identifier{w.DBName}.Sanitize()
	roleIdent := pgx.Identifier{w.DBUser}.Sanitize()

	roleExists, err := existsQuery(ctx, admin, "SELECT 1 FROM pg_roles WHERE rolname = $1", w.DBUser)
	if err != nil {
		return fmt.Errorf("check role: %w", err)
	}
	if !roleExists {
		// Password is a literal in the statement, not a bind parameter —
		// CREATE ROLE ... PASSWORD does not accept one. Never logged.
		stmt := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s", roleIdent, quoteLiteral(w.DBPassword))
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
		log.Info("postgres role created", "workspace_id", id)
	}

	dbExists, err := existsQuery(ctx, admin, "SELECT 1 FROM pg_database WHERE datname = $1", w.DBName)
	if err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	if !dbExists {
		if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", dbIdent)); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
		log.Info("postgres database created", "workspace_id", id)
	}

	if _, err := admin.Exec(ctx, fmt.Sprintf(
		"GRANT ALL PRIVILEGES ON DATABASE %s TO %s", dbIdent, roleIdent)); err != nil {
		return fmt.Errorf("grant database: %w", err)
	}

	if err := prepareWorkspaceDatabase(ctx, cfg, id); err != nil {
		return fmt.Errorf("prepare database: %w", err)
	}

	return applyWorkspaceSchema(ctx, cfg, id, log)
}

// prepareWorkspaceDatabase does the two things only a superuser can do,
// inside the workspace's own database, before handing off to its own
// unprivileged role for the rest of the schema (applyWorkspaceSchema):
//
//  1. Installs pgvector. CREATE EXTENSION requires superuser (pgvector is
//     not marked "trusted"), so this cannot be folded into the DDL that
//     runs as the workspace role — it would fail with "permission denied
//     to create extension" on every fresh workspace.
//  2. Makes the workspace role own the public schema. Since PostgreSQL 15,
//     a freshly created database's public schema grants CREATE to nobody
//     but its owner (the bootstrap superuser) — GRANT ALL PRIVILEGES ON
//     DATABASE does not touch schema-level privileges, so without this the
//     workspace role can connect but cannot create a single table in its
//     own database ("permission denied for schema public").
func prepareWorkspaceDatabase(ctx context.Context, cfg *config.Config, id string) error {
	w, err := cfg.Workspace(id)
	if err != nil {
		return err
	}
	u, err := url.Parse(cfg.Postgres.AdminDSN)
	if err != nil {
		return fmt.Errorf("parse infra.postgres.admin_dsn: %w", err)
	}
	u.Path = "/" + w.DBName

	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return fmt.Errorf("connect as admin to %s: %w", w.DBName, err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("create extension: %w", err)
	}

	ident := pgx.Identifier{w.DBUser}.Sanitize()
	if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER SCHEMA public OWNER TO %s", ident)); err != nil {
		return fmt.Errorf("alter schema owner: %w", err)
	}
	return nil
}

// applyWorkspaceSchema connects as the workspace's own role and applies the
// Tier 2/3 DDL, reusing postgres.DB.ApplySchema (schema.go) unchanged — one
// implementation of the schema, whether it's a fresh workspace or a re-probe
// after a model change (ingestion-design.md §4.4).
//
// This is now the only caller. A --bootstrap-schema CLI mode used to repeat
// these same steps without calling this, which is exactly the drift a second
// copy invites; it was removed once every case it served turned out to be
// covered here or by VerifyDimension.
func applyWorkspaceSchema(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	dsn, err := cfg.WorkspacePostgresDSN(id)
	if err != nil {
		return err
	}
	db, err := postgres.Connect(ctx, dsn, 2)
	if err != nil {
		return fmt.Errorf("connect workspace database: %w", err)
	}
	defer db.Close()

	if err := cfg.RequireEmbedding(); err != nil {
		return err
	}
	client := embedding.New(cfg.Embedding)
	info, err := client.Probe(ctx)
	if err != nil {
		return fmt.Errorf("probe embedding endpoint %s: %w", cfg.Embedding.Endpoint, err)
	}
	model := cfg.Embedding.Model
	if info.Model != "" {
		model = info.Model
	}

	if err := db.ApplySchema(ctx, postgres.SchemaMetadata{
		EmbedModel: model,
		EmbedDim:   info.Dimension,
	}); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	log.Info("workspace schema applied", "workspace_id", id, "model", model, "dimension", info.Dimension)
	return nil
}

// deletePostgres drops the workspace's database and role. WITH (FORCE)
// terminates any lingering connections rather than failing the drop —
// there should be none by the time --delete-workspace reaches this step,
// but a lingering connection must not block teardown.
func deletePostgres(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	admin, err := pgx.Connect(ctx, cfg.Postgres.AdminDSN)
	if err != nil {
		return fmt.Errorf("connect admin dsn: %w", err)
	}
	defer admin.Close(ctx)

	ident := pgx.Identifier{id}.Sanitize()

	if _, err := admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", ident)); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", ident)); err != nil {
		return fmt.Errorf("drop role: %w", err)
	}
	log.Info("postgres database and role dropped", "workspace_id", id)
	return nil
}

func existsQuery(ctx context.Context, conn *pgx.Conn, query, arg string) (bool, error) {
	var n int
	err := conn.QueryRow(ctx, query, arg).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// quoteLiteral escapes a string for use as a SQL string literal, for the one
// place a password must appear as a literal rather than a bind parameter
// (CREATE ROLE ... PASSWORD does not accept one). Doubles embedded quotes
// and backslashes, matching Postgres's standard_conforming_strings behavior.
func quoteLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' || r == '\\' {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}
