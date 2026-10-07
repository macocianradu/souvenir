package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
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
		SELECT id, title, summary
		  FROM conversations
		 WHERE id = $1
		`, id).Scan(&conv.Id, &conv.Title, &conv.Summary); err != nil {
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
	var messages []model.Message
	for rows.Next() {
		var (
			msgId, role, content string
			seq                  int
		)
		err = rows.Scan(&msgId, &role, &content, &seq)
		if err != nil {
			cl.logger.Error("Could not get message", "error", err)
		}
		if len(messages) == 0 || messages[len(messages)-1].Id != msgId {
			messages = append(messages, model.Message{
				Id: msgId, Role: role, Content: content, Seq: seq,
			})
		}
	}
	conv.Messages = messages
	cl.logger.Debug("Found conversation", "conversation", conv)
	return conv, nil
}

func (cl DbClient) GetConversations(ctx context.Context) ([]model.Conversation, error) {
	cl.logger.Debug("Fetching conversations")
	rows, err := cl.pool.Query(ctx,
		`
		SELECT id, title, summary
		  FROM conversations
		`)
	if err != nil {
		cl.logger.Error("Could not fetch conversations", "error", err)
		return []model.Conversation{}, err
	}
	defer rows.Close()
	conversations := []model.Conversation{}
	for rows.Next() {
		var conv model.Conversation
		rows.Scan(&conv.Id, &conv.Title, &conv.Summary)
		conversations = append(conversations, conv)
	}
	cl.logger.Debug("Found conversations", "count", len(conversations))
	return conversations, nil
}

func (cl DbClient) SaveConversation(ctx context.Context, conv model.Conversation) (model.Conversation, error) {
	cl.logger.Debug("Saving conversation", "conversation", conv)
	delta := 0
	if conv.Id == "" {
		if err := cl.pool.QueryRow(ctx,
			`
			INSERT INTO conversations(title, summary)
			     VALUES ($1, $2)
			  RETURNING id
			`,
			conv.Title, conv.Summary).Scan(&conv.Id); err != nil {
			cl.logger.Error("Could not create conversation", "title", conv.Title, "error", err)
			return conv, err
		}
	} else {
		if _, err := cl.pool.Exec(ctx,
			`
			UPDATE conversations
			   SET title = $1, summary = $2, updated_at = now()
			 WHERE id = $3`, conv.Title, conv.Summary, conv.Id); err != nil {
			cl.logger.Error("Could not update conversation", "id", conv.Id, "error", err)
			return conv, err
		}
	}
	if err := cl.pool.QueryRow(ctx, 
		`
		SELECT COALESCE(MAX(seq) + 1, 1)
		  FROM messages
		 WHERE conversation_id = $1
		`,
		conv.Id).Scan(&delta); err != nil {
		cl.logger.Warn("Could not get delta", "conversation_id", conv.Id, "error", err)
		return conv, errors.New("Could not get delta")
	}

	cl.logger.Debug("Found delta.", "delta", delta, "Unsaved messages", len(conv.Messages[delta-1:]))

	if delta >= len(conv.Messages) {
		return conv, nil
	}
	for _, message := range conv.Messages[delta-1:] {
		id, err := cl.saveMessage(ctx, conv.Id, message)
		if err != nil {
			cl.logger.Error("Could not save message", "message_id", message.Id, "seq", message.Seq, "error", err)
			return conv, errors.New("Could not save message")
		}
		message.Id = id
	}
	return conv, nil
}

func (cl DbClient) saveMessage(ctx context.Context, conversation_id string, message model.Message) (string, error) {
	var exists bool
	if message.Id != "" {
		if err := cl.pool.QueryRow(ctx,
			`
			SELECT EXISTS (
				SELECT 1
			  	  FROM messages
				 WHERE id = $1
			)
			`,
			message.Id).Scan(&exists); err != nil {
			cl.logger.Warn("Error while scanning for message", "id", message.Id, "error", err)
			return message.Id, err
		}
	}
	if !exists {
		cl.logger.Debug("Insertting message ", "seq", message.Seq)
		if err := cl.pool.QueryRow(ctx,
			`
			INSERT INTO messages (seq, role, content, conversation_id)
			     VALUES ($1, $2, $3, $4)
			  RETURNING id
			`,
			message.Seq, message.Role, message.Content, conversation_id).Scan(&message.Id); err != nil {
			cl.logger.Error("Could not save message", "seq", message.Seq, "error", err)
			return message.Id, errors.New("Could not save message")
		}
	}

	chunks := Chunk(message.Content, cl.cfg.ChunkSize, cl.cfg.ChunkOverlap)
	for _, chunk := range chunks {
		hash := sha256.Sum256([]byte(chunk))

		if err := cl.pool.QueryRow(ctx,
			`
			SELECT EXISTS (
				SELECT 1
				  FROM message_chunks
				 WHERE message_id = $1
				   AND content_hash = $2
			)
			`,
			message.Id, hex.EncodeToString(hash[:])).Scan(&exists); err != nil {
			cl.logger.Warn("Error while scanning for message_chunk", "message id", message.Id, "error", err)
			return message.Id, errors.New("Could not save message chunk")
		}
		if exists {
			cl.logger.Debug("Message chunk already exists. Skipping")
			continue
		}

		if _, err := cl.pool.Exec(ctx, 
			`
			INSERT INTO message_chunks (conversation_id, message_id, content, content_hash)
				 VALUES ($1, $2, $3, $4);
			UPDATE messages
			   SET updated_at = now()
			 WHERE id = $5
			`,
			conversation_id, message.Id, chunk, hex.EncodeToString(hash[:]), message.Id); err != nil {
			cl.logger.Warn("Error while creating message chunk", "message id", message.Id, "error", err)
		}
	}

	return message.Id, nil
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
