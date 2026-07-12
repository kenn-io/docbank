// Package backupapp adapts docbank's logical schema and mixed blob store to
// Kit's application-neutral backup engine.
package backupapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"
)

// Stats is the representation-neutral fidelity payload recorded in each
// snapshot. Physical pack rows are deliberately excluded: restore may choose a
// different loose/packed representation while preserving the same archive.
type Stats struct {
	Nodes         int64 `json:"nodes"`
	Files         int64 `json:"files"`
	Directories   int64 `json:"directories"`
	TrashedNodes  int64 `json:"trashed_nodes"`
	Blobs         int64 `json:"blobs"`
	BlobBytes     int64 `json:"blob_bytes"`
	NodeVersions  int64 `json:"node_versions"`
	Ingests       int64 `json:"ingests"`
	Provenance    int64 `json:"provenance"`
	Tags          int64 `json:"tags"`
	NodeTags      int64 `json:"node_tags"`
	ExtractedText int64 `json:"extracted_text"`
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func computeStats(ctx context.Context, q rowQuerier) (Stats, error) {
	var stats Stats
	counts := []struct {
		dst   *int64
		query string
	}{
		{&stats.Nodes, `SELECT COUNT(*) FROM nodes`},
		{&stats.Files, `SELECT COUNT(*) FROM nodes WHERE kind = 'file'`},
		{&stats.Directories, `SELECT COUNT(*) FROM nodes WHERE kind = 'dir'`},
		{&stats.TrashedNodes, `SELECT COUNT(*) FROM nodes WHERE trashed_at IS NOT NULL`},
		{&stats.NodeVersions, `SELECT COUNT(*) FROM node_versions`},
		{&stats.Ingests, `SELECT COUNT(*) FROM ingests`},
		{&stats.Provenance, `SELECT COUNT(*) FROM provenance`},
		{&stats.Tags, `SELECT COUNT(*) FROM tags`},
		{&stats.NodeTags, `SELECT COUNT(*) FROM node_tags`},
		{&stats.ExtractedText, `SELECT COUNT(*) FROM extracted_text`},
	}
	for _, count := range counts {
		if err := q.QueryRowContext(ctx, count.query).Scan(count.dst); err != nil {
			return Stats{}, fmt.Errorf("backupapp: stats query %q: %w", count.query, err)
		}
	}
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs`,
	).Scan(&stats.Blobs, &stats.BlobBytes); err != nil {
		return Stats{}, fmt.Errorf("backupapp: blob stats: %w", err)
	}
	return stats, nil
}

// App implements backup.App for docbank.
type App struct{ version string }

var _ backup.App = (*App)(nil)

func New(version string) *App { return &App{version: version} }

func (a *App) FrozenView(session *backup.FrozenSession) backup.FrozenView {
	return &frozenView{tx: session.Tx()}
}

func (a *App) DBFileName() string        { return "docbank.db" }
func (a *App) ContentDirName() string    { return "blobs" }
func (a *App) PackFileExtension() string { return packstore.PackExt }
func (a *App) Version() string           { return a.version }
func (a *App) ExcludedPaths() []string {
	return []string{"config.toml", "logs/", "vault.lock", "launch.lock", "daemon.*.json", "blobs/tmp/"}
}

type frozenView struct{ tx *sql.Tx }

func (v *frozenView) ContentInfo(ctx context.Context) (*backup.ContentInfo, error) {
	rows, err := v.tx.QueryContext(ctx, `SELECT hash, size FROM blobs ORDER BY hash`)
	if err != nil {
		return nil, fmt.Errorf("backupapp: listing frozen blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []backup.ContentRef
	for rows.Next() {
		var ref backup.ContentRef
		if err := rows.Scan(&ref.Hash, &ref.Size); err != nil {
			return nil, fmt.Errorf("backupapp: scanning frozen blob: %w", err)
		}
		if _, err := packstore.ParseHash(ref.Hash); err != nil {
			return nil, fmt.Errorf("backupapp: frozen blob hash %q: %w", ref.Hash, err)
		}
		if ref.Size < 0 {
			return nil, fmt.Errorf("backupapp: frozen blob %s has negative size %d", ref.Hash, ref.Size)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backupapp: frozen blob rows: %w", err)
	}
	return &backup.ContentInfo{Refs: refs, Rows: int64(len(refs))}, nil
}

func (v *frozenView) Stats(ctx context.Context) (json.RawMessage, error) {
	stats, err := computeStats(ctx, v.tx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(stats)
}

func (a *App) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	return restoredContentPaths(ctx, db, false)
}

func restoredContentPaths(ctx context.Context, db *sql.DB, allowPackedRestore bool) (map[string][]string, error) {
	if !allowPackedRestore {
		var packed bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM blob_pack_index)
			    OR EXISTS(SELECT 1 FROM blob_packs)`).Scan(&packed); err != nil {
			return nil, fmt.Errorf("backupapp: checking restored pack authority: %w", err)
		}
		if packed {
			return nil, errors.New("backupapp: snapshot contains packed blob authority; use backupapp.Restore")
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT hash FROM blobs ORDER BY hash`)
	if err != nil {
		return nil, fmt.Errorf("backupapp: listing restored blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	paths := make(map[string][]string)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("backupapp: scanning restored blob: %w", err)
		}
		parsed, err := packstore.ParseHash(hash)
		if err != nil {
			return nil, fmt.Errorf("backupapp: restored blob hash %q: %w", hash, err)
		}
		paths[hash] = []string{parsed.String()[:2] + "/" + parsed.String()}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backupapp: restored blob rows: %w", err)
	}
	return paths, nil
}

func (a *App) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	stats, err := computeStats(ctx, db)
	if err != nil {
		return nil, err
	}
	return json.Marshal(stats)
}

func (a *App) CheckManifest(manifest *backup.Manifest) []string {
	stats, err := ParseStats(manifest.Stats)
	if err != nil {
		return []string{fmt.Sprintf("manifest stats unreadable: %v", err)}
	}
	var problems []string
	if stats.Blobs != manifest.Attachments.Blobs {
		problems = append(problems, fmt.Sprintf("stats.blobs %d != attachments.blobs %d",
			stats.Blobs, manifest.Attachments.Blobs))
	}
	if stats.BlobBytes != manifest.Attachments.BlobBytes {
		problems = append(problems, fmt.Sprintf("stats.blob_bytes %d != attachments.blob_bytes %d",
			stats.BlobBytes, manifest.Attachments.BlobBytes))
	}
	if stats.Blobs != manifest.Attachments.Rows {
		problems = append(problems, fmt.Sprintf("stats.blobs %d != attachments.rows %d",
			stats.Blobs, manifest.Attachments.Rows))
	}
	return problems
}

func ParseStats(raw json.RawMessage) (Stats, error) {
	var stats Stats
	if err := json.Unmarshal(raw, &stats); err != nil {
		return Stats{}, fmt.Errorf("backupapp: parsing manifest stats: %w", err)
	}
	return stats, nil
}

// BlobStreamer is the verified mixed loose/packed read surface backup capture
// needs. Kit must consume the returned stream through verified EOF before its
// bytes can enter an archive.
type BlobStreamer interface {
	OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error)
}

// ContentSource adapts docbank's catalog-authorized mixed store to backup.
type ContentSource struct{ blobs BlobStreamer }

var _ backup.ContentSource = (*ContentSource)(nil)

func NewContentSource(blobs BlobStreamer) *ContentSource { return &ContentSource{blobs: blobs} }

func (s *ContentSource) Open(ctx context.Context, ref backup.ContentRef) (io.ReadCloser, error) {
	if s == nil || s.blobs == nil {
		return nil, errors.New("backupapp: content source has no blob store")
	}
	reader, _, err := s.blobs.OpenStreamContext(ctx, ref.Hash)
	if err != nil {
		return nil, fmt.Errorf("backupapp: opening content %s: %w", ref.Hash, err)
	}
	return reader, nil
}
