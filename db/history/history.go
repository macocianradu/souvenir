package history

import (
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/gopsql/db"
	"github.com/gopsql/pgx"
)

type DbClient struct {
	Cfg    config.DbConfig
	Logger slog.Logger
}

func (client DbClient) Init() error {
	const createConversationTable = `
	CREATE TABLE IF NOT EXISTS conversations (
		id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name        TEXT,
		summary     TEXT,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	const createMessageTable = `
	CREATE TABLE IF 	NOT EXISTS messages (
		id          	UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		role			TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		content 		TEXT NOT NULL,
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		created_at  	TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (conversation_id, seq)
	)`
	const createToolCallTable = `
	CREATE TABLE IF	 NOT EXISTS tool_calls (
		id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		type 		 TEXT NOT NULL,
		functionName TEXT NOT NULL,
		functionArgs TEXT NOT NULL,
		message_id 	 UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
	)`

	if err := client.ensureDbExists(); err != nil {
		return err
	}
	conn := pgx.MustOpen(client.connectionString())
	defer conn.Close()
	tables := []struct {
		tableName string
		script    string
	}{
		{"conversations", createConversationTable},
		{"messages", createMessageTable},
		{"tool_calls", createToolCallTable},
	}

	for _, table := range tables {
		var exists bool
		if err := conn.QueryRow(`SELECT EXISTS
			(SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public'
			AND table_name =$1)`, table.tableName).Scan(&exists); err != nil {
			client.Logger.Error("Error while checking table", "table", table.tableName, "error", err)
			return err
		}
		if exists {
			client.Logger.Debug("Table found", "table", table.tableName)
		} else {
			client.Logger.Debug("Table not found. Creating", "table", table.tableName)
			if _, err := conn.Exec(table.script); err != nil {
				client.Logger.Error("Could not create table", "table", table.tableName, "error", err)
				return err
			}
		}
	}

	return nil
}

func (client DbClient) GetConversation(id string) (model.Conversation, error) {
	conn := pgx.MustOpen(client.connectionString())
	defer conn.Close()

	client.Logger.Debug("Searching for conversation", "id", id)
	var conv model.Conversation
	if err := conn.QueryRow("SELECT id, name, summary FROM conversations WHERE id = $1", id).Scan(&conv.Id, &conv.Name, &conv.Summary); err != nil {
		client.Logger.Error("Could not fetch conversation", "id", id, "error", err)
		return conv, err
	}
	rows, err := conn.Query(`SELECT t.id, t.type, t.functionName, t.functionArgs, m.id, m.role, m.content, m.seq
		FROM messages m 
		LEFT JOIN tool_calls t
		ON m.id = t.message_id
		WHERE m.conversation_id = $1 ORDER BY m.seq`, id)
	if err != nil {
		client.Logger.Error("Could not fetch messages", "conversation_id", id, "error", err)
		return conv, err
	}
	defer rows.Close()
	var messages []model.Message
	for rows.Next() {
		var (
			tcId, tcType, tcName, tcArgs *string
			msgId, role, content         string
			seq                          int
		)
		err = rows.Scan(&tcId, &tcType, &tcName, &tcArgs, &msgId, &role, &content, &seq)
		if err != nil {
			client.Logger.Error("Could not get message", "error", err)
		}
		if len(messages) == 0 || messages[len(messages) - 1].Id != msgId {
			messages = append(messages, model.Message{
				Id: msgId, Role: role, Content: content, Seq: seq,
			})
		}
		if tcId != nil {
			deref := func(s *string) string {
				if s == nil {
					return ""
				}
				return *s
			}
			messages[len(messages) - 1].ToolCalls = append(messages[len(messages)-1].ToolCalls, model.ToolCall{
				Id: *tcId,
				Type: deref(tcType),
				FunctionName: deref(tcName),
				FunctionArguments: deref(tcArgs),
			})
		}
	}
	conv.Messages = messages
	client.Logger.Debug("Found conversation", "conversation", conv)
	return conv, nil
}

func (client DbClient) GetConversations() ([]model.Conversation, error) {
	client.Logger.Debug("Fetching conversations")
	conn := pgx.MustOpen(client.connectionString())
	defer conn.Close()
	rows, err := conn.Query("SELECT id, name, summary FROM conversations")
	if err != nil {
		client.Logger.Error("Could not fetch conversations", "error", err)
		return []model.Conversation{}, err
	}
	defer rows.Close()
	conversations := []model.Conversation{}
	for rows.Next() {
		var conv model.Conversation
		rows.Scan(&conv.Id, &conv.Name, &conv.Summary)
		conversations = append(conversations, conv)
	}
	client.Logger.Debug("Found conversations", "count", len(conversations))
	return conversations, nil
}

func (client DbClient) SaveConversation(conv model.Conversation) (model.Conversation, error) {
	client.Logger.Debug("Saving conversation", "conversation", conv)
	conn := pgx.MustOpen(client.connectionString())
	defer conn.Close()
	delta := 0
	if conv.Id == "" {
		if err := conn.QueryRow(`INSERT INTO conversations(name, summary) VALUES($1, $2) RETURNING id`,
			conv.Name,
			conv.Summary).Scan(&conv.Id); err != nil {
			client.Logger.Error("Could not create conversation", "name", conv.Name, "error", err)
			return conv, err
		}
	}
	if err := conn.QueryRow(`SELECT COALESCE(MAX(seq) + 1, 1) FROM messages WHERE conversation_id = $1`, conv.Id).Scan(&delta); err != nil {
		client.Logger.Warn("Could not get delta", "conversation_id", conv.Id, "error", err)
		return conv, errors.New("Could not get delta")
	}

	client.Logger.Debug("Found delta.", "delta", delta, "Unsaved messages", len(conv.Messages[delta - 1:]))

	if delta >= len(conv.Messages) {
		return conv, nil
	}
	for _, message := range conv.Messages[delta - 1:] {
		id, err := client.saveMessage(conv.Id, message, conn)
		if err != nil {
			client.Logger.Error("Could not save message", "message_id", message.Id, "seq", message.Seq, "error", err)
			return conv, errors.New("Could not save message")
		}
		message.Id = id
	}
	return conv, nil
}

func (client DbClient) saveMessage(conversation_id string, message model.Message, conn db.DB) (string, error) {
	var exists bool
	if message.Id != "" {
		if err := conn.QueryRow(`SELECT EXISTS
				(SELECT 1 FROM messages
				WHERE id = $1)`, message.Id).Scan(&exists); err != nil {
			client.Logger.Warn("Error while scanning for message", "id", message.Id, "error", err)
			return message.Id, err
		}
	}
	if exists {
		client.Logger.Warn("Message found. Skipping", "id", message.Id)
		return message.Id, nil
	}

	if err := conn.QueryRow(`INSERT INTO messages (seq, role, content, conversation_id) VALUES ($1, $2, $3, $4) RETURNING id`,
		message.Seq, message.Role, message.Content, conversation_id).Scan(&message.Id); err != nil {
		client.Logger.Error("Could not save message", "seq", message.Seq, "error", err)
		return message.Id, errors.New("Could not save message")
	}

	placeholder := 0
	var placeholders strings.Builder
	args := []any{}
	const cols = 4
	for _, call := range message.ToolCalls {
		placeholders.WriteString("($")
		placeholders.WriteString(strconv.Itoa(placeholder + 1))
		placeholders.WriteString(",$")
		placeholders.WriteString(strconv.Itoa(placeholder + 2))
		placeholders.WriteString(",$")
		placeholders.WriteString(strconv.Itoa(placeholder + 3))
		placeholders.WriteString(",$")
		placeholders.WriteString(strconv.Itoa(placeholder + 4))
		placeholders.WriteString(")")
		args = append(args, call.Type, call.FunctionName, call.FunctionArguments, message.Id)
		placeholder += cols
	}
	if _, err := conn.Exec(`INSERT INTO tool_calls(type, functionName, functionArgs, message_id) VALUES `+placeholders.String(),
		args...); err != nil && placeholder > 0 {
		client.Logger.Warn("Could not insert tool_calls", "message_id", message.Id, "error", err)
	}
	return message.Id, nil
}

func (client DbClient) ensureDbExists() error {
	maintenanceUrl := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(client.Cfg.User, client.Cfg.Password),
		Host:   net.JoinHostPort(client.Cfg.Url, client.Cfg.Port),
		Path:   "postgres",
	}
	conn := pgx.MustOpen(maintenanceUrl.String())
	defer conn.Close()

	var exists bool
	if err := conn.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, client.Cfg.DbName).Scan(&exists); err != nil {
		return err
	}
	if exists {
		client.Logger.Debug("Database already exists", "name", client.Cfg.DbName)
		return nil
	}

	client.Logger.Info("Creating database", "name", client.Cfg.DbName)
	_, err := conn.Exec(`CREATE DATABASE "` + client.Cfg.DbName + `"`)
	return err
}

func (client DbClient) connectionString() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(client.Cfg.User, client.Cfg.Password),
		Host:   net.JoinHostPort(client.Cfg.Url, client.Cfg.Port),
		Path:   client.Cfg.DbName,
	}
	return u.String()
}
