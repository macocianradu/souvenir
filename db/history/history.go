package history

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/gopsql/pgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DbClient struct {
	cfg    config.DbConfig
	logger slog.Logger
	pool   *pgxpool.Pool
}

func Init(ctx context.Context, cfg config.DbConfig) (*DbClient, error) {
	logger := *slog.Default().With("Component", "History")
	const createConversationTable = `
	CREATE TABLE IF NOT EXISTS conversations (
		id           		UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		title 		 		TEXT,
		title_source 		TEXT,
		summary      		TEXT,
		summary_through_seq int default 0
		created_at   		TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	const createMessageTable = `
	CREATE TABLE IF NOT EXISTS messages (
		id          	UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		role			TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		content 		TEXT NOT NULL,
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		created_at  	TIMESTAMPTZ NOT NULL DEFAULT now(),
		tsv tsvector generated always as (to_tsvector('english', content)) stored,
		UNIQUE (conversation_id, seq)
	);
	CREATE INDEX on messages using gin (tsv);
	CREATE INDEX on messages (conversation_id, seq);`
	const createMessageChunkTable = `
	CREATE TABLE IF NOT EXISTS message_chunks (
		id				UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		message_id		UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		content 		TEXT NOT NULL,
		content_hash	TEXT NOT NULL
	)`

	if err := ensureDbExists(cfg, logger); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, cfg.ConnectionString())
	if err != nil {
		logger.ErrorContext(ctx, "Could not create pgxpool")
		return nil, err
	}
	tables := []struct {
		tableName string
		script    string
	}{
		{"conversations", createConversationTable},
		{"messages", createMessageTable},
		{"message_chunks", createMessageChunkTable},
	}

	for _, table := range tables {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS
			(SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public'
			AND table_name =$1)`, table.tableName).Scan(&exists); err != nil {
			logger.Error("Error while checking table", "table", table.tableName, "error", err)
			return nil, err
		}
		if exists {
			logger.Debug("Table found", "table", table.tableName)
		} else {
			logger.Debug("Table not found. Creating", "table", table.tableName)
			if _, err := pool.Exec(ctx, table.script); err != nil {
				logger.Error("Could not create table", "table", table.tableName, "error", err)
				return nil, err
			}
		}
	}

	return &DbClient{
		logger: logger,
		cfg:    cfg,
		pool:   pool,
	}, nil
}

func (cl DbClient) GetConversation(ctx context.Context, id string) (model.Conversation, error) {
	cl.logger.Debug("Searching for conversation", "id", id)
	var conv model.Conversation
	if err := cl.pool.QueryRow(ctx, "SELECT id, name, summary FROM conversations WHERE id = $1", id).Scan(&conv.Id, &conv.Name, &conv.Summary); err != nil {
		cl.logger.Error("Could not fetch conversation", "id", id, "error", err)
		return conv, err
	}
	rows, err := cl.pool.Query(ctx, `SELECT id, role, content, seq
		FROM messages
		WHERE conversation_id = $1 ORDER BY seq`, id)
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
	rows, err := cl.pool.Query(ctx, "SELECT id, name, summary FROM conversations")
	if err != nil {
		cl.logger.Error("Could not fetch conversations", "error", err)
		return []model.Conversation{}, err
	}
	defer rows.Close()
	conversations := []model.Conversation{}
	for rows.Next() {
		var conv model.Conversation
		rows.Scan(&conv.Id, &conv.Name, &conv.Summary)
		conversations = append(conversations, conv)
	}
	cl.logger.Debug("Found conversations", "count", len(conversations))
	return conversations, nil
}

func (cl DbClient) SaveConversation(ctx context.Context, conv model.Conversation) (model.Conversation, error) {
	cl.logger.Debug("Saving conversation", "conversation", conv)
	delta := 0
	if conv.Id == "" {
		if err := cl.pool.QueryRow(ctx, `INSERT INTO conversations(name, summary) VALUES($1, $2) RETURNING id`,
			conv.Name,
			conv.Summary).Scan(&conv.Id); err != nil {
			cl.logger.Error("Could not create conversation", "name", conv.Name, "error", err)
			return conv, err
		}
	}
	if err := cl.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq) + 1, 1) FROM messages WHERE conversation_id = $1`, conv.Id).Scan(&delta); err != nil {
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
		if err := cl.pool.QueryRow(ctx, `SELECT EXISTS
				(SELECT 1 FROM messages
				WHERE id = $1)`, message.Id).Scan(&exists); err != nil {
			cl.logger.Warn("Error while scanning for message", "id", message.Id, "error", err)
			return message.Id, err
		}
	}
	if exists {
		cl.logger.Warn("Insertting message ", "seq", message.Seq)
		if err := cl.pool.QueryRow(ctx, `INSERT INTO messages (seq, role, content, conversation_id) VALUES ($1, $2, $3, $4) RETURNING id`,
			message.Seq, message.Role, message.Content, conversation_id).Scan(&message.Id); err != nil {
			cl.logger.Error("Could not save message", "seq", message.Seq, "error", err)
			return message.Id, errors.New("Could not save message")
		}
	}

	if err := cl.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM message_chunks WHERE id = $1)`,
		message.Id).Scan(&exists); err != nil {
		cl.logger.Warn("Error while scanning for message_chunk", "message id", message.Id, "error", err)
		return message.Id, errors.New("Could not save message chunk")
	}
	if exists {
		cl.logger.Debug("Message chunk already exists. Skipping")
		return message.Id, nil
	}

	if _, err := cl.pool.Exec(ctx, `INSERT INTO message_chunks (conversation_id, message_id, content, content_hash)
		VALUES ($1, $2, $3, $4)`); err != nil {
		cl.logger.Warn("Error while creating message chunk", "message id", message.Id, "error", err)
	}

	return message.Id, nil
}

func (cl DbClient) Close() {
	cl.pool.Close()
}

func ensureDbExists(cfg config.DbConfig, logger slog.Logger) error {
	maintenanceUrl := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   net.JoinHostPort(cfg.Url, cfg.Port),
		Path:   "postgres",
	}
	conn := pgx.MustOpen(maintenanceUrl.String())
	defer conn.Close()

	var exists bool
	if err := conn.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, cfg.DbName).Scan(&exists); err != nil {
		return err
	}
	if exists {
		logger.Debug("Database already exists", "name", cfg.DbName)
		return nil
	}

	logger.Info("Creating database", "name", cfg.DbName)
	_, err := conn.Exec(`CREATE DATABASE "` + cfg.DbName + `"`)
	return err
}
