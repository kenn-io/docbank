package backupapp

import (
	"context"
	"fmt"

	"go.kenn.io/docbank/internal/store"
)

// BackupCoverage is the versioned logical inventory proved before restore publication.
// Byte counts describe distinct verified content, independent of compression.
type BackupCoverage struct {
	Version  int              `json:"version"`
	Included CoverageIncluded `json:"included"`
	Excluded []string         `json:"excluded"`
}

// CoverageIncluded counts persisted owner decisions and their content.
// Adding fields or changing counting queries requires a new coverage version because restore compares recomputed stats byte for byte.
type CoverageIncluded struct {
	PhotoAssets    int64 `json:"photo_assets"`
	PhotoFiles     int64 `json:"photo_files"`
	PhotoBlobBytes int64 `json:"photo_blob_bytes"`
	Albums         int64 `json:"albums"`
	AlbumMembers   int64 `json:"album_members"`
	HiddenPhotos   int64 `json:"hidden_photos"`
	// TrashedPhotos counts assets with at least one trashed managed file.
	TrashedPhotos       int64 `json:"trashed_photos"`
	CheckoutDefinitions int64 `json:"checkout_definitions"`
	CheckoutEntries     int64 `json:"checkout_entries"`
	RetainedReceipts    int64 `json:"retained_receipts"`
}

func computeCoverage(ctx context.Context, q rowQuerier) (*BackupCoverage, error) {
	c := &BackupCoverage{Version: 1, Excluded: []string{
		"browser_sessions", "hidden_unlock_sessions", "storage_operations",
		"checkout_destination_bindings", "search_indexes", "photo_timeline", "photo_quality_signals", "photo_technical_metadata", "vector_indexes", "photo_suggestion_text",
	}}
	counts := []struct {
		dst   *int64
		query string
	}{
		{&c.Included.PhotoAssets, `SELECT COUNT(*) FROM photo_assets`},
		{&c.Included.PhotoFiles, `SELECT COUNT(*) FROM photo_files`},
		{&c.Included.PhotoBlobBytes, storePhotoBytesQuery},
		{&c.Included.Albums, `SELECT COUNT(*) FROM photo_sets`},
		{&c.Included.AlbumMembers, `SELECT COUNT(*) FROM photo_set_members`},
		{&c.Included.HiddenPhotos, `SELECT COUNT(*) FROM photo_assets WHERE hidden_at IS NOT NULL`},
		{&c.Included.TrashedPhotos, `SELECT COUNT(*) FROM photo_assets a WHERE EXISTS (SELECT 1 FROM photo_files f JOIN nodes n ON n.id=f.node_id WHERE f.asset_id=a.asset_id AND n.trashed_at IS NOT NULL)`},
		{&c.Included.CheckoutDefinitions, `SELECT COUNT(*) FROM photo_checkouts`},
		{&c.Included.CheckoutEntries, `SELECT COUNT(*) FROM photo_checkout_entries`},
		{&c.Included.RetainedReceipts, `SELECT COUNT(*) FROM photo_change_receipts`},
	}
	for _, count := range counts {
		if err := q.QueryRowContext(ctx, count.query).Scan(count.dst); err != nil {
			return nil, fmt.Errorf("backupapp: coverage query: %w", err)
		}
	}
	return c, nil
}

var storePhotoBytesQuery = store.BackupBlobAuthorityCTE() + `SELECT COALESCE(SUM(b.size),0) FROM blobs b JOIN backup_authorized_blobs a ON a.hash=b.hash WHERE b.hash IN (
 SELECT v.blob_hash FROM content_versions v JOIN photo_files f ON f.node_id=v.node_id
 UNION SELECT blob_hash FROM photo_checkout_entries)`
