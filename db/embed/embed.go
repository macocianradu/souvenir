package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/db"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

type EmbedStore struct {
	logger    slog.Logger
	embedder  llm.Embedder
	tableName string
	pool      *pgxpool.Pool
}

type Chunk struct {
	id, content, contentHash string
}

func (e EmbedStore) TableName() string {
	return e.tableName
}

func (e EmbedStore) MaxDistance() float64 {
	return e.embedder.MaxDistance()
}

func (e EmbedStore) DistanceMargin() float64 {
	return e.embedder.DistanceMargin()
}

func (e EmbedStore) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	return e.embedder.EmbedQuery(ctx, query)
}

func (e EmbedStore) GetConversationsToEmbed(ctx context.Context, quietMinutes int) ([]string, error) {
	rows, err := e.pool.Query(ctx, fmt.Sprintf(
		`
		   SELECT DISTINCT c.id
		     FROM conversations c
		     JOIN message_chunks mc
		       ON mc.conversation_id = c.id
		LEFT JOIN %s v
		       ON v.chunk_id = mc.id AND v.content_hash = mc.content_hash
		    WHERE c.updated_at < now() - make_interval(mins => $1)
		      AND v.chunk_id IS NULL
		`,
		e.TableName()), quietMinutes)
	if err != nil {
		e.logger.Error("Error while getting conversations to embed", "error", err)
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		e.logger.Error("Error while reading conversations to embed", "error", err)
		return nil, err
	}
	return ids, nil
}

func NewEmbedStore(ctx context.Context, pool *pgxpool.Pool, dbName string, embedder llm.Embedder) (*EmbedStore, error) {
	if err := db.EnsureVectorExtension(ctx, pool, dbName); err != nil {
		return nil, err
	}
	store := EmbedStore{
		logger:   *slog.Default().With("Component", "EmbedStore"),
		embedder: embedder,
		pool:     pool,
		tableName: fmt.Sprintf("message_chunks_vec_%s_d%d",
			strings.ToLower(embedder.ID()), embedder.Dim()),
	}
	script := fmt.Sprintf(db.VectorTableTemplate, store.tableName, embedder.Dim())
	if _, err := pool.Exec(ctx, script); err != nil {
		store.logger.Error("Could not create vector table", "table", store.tableName, "error", err)
		return nil, err
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
		vecs, err := e.embedder.EmbedDocuments(ctx, texts)
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
