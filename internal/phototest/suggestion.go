// Package phototest seeds synthetic photo proposals for store, API, and browser tests.
package phototest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/docbank/document"
)

func SeedSuggestion(ctx context.Context, db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, versionID string, description document.PhotoDescription) (string, error) {
	raw, err := json.Marshal(description)
	if err != nil {
		return "", err
	}
	id := uuid.New().String()
	_, err = db.ExecContext(ctx, `INSERT INTO photo_suggestions(suggestion_id,content_version_id,producer_fingerprint,kind,value_json,response,revision,created_at) VALUES(?,?,?,'photo_description',?,'pending',1,?)`, id, versionID, strings.Repeat("a", 64), string(raw), time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	return id, err
}
