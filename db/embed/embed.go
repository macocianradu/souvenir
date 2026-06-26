package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

type EmbedStore struct {
	cfg       config.DbConfig
	logger    slog.Logger
	embedder  llm.Embedder
	tableName string
	pool      *pgxpool.Pool
}

type Chunk struct {
	id, content, contentHash string
}

func (e EmbedStore) TableName() string {
	if e.tableName == "" {
		e.tableName = fmt.Sprintf(`message_chunks_vec_%s`, e.embedder.ID())
	}
	return e.tableName
}

func (e EmbedStore) GetConversationsToEmbed(ctx context.Context) ([]string, error) {
	rows, err := e.pool.Query(ctx, fmt.Sprintf(
		`
		   SELECT DISTINC c.id
		     FROM conversations c
		     JOIN message_chunks mc
		       ON mc.conversation_id = c.id
	    LEFT JOIN %s v
			   ON v.chunk_id = mc.id AND v.content_hash = mc.content_hash
		    WHERE c.updated_at < now() - interval '5 hours'
		      AND v.chunk_id is NULL;
		`,
		e.TableName()))
	if err != nil {
		e.logger.Error("Error while getting conversations to embed", "error", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		err = rows.Scan(&id)
		if err != nil {
			e.logger.Error("Could not get id", "error", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
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
		pool:     pool,
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`
		SELECT EXISTS
			(SELECT 1
			   FROM information_schema.tables
			  WHERE table_schema = 'public'
			    AND table_name =$1)
		`,
		store.TableName()).Scan(&exists); err != nil {
		logger.Error("Error while checking table", "table", store.TableName(), "error", err)
		return nil, err
	}
	if exists {
		logger.Debug("Table found", "table", store.TableName())
	} else {
		logger.Debug("Table not found. Creating", "table", store.TableName())
		script := fmt.Sprintf(db.CreateVectorTableTemplate, store.TableName(), store.embedder.Dim())
		if _, err := pool.Exec(ctx, script); err != nil {
			logger.Error("Could not create table", "table", store.TableName(), "error", err)
			return nil, err
		}
	}
	return &store, nil
}

func (e EmbedStore) EmbedConversation(ctx context.Context, conversationId string) error {
	rows, err := e.pool.Query(ctx, fmt.Sprintf(
		`
		   SELECT c.id, c.content, c.content_hash
		     FROM message_chunks c
		LEFT JOIN %s v
		       ON v.chunk_id = c.id 
			  AND v.content_hash = c.content_hash
		    WHERE c.conversation_id = $1
			  AND v.chunk_id IS NULL
		`,
		e.TableName()),
		conversationId)

	if err != nil {
		e.logger.Error("Could not get message chunks", "conversation_id", conversationId, "error", err)
		return err
	}

	var pending []Chunk

	for rows.Next() {
		var p Chunk
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

	for start := 0; start < len(pending); start += e.embedder.Cfg.BatchSize {
		batch := pending[start:min(start+e.embedder.Cfg.BatchSize, len(pending))]

		texts := make([]string, len(batch))
		for i, chunk := range batch {
			texts[i] = chunk.content
		}
		vecs, err := e.embedder.EmbedBatch(texts)
		if err != nil {
			e.logger.Error("There was an error embedding batch", "conversation_id", conversationId, "error", err)
			return err
		}
		if len(vecs) != len(batch) {
			err := fmt.Sprintf("Embedder returned %d vecs for %d inputs", len(vecs), len(batch))
			e.logger.Error(err)
			return errors.New(err)
		}
		if err := e.upsert(ctx, batch, vecs); err != nil {
			e.logger.Error("Error inserting vectors in db", "error", err)
			return err
		}
	}
	return nil
}

func (e EmbedStore) upsert(ctx context.Context, chunks []Chunk, vecs [][]float32) error {
	query := fmt.Sprintf(
		`
		INSERT INTO %s (chunk_id, embedding, content_hash)
		     VALUES ($1, $2, $3)
		ON CONFLICT (chunk_id) DO UPDATE
			    SET embedding = EXCLUDED.embedding,
					content_hash = EXCLUDED.content_hash
		`,
		e.TableName())

	batch := &pgx.Batch{}
	for i, chunk := range chunks {
		batch.Queue(query, chunk.id, pgvector.NewVector(vecs[i]), chunk.contentHash)
	}
	result := e.pool.SendBatch(ctx, batch)
	defer result.Close()
	for range chunks {
		if _, err := result.Exec(); err != nil {
			e.logger.Error("Error inserting chunk", "error", err)
			return err
		}
	}
	return nil
}

func (e EmbedStore) Close() {
	e.pool.Close()
}
