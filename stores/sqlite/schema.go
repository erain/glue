package sqlite

// schemaSQL is executed once per Store on Open. Every statement is
// idempotent (CREATE … IF NOT EXISTS). Schema is the contract once
// shipped; future changes must be versioned migrations off the
// schema_version table.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY
);
INSERT OR IGNORE INTO schema_version (version) VALUES (1);

CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT PRIMARY KEY,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    metadata_json TEXT
);

CREATE TABLE IF NOT EXISTS messages (
    rowid        INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL,
    ord          INTEGER NOT NULL,
    role         TEXT NOT NULL,
    content_text TEXT NOT NULL,
    content_json TEXT NOT NULL,
    ts           INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    UNIQUE (session_id, ord)
);

CREATE INDEX IF NOT EXISTS messages_session_idx ON messages(session_id);
CREATE INDEX IF NOT EXISTS messages_ts_idx      ON messages(ts);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content_text,
    content='messages',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content_text) VALUES (new.rowid, new.content_text);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content_text) VALUES ('delete', old.rowid, old.content_text);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content_text) VALUES ('delete', old.rowid, old.content_text);
    INSERT INTO messages_fts(rowid, content_text)                 VALUES (new.rowid, new.content_text);
END;
`

// SchemaVersion is the on-disk schema version this build writes and
// expects to read. Exposed for tests and operators.
const SchemaVersion = 1
