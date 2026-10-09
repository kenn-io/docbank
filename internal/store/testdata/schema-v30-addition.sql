
-- Derived pixels rebuild locally and are omitted from metadata backups.
-- Unavailable rows record previews whose verified bytes cannot be measured.
-- Go validates state and score presence on every write and read.
CREATE TABLE IF NOT EXISTS photo_quality_signals (
 content_version_id TEXT NOT NULL REFERENCES content_versions(version_id) ON DELETE CASCADE,
 evaluator_fingerprint TEXT NOT NULL,
 state TEXT NOT NULL,
 focus REAL, blur REAL, brightness REAL,
 color_red REAL, color_green REAL, color_blue REAL,
 framing REAL, aesthetics REAL,
 PRIMARY KEY(content_version_id,evaluator_fingerprint)
);
