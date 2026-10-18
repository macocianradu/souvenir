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

	"git.estatecloud.org/radumaco/souvenir/db/users"
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
	decoys    decoyVectors
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
	userId, err := users.Id(ctx)
	if err != nil {
		return m, false, err
	}
	err = s.pool.QueryRow(ctx,
		`
		WITH inserted AS (
			INSERT INTO memories (content, content_hash, source_conversation_id, user_id)
			     VALUES ($1, $2, (SELECT id FROM conversations WHERE id = NULLIF($3, '')::uuid AND user_id = $4), $4)
			ON CONFLICT (user_id, content_hash) DO NOTHING
			  RETURNING id, content, created_at
		)
		SELECT id, content, created_at, true FROM inserted
		 UNION ALL
		SELECT id, content, created_at, false FROM memories WHERE user_id = $4 AND content_hash = $2
		 LIMIT 1
		`, content, hex.EncodeToString(hash[:]), conversationId, userId).Scan(&m.Id, &m.Content, &m.CreatedAt, &created)
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
	userId, err := users.Id(ctx)
	if err != nil {
		return m, err
	}
	err = s.pool.QueryRow(ctx,
		`
		   UPDATE memories
		      SET content = $2, content_hash = $3
		    WHERE id = $1 AND user_id = $4
		RETURNING id, content, created_at
		`, id, content, hex.EncodeToString(hash[:]), userId).Scan(&m.Id, &m.Content, &m.CreatedAt)
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
	userId, err := users.Id(ctx)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM memories WHERE id = $1 AND user_id = $2`, id, userId)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no memory with id %s", id)
	}
	return nil
}

func (s *Store) List(ctx context.Context) ([]Memory, error) {
	userId, err := users.Id(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, content, created_at FROM memories WHERE user_id = $1 ORDER BY created_at DESC`, userId)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMemory)
}

func (s *Store) Search(ctx context.Context, query string, limit int) ([]Memory, error) {
	var maxDistance float64
	if s.embedder != nil {
		maxDistance = s.embedder.MaxDistance()
	}
	found, err := s.search(ctx, query, s.embedQuery(ctx, query), limit, maxDistance)
	return memories(found), err
}

func (s *Store) Recall(ctx context.Context, message string, limit int) ([]Memory, error) {
	vec := s.embedQuery(ctx, message)
	var maxDistance, mean, std float64
	if vec != nil {
		maxDistance = s.embedder.RecallDistance()
		if k := s.embedder.RecallDeviations(); k > 0 {
			vectors, err := s.decoyVectors(ctx)
			if err != nil {
				// Without the decoys nothing tells a related memory from the
				// rest, so only keywords are trusted.
				s.logger.Warn("Could not embed the decoys, recalling by keywords only", "error", err)
				vec = nil
			} else {
				mean, std = spread(vec, vectors)
				if cutoff := mean - k*std; maxDistance == 0 || cutoff < maxDistance {
					maxDistance = cutoff
				}
				// 0 would turn the cutoff off rather than keep nothing.
				maxDistance = max(maxDistance, 1e-9)
				s.logger.Debug("Recall cutoff", "decoyMean", mean, "decoyStd", std, "cutoff", maxDistance)
			}
		}
	}
	found, err := s.search(ctx, message, vec, limit, maxDistance)
	for _, h := range found {
		switch {
		case h.distance == nil:
			s.logger.Debug("Recalled memory", "id", h.Id, "keyword", true)
		case std > 0:
			s.logger.Debug("Recalled memory", "id", h.Id, "distance", *h.distance, "z", (*h.distance-mean)/std)
		default:
			s.logger.Debug("Recalled memory", "id", h.Id, "distance", *h.distance)
		}
	}
	return memories(found), err
}

func (s *Store) embedQuery(ctx context.Context, query string) []float32 {
	if s.embedder == nil {
		return nil
	}
	vec, err := s.embedder.EmbedQuery(ctx, query)
	if err != nil {
		s.logger.Warn("Could not embed query, using keyword search only", "error", err)
		return nil
	}
	return vec
}

type hit struct {
	Memory
	distance *float64
}

func memories(hits []hit) []Memory {
	found := make([]Memory, len(hits))
	for i, h := range hits {
		found[i] = h.Memory
	}
	return found
}

func (s *Store) search(ctx context.Context, query string, vec []float32, limit int, maxDistance float64) ([]hit, error) {
	userId, err := users.Id(ctx)
	if err != nil {
		return nil, err
	}
	args := []any{query, limit, userId}
	vectorLeg := `SELECT NULL::uuid AS id, NULL::bigint AS rank, NULL::float8 AS distance WHERE false`
	if vec != nil {
		args = append(args, pgvector.NewVector(vec), maxDistance, s.embedder.DistanceMargin())
		vectorLeg = fmt.Sprintf(`
			SELECT id, row_number() OVER (ORDER BY distance) AS rank, distance
			  FROM (
				  SELECT id, distance, min(distance) OVER () AS best
				    FROM (
				  SELECT v.memory_id AS id, v.embedding <=> $4 AS distance
				    FROM %s v
				    JOIN memories m ON m.id = v.memory_id AND m.content_hash = v.content_hash
				   WHERE m.user_id = $3
				     AND ($5::float8 = 0 OR v.embedding <=> $4 <= $5)
				) nearest
			  ) scored
			 WHERE $6::float8 = 0 OR distance <= best + $6`, s.tableName)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		WITH keyword AS (
			SELECT m.id, row_number() OVER (ORDER BY ts_rank_cd(m.tsv, q) DESC) AS rank, NULL::float8 AS distance
			  FROM memories m, websearch_to_tsquery('english', $1) q
			 WHERE m.tsv @@ q
			   AND m.user_id = $3
		),
		vector AS (%s),
		fused AS (
			  SELECT id, sum(1.0 / (60 + rank)) AS score, min(distance) AS distance
			    FROM (SELECT * FROM keyword UNION ALL SELECT * FROM vector) ranked
			GROUP BY id
		)
		  SELECT m.id, m.content, m.created_at, f.distance
		    FROM fused f
		    JOIN memories m ON m.id = f.id
		ORDER BY f.score DESC
		   LIMIT $2
		`, vectorLeg), args...)
	if err != nil {
		s.logger.Error("Memory search failed", "error", err)
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (hit, error) {
		var h hit
		return h, row.Scan(&h.Id, &h.Content, &h.CreatedAt, &h.distance)
	})
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
