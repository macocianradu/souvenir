package history

import (
	"net"
	"net/url"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/gopsql/pgx"
)

type DbClient struct {
	Cfg config.DbConfig
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
		role			TEXT NOT NULL
		content 		TEXT NOT NULL
		conversation_id UUID
		created_at  	TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	const createToolCallTable = `
	CREATE TABLE IF	 NOT EXISTS tool_calls (
		id           UUID PRIMARY KEY,
		type 		 TEXT NOT NULL
		functionName TEXT NOT NULL
		functionArgs TEXT NOT NULL
		message_id 	 UUID
		created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
	)`

	conn := pgx.MustOpen(client.connectionString())
	defer conn.Close()
	
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
