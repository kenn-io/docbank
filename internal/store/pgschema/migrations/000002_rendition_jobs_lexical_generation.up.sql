-- Lexical collection checks each superseded generation against the jobs that
-- name it; without this every check scans the whole job table.
CREATE INDEX rendition_jobs_lexical_generation
    ON {{schema}}.rendition_jobs(lexical_generation_id);
