package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DbClient struct {
	cfg    config.DbConfig
	logger slog.Logger
	pool   *pgxpool.Pool
}

func New(pool *pgxpool.Pool, cfg config.DbConfig) *DbClient {
	return &DbClient{
		logger: *slog.Default().With("Component", "History"),
		cfg:    cfg,
		pool:   pool,
	}
}

func (cl DbClient) GetConversation(ctx context.Context, id string) (model.Conversation, error) {
	cl.logger.Debug("Searching for conversation", "id", id)
	var conv model.Conversation
	if err := cl.pool.QueryRow(ctx,
		`
		SELECT id, COALESCE(title, ''), COALESCE(title_source, ''), COALESCE(summary, '')
		  FROM conversations
		 WHERE id = $1
		`, id).Scan(&conv.Id, &conv.Title, &conv.TitleSource, &conv.Summary); err != nil {
		cl.logger.Error("Could not fetch conversation", "id", id, "error", err)
		return conv, err
	}
	rows, err := cl.pool.Query(ctx,
		`
		  SELECT id, role, content, seq
		    FROM messages
		   WHERE conversation_id = $1
		ORDER BY seq
		`, id)
	if err != nil {
		cl.logger.Error("Could not fetch messages", "conversation_id", id, "error", err)
		return conv, err
	}
	defer rows.Close()
	for rows.Next() {
		var msg model.Message
		if err := rows.Scan(&msg.Id, &msg.Role, &msg.Content, &msg.Seq); err != nil {
			cl.logger.Error("Could not read message", "conversation_id", id, "error", err)
			return conv, err
		}
		conv.Messages = append(conv.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		cl.logger.Error("Could not fetch messages", "conversation_id", id, "error", err)
		return conv, err
	}
	var summary model.ContextSummary
	err = cl.pool.QueryRow(ctx,
		`
		  SELECT through_seq, content
		    FROM conversation_summaries
		   WHERE conversation_id = $1
		ORDER BY through_seq DESC
		   LIMIT 1
		`, id).Scan(&summary.ThroughSeq, &summary.Content)
	switch {
	case err == nil:
		conv.ContextSummary = &summary
	case !errors.Is(err, pgx.ErrNoRows):
		cl.logger.Error("Could not fetch context summary", "conversation_id", id, "error", err)
		return conv, err
	}
	cl.logger.Debug("Found conversation", "id", conv.Id, "messages", len(conv.Messages))
	return conv, nil
}

func (cl DbClient) SaveContextSummary(ctx context.Context, conversationId string, summary model.ContextSummary) error {
	_, err := cl.pool.Exec(ctx,
		`
		INSERT INTO conversation_summaries (conversation_id, through_seq, content)
		     VALUES ($1, $2, $3)
		ON CONFLICT (conversation_id, through_seq) DO UPDATE
		        SET content = EXCLUDED.content
		`, conversationId, summary.ThroughSeq, summary.Content)
	if err != nil {
		cl.logger.Error("Could not save context summary", "conversation_id", conversationId, "error", err)
	}
	return err
}

func (cl DbClient) GetConversations(ctx context.Context) ([]model.Conversation, error) {
	cl.logger.Debug("Fetching conversations")
	rows, err := cl.pool.Query(ctx,
		`
		  SELECT id, COALESCE(title, ''), COALESCE(summary, '')
		    FROM conversations
		ORDER BY updated_at DESC
		`)
	if err != nil {
		cl.logger.Error("Could not fetch conversations", "error", err)
		return nil, err
	}
	defer rows.Close()
	conversations := []model.Conversation{}
	for rows.Next() {
		var conv model.Conversation
		if err := rows.Scan(&conv.Id, &conv.Title, &conv.Summary); err != nil {
			cl.logger.Error("Could not read conversation", "error", err)
			return nil, err
		}
		conversations = append(conversations, conv)
	}
	if err := rows.Err(); err != nil {
		cl.logger.Error("Could not fetch conversations", "error", err)
		return nil, err
	}
	cl.logger.Debug("Found conversations", "count", len(conversations))
	return conversations, nil
}

func (cl DbClient) SaveConversation(ctx context.Context, conv model.Conversation) (model.Conversation, error) {
	cl.logger.Debug("Saving conversation", "id", conv.Id, "messages", len(conv.Messages))
	conv.Messages = slices.Clone(conv.Messages)

	tx, err := cl.pool.Begin(ctx)
	if err != nil {
		return conv, fmt.Errorf("begin save: %w", err)
	}
	defer tx.Rollback(ctx)

	if conv.Id == "" {
		if err := tx.QueryRow(ctx,
			`
			INSERT INTO conversations (title, title_source, summary)
			     VALUES ($1, NULLIF($2, ''), $3)
			  RETURNING id
			`,
			conv.Title, conv.TitleSource, conv.Summary).Scan(&conv.Id); err != nil {
			cl.logger.Error("Could not create conversation", "title", conv.Title, "error", err)
			return conv, fmt.Errorf("create conversation: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx,
			`
			UPDATE conversations
			   SET title = $1, title_source = NULLIF($2, ''), summary = $3, updated_at = now()
			 WHERE id = $4
			`,
			conv.Title, conv.TitleSource, conv.Summary, conv.Id); err != nil {
			cl.logger.Error("Could not update conversation", "id", conv.Id, "error", err)
			return conv, fmt.Errorf("update conversation: %w", err)
		}
	}

	var storedSeq int
	if err := tx.QueryRow(ctx,
		`
		SELECT COALESCE(MAX(seq), 0)
		  FROM messages
		 WHERE conversation_id = $1
		`,
		conv.Id).Scan(&storedSeq); err != nil {
		cl.logger.Error("Could not read stored seq", "conversation_id", conv.Id, "error", err)
		return conv, fmt.Errorf("read stored seq: %w", err)
	}

	for i, message := range conv.Messages {
		if message.Seq <= storedSeq {
			continue
		}
		id, err := cl.saveMessage(ctx, tx, conv.Id, message)
		if err != nil {
			cl.logger.Error("Could not save message", "seq", message.Seq, "error", err)
			return conv, fmt.Errorf("save message %d: %w", message.Seq, err)
		}
		conv.Messages[i].Id = id
	}

	if err := tx.Commit(ctx); err != nil {
		return conv, fmt.Errorf("commit save: %w", err)
	}
	return conv, nil
}

func (cl DbClient) saveMessage(ctx context.Context, tx pgx.Tx, conversationId string, message model.Message) (string, error) {
	var id string
	if err := tx.QueryRow(ctx,
		`
		INSERT INTO messages (seq, role, content, conversation_id)
		     VALUES ($1, $2, $3, $4)
		  RETURNING id
		`,
		message.Seq, message.Role, message.Content, conversationId).Scan(&id); err != nil {
		return "", err
	}

	if message.Role != "user" && message.Role != "assistant" {
		return id, nil
	}
	chunks := Chunk(message.Content, cl.cfg.ChunkSize, cl.cfg.ChunkOverlap)
	if len(chunks) == 0 {
		return id, nil
	}
	batch := &pgx.Batch{}
	for _, chunk := range chunks {
		hash := sha256.Sum256([]byte(chunk))
		batch.Queue(
			`
			INSERT INTO message_chunks (conversation_id, message_id, content, content_hash)
			     VALUES ($1, $2, $3, $4)
			ON CONFLICT (message_id, content_hash) DO NOTHING
			`,
			conversationId, id, chunk, hex.EncodeToString(hash[:]))
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return "", fmt.Errorf("insert chunks: %w", err)
	}
	return id, nil
}

func Chunk(text string, size int, overlap int) []string {
	if size <= overlap {
		return []string{}
	}
	words := strings.Fields(text)
	var chunks []string
	for start := 0; start < len(words); start += size - overlap {
		end := min(start+size, len(words))
		chunks = append(chunks, strings.Join(words[start:end], " "))
		if end == len(words) {
			return chunks
		}
	}
	return chunks
}
