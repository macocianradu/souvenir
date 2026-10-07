package db

import (
	"context"
	"fmt"
	"log/slog"

	"git.estatecloud.org/radumaco/souvenir/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open makes sure the database exists, connects to it and brings the schema
// up to date. The returned pool is shared by every store.
func Open(ctx context.Context, cfg config.DbConfig) (*pgxpool.Pool, error) {
	logger := slog.Default().With("Component", "Db")

	if err := ensureDatabase(ctx, cfg, logger); err != nil {
		return nil, fmt.Errorf("ensure database %q: %w", cfg.DbName, err)
	}
	pool, err := pgxpool.New(ctx, cfg.ConnectionString())
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := migrate(ctx, pool, logger); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return pool, nil
}

func ensureDatabase(ctx context.Context, cfg config.DbConfig, logger *slog.Logger) error {
	maintenance := cfg
	maintenance.DbName = "postgres"
	conn, err := pgx.Connect(ctx, maintenance.ConnectionString())
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`,
		cfg.DbName).Scan(&exists); err != nil {
		return err
	}
	if exists {
		logger.Debug("Database already exists", "name", cfg.DbName)
		return nil
	}

	logger.Info("Creating database", "name", cfg.DbName)
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{cfg.DbName}.Sanitize())
	return err
}

// EnsureVectorExtension creates the pgvector extension if it is missing.
// pgvector is not a trusted extension, so this needs a superuser the first
// time; the error says what to run when the app's user cannot do it.
func EnsureVectorExtension(ctx context.Context, pool *pgxpool.Pool, dbName string) error {
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("pgvector extension missing and could not be created (%w); "+
			"run as a superuser: psql -d %s -c 'CREATE EXTENSION vector'", err, dbName)
	}
	return nil
}
