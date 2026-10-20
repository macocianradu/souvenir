package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/users"
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
	userId, err := users.Id(ctx)
	if err != nil {
		return conv, err
	}
	if err := cl.pool.QueryRow(ctx,
		`
		SELECT id, COALESCE(title, ''), COALESCE(title_source, ''), COALESCE(summary, '')
		  FROM conversations
		 WHERE id = $1 AND user_id = $2
		`, id, userId).Scan(&conv.Id, &conv.Title, &conv.TitleSource, &conv.Summary); err != nil {
		cl.logger.Error("Could not fetch conversation", "id", id, "error", err)
		if errors.Is(err, pgx.ErrNoRows) {
			return conv, fmt.Errorf("conversation %s not found", id)
		}
		return conv, err
	}
	rows, err := cl.pool.Query(ctx,
		`
		  SELECT id, role, content, tool_calls, COALESCE(tool_call_id, ''), seq, created_at
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
		var toolCalls []byte
		if err := rows.Scan(&msg.Id, &msg.Role, &msg.Content, &toolCalls, &msg.ToolCallId, &msg.Seq, &msg.CreatedAt); err != nil {
			cl.logger.Error("Could not read message", "conversation_id", id, "error", err)
			return conv, err
		}
		if toolCalls != nil {
			if err := json.Unmarshal(toolCalls, &msg.ToolCalls); err != nil {
				cl.logger.Error("Could not read tool calls", "conversation_id", id, "seq", msg.Seq, "error", err)
				return conv, err
			}
		}
		conv.Messages = append(conv.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		cl.logger.Error("Could not fetch messages", "conversation_id", id, "error", err)
		return conv, err
	}
	if err := cl.loadImages(ctx, &conv); err != nil {
		cl.logger.Error("Could not fetch images", "conversation_id", id, "error", err)
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

func (cl DbClient) loadImages(ctx context.Context, conv *model.Conversation) error {
	rows, err := cl.pool.Query(ctx,
		`
		  SELECT i.message_id, i.media_type, i.data
		    FROM message_images i
		    JOIN messages m ON m.id = i.message_id
		   WHERE m.conversation_id = $1
		ORDER BY i.message_id, i.position
		`, conv.Id)
	if err != nil {
		return err
	}
	defer rows.Close()
	byMessage := map[string][]model.Image{}
	for rows.Next() {
		var messageId string
		var img model.Image
		if err := rows.Scan(&messageId, &img.MediaType, &img.Data); err != nil {
			return err
		}
		byMessage[messageId] = append(byMessage[messageId], img)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range conv.Messages {
		conv.Messages[i].Images = byMessage[conv.Messages[i].Id]
	}
	return nil
}

func (cl DbClient) DeleteConversation(ctx context.Context, id string) error {
	userId, err := users.Id(ctx)
	if err != nil {
		return err
	}
	tag, err := cl.pool.Exec(ctx, `DELETE FROM conversations WHERE id = $1 AND user_id = $2`, id, userId)
	if err != nil {
		cl.logger.Error("Could not delete conversation", "id", id, "error", err)
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("conversation %s not found", id)
	}
	return nil
}

func (cl DbClient) SetTitle(ctx context.Context, id, title, source string) error {
	userId, err := users.Id(ctx)
	if err != nil {
		return err
	}
	tag, err := cl.pool.Exec(ctx,
		`UPDATE conversations SET title = $1, title_source = NULLIF($2, '') WHERE id = $3 AND user_id = $4`,
		title, source, id, userId)
	if err != nil {
		cl.logger.Error("Could not rename conversation", "id", id, "error", err)
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("conversation %s not found", id)
	}
	return nil
}

func (cl DbClient) ReplaceToolResult(ctx context.Context, conversationId, messageId, prefix, content string) (bool, error) {
	userId, err := users.Id(ctx)
	if err != nil {
		return false, err
	}
	tag, err := cl.pool.Exec(ctx,
		`
		UPDATE messages m
		   SET content = $1
		  FROM conversations c
		 WHERE m.id = $2 AND m.conversation_id = $3 AND m.role = 'tool' AND starts_with(m.content, $4)
		   AND c.id = m.conversation_id AND c.user_id = $5
		`, content, messageId, conversationId, prefix, userId)
	if err != nil {
		cl.logger.Error("Could not replace the tool result", "conversation", conversationId, "message", messageId, "error", err)
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (cl DbClient) SaveContextSummary(ctx context.Context, conversationId string, summary model.ContextSummary) error {
	userId, err := users.Id(ctx)
	if err != nil {
		return err
	}
	tag, err := cl.pool.Exec(ctx,
		`
		INSERT INTO conversation_summaries (conversation_id, through_seq, content)
		     SELECT id, $2, $3 FROM conversations WHERE id = $1 AND user_id = $4
		ON CONFLICT (conversation_id, through_seq) DO UPDATE
		        SET content = EXCLUDED.content
		`, conversationId, summary.ThroughSeq, summary.Content, userId)
	if err != nil {
		cl.logger.Error("Could not save context summary", "conversation_id", conversationId, "error", err)
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("conversation %s not found", conversationId)
	}
	return nil
}

func (cl DbClient) GetConversations(ctx context.Context) ([]model.Conversation, error) {
	cl.logger.Debug("Fetching conversations")
	userId, err := users.Id(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := cl.pool.Query(ctx,
		`
		   SELECT c.id, COALESCE(c.title, ''), COALESCE(c.summary, ''), COALESCE(first.content, ''), c.updated_at
		     FROM conversations c
		LEFT JOIN LATERAL (
		              SELECT content
		                FROM messages m
		               WHERE m.conversation_id = c.id
		            ORDER BY m.seq
		               LIMIT 1
		          ) first ON true
		    WHERE c.user_id = $1
		      AND (COALESCE(c.title, '') <> '' OR first.content IS NOT NULL)
		 ORDER BY c.updated_at DESC
		`, userId)
	if err != nil {
		cl.logger.Error("Could not fetch conversations", "error", err)
		return nil, err
	}
	defer rows.Close()
	conversations := []model.Conversation{}
	for rows.Next() {
		var conv model.Conversation
		if err := rows.Scan(&conv.Id, &conv.Title, &conv.Summary, &conv.Preview, &conv.UpdatedAt); err != nil {
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
	userId, err := users.Id(ctx)
	if err != nil {
		return conv, err
	}

	tx, err := cl.pool.Begin(ctx)
	if err != nil {
		return conv, fmt.Errorf("begin save: %w", err)
	}
	defer tx.Rollback(ctx)

	if conv.Id == "" {
		if err := tx.QueryRow(ctx,
			`
			INSERT INTO conversations (title, title_source, summary, user_id)
			     VALUES ($1, NULLIF($2, ''), $3, $4)
			  RETURNING id
			`,
			conv.Title, conv.TitleSource, conv.Summary, userId).Scan(&conv.Id); err != nil {
			cl.logger.Error("Could not create conversation", "title", conv.Title, "error", err)
			return conv, fmt.Errorf("create conversation: %w", err)
		}
	} else {
		tag, err := tx.Exec(ctx,
			`
			UPDATE conversations
			   SET title = $1, title_source = NULLIF($2, ''), summary = $3, updated_at = now()
			 WHERE id = $4 AND user_id = $5
			`,
			conv.Title, conv.TitleSource, conv.Summary, conv.Id, userId)
		if err != nil {
			cl.logger.Error("Could not update conversation", "id", conv.Id, "error", err)
			return conv, fmt.Errorf("update conversation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return conv, fmt.Errorf("conversation %s not found", conv.Id)
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
		id, createdAt, err := cl.saveMessage(ctx, tx, conv.Id, message)
		if err != nil {
			cl.logger.Error("Could not save message", "seq", message.Seq, "error", err)
			return conv, fmt.Errorf("save message %d: %w", message.Seq, err)
		}
		conv.Messages[i].Id = id
		conv.Messages[i].CreatedAt = createdAt
	}

	if err := tx.Commit(ctx); err != nil {
		return conv, fmt.Errorf("commit save: %w", err)
	}
	return conv, nil
}

func (cl DbClient) saveMessage(ctx context.Context, tx pgx.Tx, conversationId string, message model.Message) (string, time.Time, error) {
	var id string
	var createdAt time.Time
	var toolCalls []byte
	if len(message.ToolCalls) > 0 {
		var err error
		if toolCalls, err = json.Marshal(message.ToolCalls); err != nil {
			return "", time.Time{}, fmt.Errorf("encode tool calls: %w", err)
		}
	}
	if err := tx.QueryRow(ctx,
		`
		INSERT INTO messages (seq, role, content, tool_calls, tool_call_id, conversation_id)
		     VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
		  RETURNING id, created_at
		`,
		message.Seq, message.Role, message.Content, toolCalls, message.ToolCallId, conversationId).Scan(&id, &createdAt); err != nil {
		return "", time.Time{}, err
	}

	for i, img := range message.Images {
		if _, err := tx.Exec(ctx,
			`
			INSERT INTO message_images (message_id, position, media_type, data)
			     VALUES ($1, $2, $3, $4)
			`, id, i, img.MediaType, img.Data); err != nil {
			return "", time.Time{}, fmt.Errorf("insert image: %w", err)
		}
	}

	if message.Role != "user" && message.Role != "assistant" {
		return id, createdAt, nil
	}
	chunks := Chunk(message.Content, cl.cfg.ChunkSize, cl.cfg.ChunkOverlap)
	if len(chunks) == 0 {
		return id, createdAt, nil
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
		return "", time.Time{}, fmt.Errorf("insert chunks: %w", err)
	}
	return id, createdAt, nil
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
