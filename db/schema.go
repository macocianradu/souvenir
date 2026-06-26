package db

const MessageTableScript = ` 
	CREATE TABLE IF NOT EXISTS messages (
		id          	UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		role			TEXT NOT NULL,
		seq             INTEGER NOT NULL,
		content 		TEXT NOT NULL,
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		created_at  	TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at		TIMESTAMPTZ NOT NULL DEFAULT now(),
		tsv tsvector generated always as (to_tsvector('english', content)) stored,
		UNIQUE 			(conversation_id, seq)
	);
	CREATE INDEX on messages using gin (tsv);
	CREATE INDEX on messages (conversation_id, seq);
	`
const ConversationTableScript = `
	CREATE TABLE IF NOT EXISTS conversations (
		id           		UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		title 		 		TEXT,
		title_source 		TEXT,
		summary      		TEXT,
		summary_through_seq int default 0,
		created_at   		TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at			TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
const MessageChunkTableScript = `
	CREATE TABLE IF NOT EXISTS message_chunks (
		id				UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		message_id		UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		content 		TEXT NOT NULL,
		content_hash	TEXT NOT NULL,
		created_at		TIMESTAMPTZ NOT NULL DEFAULT now()
	)`

const CreateVectorTableTemplate = `
	CREATE TABLE IF NOT EXISTS %s (
		chunk_id		UUID PRIMARY KEY REFERENCES message_chunks(id) ON DELETE CASCADE,
		embedding		vector(%d),
		content_hash	text not null
		created_at		TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
