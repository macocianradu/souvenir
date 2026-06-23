package embed

import (
	"context"
	"fmt"
	"log/slog"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"github.com/gopsql/pgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type EmbedStore struct {
	cfg       config.DbConfig
	logger    slog.Logger
	embedder  llm.Embedder
	tableName string
	pool	  *pgxpool.Pool
}

func (e EmbedStore) TableName() string {
	if e.tableName == "" {
		e.tableName = fmt.Sprintf(`message_chunks_vec_%s`, e.embedder.ID())
	}
	return e.tableName
}

func NewEmbedStore(ctx context.Context, cfg config.DbConfig, embedder llm.Embedder) (*EmbedStore, error) {
	logger := *slog.Default().With("Component", "EmbedStore")

	pool, err := pgxpool.New(ctx, cfg.ConnectionString())
	if err != nil {
		logger.ErrorContext(ctx, "Could not create pgxpool")
		return nil, err
	}
	store := EmbedStore{
		cfg:      cfg,
		logger:   logger,
		embedder: embedder,
		pool: pool,
	}

	const createVectorTableTemplate = `
	CREATE TABLE IF NOT EXISTS %s (
		chunk_id		UUID PRIMARY KEY REFERENCES message_chunks(id) ON DELETE CASCADE,
		embedding		vector(%d),
		content_hash	text not null
	)`
	conn := pgx.MustOpen(cfg.ConnectionString())
	var exists bool
	if err := conn.QueryRow(`SELECT EXISTS
			(SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public'
			AND table_name =$1)`, store.TableName()).Scan(&exists); err != nil {
		logger.Error("Error while checking table", "table", store.TableName(), "error", err)
		return nil, err
	}
	if exists {
		logger.Debug("Table found", "table", store.TableName())
	} else {
		logger.Debug("Table not found. Creating", "table", store.TableName())
		script := fmt.Sprintf(createVectorTableTemplate, store.TableName(), store.embedder.Dim())
		if _, err := conn.Exec(script); err != nil {
			logger.Error("Could not create table", "table", store.TableName(), "error", err)
			return nil, err
		}
	}
	return &store, nil
}

func (e EmbedStore) EmbedConversation(ctx context.Context, conversationId string) error {
	rows, err := e.pool.Query(ctx, `SELECT c.id, c.content, c.content_hash
		FROM message_chunks c
		LEFT JOIN $1 v
			ON v.chunk_id = c.id AND v.content_hash = c.content_hash
		WHERE c.conversation_id = $2 and v.chunk_id IS NULL`,
		e.TableName(), conversationId)

	if err != nil {
		e.logger.Error("Could not get message chunks", "conversation_id", conversationId, "error", err)
		return err
	}

	var pending []struct{
		id, content, contentHash string
	}
	for rows.Next() {
		var p struct{id, content, contentHash string}
		if err := rows.Scan(&p.id, &p.content, &p.contentHash); err != nil {
			rows.Close()
			e.logger.Error("Error reading chunk", "error", err)
			return err
		}
		pending = append(pending, p)
	}
	rows.Close()

	if len(pending) == 0 {
		e.logger.Debug("Nothing to embed", "conversation_id", conversationId)
		return nil
	}

	for start := 0; start < len(pending); start += e.cfg.ChunkSize {
		e.logger.Debug("", "a", start)
	}
	return nil
}
