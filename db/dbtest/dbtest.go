// Package dbtest gives tests a fresh, migrated database.
package dbtest

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool drops and recreates the database named by SOUVENIR_TEST_DB, on the
// server given by PGHOST, PGPORT, PGUSER and PGPASSWORD, and skips the test
// when it is not set. Each test package should use its own database name
// suffix, since packages run in parallel.
func Pool(t *testing.T, suffix string) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("SOUVENIR_TEST_DB")
	if base == "" {
		t.Skip("set SOUVENIR_TEST_DB to run against Postgres")
	}
	name := base + "_" + suffix
	cfg := config.DbConfig{
		Url:      cmp.Or(os.Getenv("PGHOST"), "localhost"),
		Port:     cmp.Or(os.Getenv("PGPORT"), "5432"),
		User:     os.Getenv("PGUSER"),
		Password: os.Getenv("PGPASSWORD"),
	}
	ctx := context.Background()
	maintenance := cfg
	maintenance.DbName = "postgres"
	conn, err := pgx.Connect(ctx, maintenance.ConnectionString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	ident := pgx.Identifier{name}.Sanitize()
	// template0, because template1 can carry a stale collation version.
	for _, stmt := range []string{"DROP DATABASE IF EXISTS %s WITH (FORCE)", "CREATE DATABASE %s TEMPLATE template0"} {
		if _, err := conn.Exec(ctx, fmt.Sprintf(stmt, ident)); err != nil {
			t.Fatal(err)
		}
	}
	cfg.DbName = name
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		conn, err := pgx.Connect(context.Background(), maintenance.ConnectionString())
		if err == nil {
			conn.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", ident))
			conn.Close(context.Background())
		}
	})
	return pool
}
