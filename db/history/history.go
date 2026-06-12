package history

import (
	"log/slog"
	"net"
	"net/url"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/gopsql/pgx"
)

type DbClient struct {
	Cfg config.DbConfig
	Logger slog.Logger
}

type Conversation struct {
	Id       string
	Messages []model.Message
}

func (client DbClient) init() error {
	const createConversationTable = `
	CREATE TABLE IF NOT EXISTS conversations (
		id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	const createMessageTable = `
	CREATE TABLE IF 	NOT EXISTS messages (
		id          	UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		role			TEXT NOT NULL,
		content 		TEXT NOT NULL,
		conversation_id UUID,
		created_at  	TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	const createToolCallTable = `
	CREATE TABLE IF	 NOT EXISTS tool_calls (
		id           UUID PRIMARY KEY,
		type 		 TEXT NOT NULL,
		functionName TEXT NOT NULL,
		functionArgs TEXT NOT NULL,
		message_id 	 UUID,
		created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
	)`

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
				client.Logger.Error("Error while checking table", "table", table.tableName)
				return err
		}
		if exists {
			client.Logger.Debug("Table found", "table", table.tableName)
		} else {
			client.Logger.Debug("Table not found. Creating", "table", table.tableName)
			if _, err := conn.Exec(table.script); err != nil {
				client.Logger.Error("Could not create table", "table", table.tableName)
				return err
			}
		}
	}

	if _, err := conn.Exec(createToolCallTable); err != nil {
		return err
	}
	if _, err := conn.Exec(createToolCallTable); err != nil {
		return err
	}
	return nil
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
