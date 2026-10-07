package db

// VectorTableTemplate creates the vector table for one embedding model.
// Arguments: table name, vector dimension. The table name must already be a
// safe identifier.
const VectorTableTemplate = `
	CREATE TABLE IF NOT EXISTS %s (
		chunk_id     UUID PRIMARY KEY REFERENCES message_chunks(id) ON DELETE CASCADE,
		embedding    vector(%d),
		content_hash TEXT NOT NULL,
		created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
