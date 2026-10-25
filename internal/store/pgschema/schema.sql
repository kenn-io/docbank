-- The PostgreSQL identity follows the same logical vault through export and
-- restore. This version is independent of SQLite's physical schema version.
CREATE TABLE {{schema}}.vault_metadata (
    singleton      SMALLINT PRIMARY KEY,
    vault_uid      UUID NOT NULL UNIQUE,
    schema_version INTEGER NOT NULL
);
