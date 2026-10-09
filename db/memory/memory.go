package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/llm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

const vectorTableTemplate = `
	CREATE TABLE IF NOT EXISTS %s (
		memory_id    UUID PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
		embedding    vector(%d),
		content_hash TEXT NOT NULL,
		created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
	)`

type Memory struct {
	Id        string
	Content   string
	CreatedAt time.Time
}

type Store struct {
	logger    slog.Logger
	pool      *pgxpool.Pool
	embedder  *llm.Embedder
	tableName string
}

func NewStore(ctx context.Context, pool *pgxpool.Pool, embedder *llm.Embedder) (*Store, error) {
	s := &Store{logger: *slog.Default().With("Component", "Memory"), pool: pool, embedder: embedder}
	if embedder == nil {
		return s, nil
	}
	s.tableName = fmt.Sprintf("memories_vec_%s_d%d", strings.ToLower(embedder.ID()), embedder.Dim())
	if _, err := pool.Exec(ctx, fmt.Sprintf(vectorTableTemplate, s.tableName, embedder.Dim())); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Save(ctx context.Context, content, conversationId string) (Memory, bool, error) {
	content = strings.TrimSpace(content)
	hash := sha256.Sum256([]byte(content))
	var m Memory
	var created bool
	err := s.pool.QueryRow(ctx,
		`
		WITH inserted AS (
			INSERT INTO memories (content, content_hash, source_conversation_id)
			     VALUES ($1, $2, NULLIF($3, '')::uuid)
			ON CONFLICT (content_hash) DO NOTHING
			  RETURNING id, content, created_at
		)
		SELECT id, content, created_at, true FROM inserted
		 UNION ALL
		SELECT id, content, created_at, false FROM memories WHERE content_hash = $2
		 LIMIT 1
		`, content, hex.EncodeToString(hash[:]), conversationId).Scan(&m.Id, &m.Content, &m.CreatedAt, &created)
	if err != nil {
		s.logger.Error("Could not save memory", "error", err)
		return m, false, err
	}
	if created {
		if err := s.Backfill(ctx); err != nil {
			s.logger.Warn("Memory saved without a vector for now", "id", m.Id, "error", err)
		}
	}
	return m, created, nil
}

func (s *Store) Update(ctx context.Context, id, content string) (Memory, error) {
	content = strings.TrimSpace(content)
	hash := sha256.Sum256([]byte(content))
	var m Memory
	err := s.pool.QueryRow(ctx,
		`
		   UPDATE memories
		      SET content = $2, content_hash = $3
		    WHERE id = $1
		RETURNING id, content, created_at
		`, id, content, hex.EncodeToString(hash[:])).Scan(&m.Id, &m.Content, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, fmt.Errorf("no memory with id %s", id)
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return m, errors.New("another memory already says exactly that; forget this one instead")
	}
	if err != nil {
		return m, err
	}
	if err := s.Backfill(ctx); err != nil {
		s.logger.Warn("Memory updated without a fresh vector for now", "id", m.Id, "error", err)
	}
	return m, nil
}

func (s *Store) Forget(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memories WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no memory with id %s", id)
	}
	return nil
}

func (s *Store) List(ctx context.Context) ([]Memory, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, content, created_at FROM memories ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMemory)
}

func (s *Store) Search(ctx context.Context, query string, limit int) ([]Memory, error) {
	args := []any{query, limit}
	vectorLeg := `SELECT NULL::uuid AS id, NULL::bigint AS rank WHERE false`
	if s.embedder != nil {
		vec, err := s.embedder.EmbedQuery(ctx, query)
		if err != nil {
			s.logger.Warn("Could not embed query, using keyword search only", "error", err)
		} else {
			args = append(args, pgvector.NewVector(vec), s.embedder.MaxDistance(), s.embedder.DistanceMargin())
			vectorLeg = fmt.Sprintf(`
				SELECT id, row_number() OVER (ORDER BY distance) AS rank
				  FROM (
					  SELECT id, distance, min(distance) OVER () AS best
					    FROM (
					  SELECT v.memory_id AS id, v.embedding <=> $3 AS distance
					    FROM %s v
					    JOIN memories m ON m.id = v.memory_id AND m.content_hash = v.content_hash
					   WHERE $4::float8 = 0 OR v.embedding <=> $3 <= $4
					) nearest
				  ) scored
				 WHERE $5::float8 = 0 OR distance <= best + $5`, s.tableName)
		}
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		WITH keyword AS (
			SELECT m.id, row_number() OVER (ORDER BY ts_rank_cd(m.tsv, q) DESC) AS rank
			  FROM memories m, websearch_to_tsquery('english', $1) q
			 WHERE m.tsv @@ q
		),
		vector AS (%s),
		fused AS (
			  SELECT id, sum(1.0 / (60 + rank)) AS score
			    FROM (SELECT * FROM keyword UNION ALL SELECT * FROM vector) ranked
			GROUP BY id
		)
		  SELECT m.id, m.content, m.created_at
		    FROM fused f
		    JOIN memories m ON m.id = f.id
		ORDER BY f.score DESC
		   LIMIT $2
		`, vectorLeg), args...)
	if err != nil {
		s.logger.Error("Memory search failed", "error", err)
		return nil, err
	}
	return pgx.CollectRows(rows, scanMemory)
}

func (s *Store) Backfill(ctx context.Context) error {
	if s.embedder == nil {
		return nil
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT m.id, m.content, m.content_hash
		  FROM memories m
		  LEFT JOIN %s v ON v.memory_id = m.id AND v.content_hash = m.content_hash
		 WHERE v.memory_id IS NULL
		`, s.tableName))
	if err != nil {
		return err
	}
	type pending struct{ id, content, hash string }
	todo, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		return p, row.Scan(&p.id, &p.content, &p.hash)
	})
	if err != nil || len(todo) == 0 {
		return err
	}
	size := max(s.embedder.Cfg.BatchSize, 1)
	for start := 0; start < len(todo); start += size {
		group := todo[start:min(start+size, len(todo))]
		texts := make([]string, len(group))
		for i, p := range group {
			texts[i] = p.content
		}
		vecs, err := s.embedder.EmbedDocuments(ctx, texts)
		if err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for i, p := range group {
			batch.Queue(fmt.Sprintf(`
				INSERT INTO %s (memory_id, embedding, content_hash)
				     VALUES ($1, $2, $3)
				ON CONFLICT (memory_id) DO UPDATE
				        SET embedding = EXCLUDED.embedding, content_hash = EXCLUDED.content_hash
				`, s.tableName), p.id, pgvector.NewVector(vecs[i]), p.hash)
		}
		if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	return nil
}

func scanMemory(row pgx.CollectableRow) (Memory, error) {
	var m Memory
	return m, row.Scan(&m.Id, &m.Content, &m.CreatedAt)
}
