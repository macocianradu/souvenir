package search

import (
	"context"
	"fmt"
	"log/slog"

	"git.estatecloud.org/radumaco/souvenir/db/embed"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

const rrfK = 60

type Hit struct {
	ConversationId    string
	ConversationTitle string
	MessageId         string
	Seq               int
	Role              string
	Snippet           string
	Score             float64
}

type Options struct {
	ConversationId string
	Limit          int
}

type Searcher struct {
	logger slog.Logger
	pool   *pgxpool.Pool
	store  *embed.EmbedStore
}

func New(pool *pgxpool.Pool, store *embed.EmbedStore) *Searcher {
	return &Searcher{
		logger: *slog.Default().With("Component", "Search"),
		pool:   pool,
		store:  store,
	}
}

func (s Searcher) Search(ctx context.Context, query string, opts Options) ([]Hit, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	candidates := max(opts.Limit*4, 50)
	var conversation *string
	if opts.ConversationId != "" {
		conversation = &opts.ConversationId
	}

	args := []any{query, conversation, candidates, opts.Limit}
	vectorLeg := `SELECT NULL::uuid AS id, NULL::bigint AS rank WHERE false`
	if s.store != nil {
		vec, err := s.store.EmbedQuery(ctx, query)
		if err != nil {
			s.logger.Warn("Could not embed query, using keyword search only", "error", err)
		} else {
			args = append(args, pgvector.NewVector(vec), s.store.MaxDistance(), s.store.DistanceMargin())
			vectorLeg = fmt.Sprintf(`
				SELECT message_id AS id, row_number() OVER (ORDER BY distance) AS rank
				  FROM (
					  SELECT message_id, distance, min(distance) OVER () AS best
					    FROM (
					  SELECT mc.message_id, min(v.embedding <=> $5) AS distance
					    FROM %s v
					    JOIN message_chunks mc
					      ON mc.id = v.chunk_id AND mc.content_hash = v.content_hash
					   WHERE $2::uuid IS NULL OR mc.conversation_id = $2
					GROUP BY mc.message_id
					  HAVING $6::float8 = 0 OR min(v.embedding <=> $5) <= $6
					ORDER BY distance
					   LIMIT $3
					    ) nearest
				  ) scored
				 WHERE $7::float8 = 0 OR distance <= best + $7`, s.store.TableName())
		}
	}

	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		WITH keyword AS (
			SELECT m.id, row_number() OVER (ORDER BY ts_rank_cd(m.tsv, q) DESC) AS rank
			  FROM messages m, websearch_to_tsquery('english', $1) q
			 WHERE m.tsv @@ q
			   AND m.role IN ('user', 'assistant')
			   AND ($2::uuid IS NULL OR m.conversation_id = $2)
			 ORDER BY rank
			 LIMIT $3
		),
		vector AS (%s),
		fused AS (
			  SELECT id, sum(1.0 / (%d + rank))::float8 AS score
			    FROM (SELECT * FROM keyword UNION ALL SELECT * FROM vector) ranked
			GROUP BY id
		)
		  SELECT m.conversation_id, COALESCE(c.title, ''), m.id, m.seq, m.role,
		         ts_headline('english', m.content, websearch_to_tsquery('english', $1),
		                     'MaxFragments=2, MinWords=5, MaxWords=20, StartSel=«, StopSel=»'),
		         f.score
		    FROM fused f
		    JOIN messages m ON m.id = f.id
		    JOIN conversations c ON c.id = m.conversation_id
		ORDER BY f.score DESC
		   LIMIT $4
		`, vectorLeg, rrfK), args...)
	if err != nil {
		s.logger.Error("Search failed", "error", err)
		return nil, err
	}
	hits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Hit, error) {
		var h Hit
		err := row.Scan(&h.ConversationId, &h.ConversationTitle, &h.MessageId, &h.Seq, &h.Role, &h.Snippet, &h.Score)
		return h, err
	})
	if err != nil {
		s.logger.Error("Could not read search results", "error", err)
		return nil, err
	}
	return hits, nil
}
