package a

const schema = `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT NOT NULL CHECK(status IN ('queued','running','done')) DEFAULT 'queued', retries INTEGER CHECK (retries >= 0));` // want "CHECK constraint on status locks" "CHECK constraint on retries locks"

var migration = "ALTER TABLE jobs ADD COLUMN kind TEXT CHECK (kind = 'a' OR kind = 'b')" // want "CHECK constraint on kind locks"

var pg = `CREATE TYPE job_status AS ENUM ('queued', 'done');` // want "enum type job_status locks its allowed values"

var fine = "CREATE TABLE ok (name TEXT NOT NULL, UNIQUE (name)) -- CHECK (length(name) > 0) moved to the app"

var prose = "Please CHECK (the generated output)"
