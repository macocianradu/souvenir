package db

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLock is the advisory lock key held while migrating, so two
// instances starting at once do not apply the same migration twice.
const migrationLock = 0x736f7576 // "souv"

// migrations are applied in order and recorded in schema_migrations. Never
// edit one that has shipped; append a new one instead.
//
// Migration 1 is the baseline. It is written to be idempotent so databases
// created before migrations existed adopt it without losing data.
var migrations = []string{
	`
	CREATE TABLE IF NOT EXISTS conversations (
		id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		title               TEXT,
		title_source        TEXT,
		summary             TEXT,
		summary_through_seq INT DEFAULT 0,
		created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
	);
	ALTER TABLE conversations ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

	CREATE TABLE IF NOT EXISTS messages (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		role            TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		content         TEXT NOT NULL,
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		tsv             TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
		UNIQUE (conversation_id, seq)
	);
	ALTER TABLE messages ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
	CREATE INDEX IF NOT EXISTS messages_tsv_idx ON messages USING gin (tsv);

	CREATE TABLE IF NOT EXISTS message_chunks (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		message_id      UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		content         TEXT NOT NULL,
		content_hash    TEXT NOT NULL,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
	);
	ALTER TABLE message_chunks ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();
	CREATE INDEX IF NOT EXISTS message_chunks_conversation_id_idx ON message_chunks (conversation_id);
	DELETE FROM message_chunks a
	      USING message_chunks b
	      WHERE a.message_id = b.message_id
	        AND a.content_hash = b.content_hash
	        AND a.id > b.id;
	CREATE UNIQUE INDEX IF NOT EXISTS message_chunks_message_id_content_hash_idx
	    ON message_chunks (message_id, content_hash);
	`,
	`
	CREATE TABLE conversation_summaries (
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		through_seq     INTEGER NOT NULL,
		content         TEXT NOT NULL,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (conversation_id, through_seq)
	);
	`,
	`
	CREATE TABLE memories (
		id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		content                TEXT NOT NULL,
		content_hash           TEXT NOT NULL UNIQUE,
		source_conversation_id UUID REFERENCES conversations(id) ON DELETE SET NULL,
		created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
		tsv                    TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED
	);
	CREATE INDEX memories_tsv_idx ON memories USING gin (tsv);
	`,
	`
	ALTER TABLE conversations DROP COLUMN summary_through_seq;
	`,
	`
	CREATE TABLE message_images (
		message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		position   INTEGER NOT NULL,
		media_type TEXT NOT NULL,
		data       BYTEA NOT NULL,
		PRIMARY KEY (message_id, position)
	);
	`,
	`
	CREATE TABLE users (
		id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name       TEXT NOT NULL UNIQUE,
		is_admin   BOOLEAN NOT NULL DEFAULT false,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);
	INSERT INTO users (name, is_admin) VALUES ('owner', true);

	CREATE TABLE ssh_keys (
		fingerprint TEXT PRIMARY KEY,
		user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		public_key  TEXT NOT NULL,
		comment     TEXT NOT NULL DEFAULT '',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	);
	CREATE INDEX ssh_keys_user_id_idx ON ssh_keys (user_id);

	ALTER TABLE conversations ADD COLUMN user_id UUID REFERENCES users(id) ON DELETE CASCADE;
	UPDATE conversations SET user_id = (SELECT id FROM users);
	ALTER TABLE conversations ALTER COLUMN user_id SET NOT NULL;
	CREATE INDEX conversations_user_id_updated_at_idx ON conversations (user_id, updated_at DESC);

	ALTER TABLE memories ADD COLUMN user_id UUID REFERENCES users(id) ON DELETE CASCADE;
	UPDATE memories SET user_id = (SELECT id FROM users);
	ALTER TABLE memories ALTER COLUMN user_id SET NOT NULL;
	ALTER TABLE memories DROP CONSTRAINT memories_content_hash_key;
	ALTER TABLE memories ADD CONSTRAINT memories_user_id_content_hash_key UNIQUE (user_id, content_hash);
	`,
	`
	CREATE TABLE oidc_identities (
		issuer     TEXT NOT NULL,
		subject    TEXT NOT NULL,
		user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (issuer, subject),
		UNIQUE (user_id, issuer)
	);

	CREATE TABLE oidc_logins (
		state      TEXT PRIMARY KEY,
		verifier   TEXT NOT NULL,
		nonce      TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);

	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX sessions_user_id_idx ON sessions (user_id);
	`,
}

func migrate(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		version := i + 1
		logger.Info("Applying migration", "version", version)
		if _, err := tx.Exec(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			return fmt.Errorf("record migration %d: %w", version, err)
		}
	}
	logger.Debug("Schema up to date", "version", len(migrations))
	return tx.Commit(ctx)
}
