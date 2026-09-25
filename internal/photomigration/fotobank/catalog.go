package fotobank

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/docbank/internal/photomigration"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func readCatalog(ctx context.Context, db *sql.DB, catalog, vaultRoot string, driver docsqlite.Driver) (photomigration.Report, []photomigration.MapEntry, error) {
	var version int64
	var fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return photomigration.Report{}, nil, fmt.Errorf("reading Fotobank schema version: %w", err)
	}
	layouts, err := catalogLayout(ctx, db)
	if err != nil {
		return photomigration.Report{}, nil, err
	}
	fingerprint = layoutFingerprint(layouts)
	var report photomigration.Report
	report.Source = photomigration.Source{Kind: photomigration.SourceInstall, Identity: fileIdentity(catalog)}
	report.Schema.CatalogVersion = version
	report.Schema.CatalogFingerprint = fingerprint
	queries := map[string]*int64{
		"owners": &report.Counts.Owners, "assets": &report.Counts.Assets,
		"media_files": &report.Counts.Files, "albums": &report.Counts.Albums,
		"scopes": &report.Counts.Shares, "checkouts": &report.Counts.Checkouts,
		"ai_results": &report.Counts.AIResults, "auth_hidden_credential": &report.Counts.HiddenSetup,
	}
	for table, dst := range queries {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(dst); err != nil {
			return photomigration.Report{}, nil, fmt.Errorf("counting Fotobank %s: %w", table, err)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM media_files`).Scan(&report.Counts.Bytes); err != nil {
		return photomigration.Report{}, nil, fmt.Errorf("counting Fotobank file bytes: %w", err)
	}
	report.Capacity.SourceBytes = report.Counts.Bytes
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM (SELECT sha256, MAX(size) AS size FROM media_files WHERE sha256 IS NOT NULL GROUP BY sha256)`).Scan(&report.Capacity.UniqueBlobBytes); err != nil {
		return photomigration.Report{}, nil, fmt.Errorf("counting Fotobank unique bytes: %w", err)
	}
	report.Capacity.MinimumContentBytes = report.Capacity.UniqueBlobBytes
	rows, err := db.QueryContext(ctx, `SELECT id, fingerprint, state FROM embedding_generations ORDER BY id`)
	if err != nil {
		return photomigration.Report{}, nil, fmt.Errorf("reading embedding generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var v photomigration.VectorGeneration
		if err := rows.Scan(&v.ID, &v.Fingerprint, &v.State); err != nil {
			return photomigration.Report{}, nil, err
		}
		v.Rebuildable = true
		report.Vectors = append(report.Vectors, v)
	}
	if err := rows.Err(); err != nil {
		return photomigration.Report{}, nil, err
	}
	if err := rows.Close(); err != nil {
		return photomigration.Report{}, nil, err
	}
	ownerRows, err := db.QueryContext(ctx, `SELECT hub,user_id,storage_key FROM owners ORDER BY hub,user_id,storage_key`)
	if err != nil {
		return photomigration.Report{}, nil, err
	}
	defer func() { _ = ownerRows.Close() }()
	var entries []photomigration.MapEntry
	for ownerRows.Next() {
		var entry photomigration.MapEntry
		if err := ownerRows.Scan(&entry.SourceHub, &entry.SourceUserID, &entry.StorageKey); err != nil {
			return photomigration.Report{}, nil, err
		}
		entries = append(entries, entry)
	}
	if err := ownerRows.Err(); err != nil {
		return photomigration.Report{}, nil, err
	}
	if err := ownerRows.Close(); err != nil {
		return photomigration.Report{}, nil, err
	}
	_ = vaultRoot
	_ = driver
	return report, entries, nil
}

func validateCatalog(ctx context.Context, db *sql.DB) error {
	var version int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("%w: reading Fotobank schema version: %w", ErrSchemaMismatch, err)
	}
	if version != 1 {
		return fmt.Errorf("%w: schema version %d, expected 1", ErrSchemaMismatch, version)
	}
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT dirty FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&dirty); err != nil {
		return fmt.Errorf("%w: reading Fotobank migration marker: %w", ErrSchemaMismatch, err)
	}
	if dirty {
		return fmt.Errorf("%w: schema_migrations is dirty", ErrSchemaMismatch)
	}
	layouts, err := catalogLayout(ctx, db)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSchemaMismatch, err)
	}
	seen := make(map[string]bool, len(layouts))
	for _, layout := range layouts {
		seen[layout.Name] = true
	}
	var missing, extra []string
	for name := range catalogTables {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	for name := range seen {
		if _, known := catalogTables[name]; !known && !isIgnoredTable(name) {
			extra = append(extra, name)
		}
	}
	for _, layout := range layouts {
		if expected, known := catalogTables[layout.Name]; known {
			got := columnNames(layout.Columns)
			want := append([]string(nil), expected...)
			if !sameStrings(got, want) {
				return fmt.Errorf("%w: %s columns %v, expected %v", ErrSchemaMismatch, layout.Name, got, want)
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("%w: missing tables %s; extra tables %s", ErrSchemaMismatch, strings.Join(missing, ","), strings.Join(extra, ","))
	}
	gotFingerprint := layoutFingerprint(layouts)
	if gotFingerprint != catalogFingerprint {
		for _, layout := range layouts {
			if expected, ok := catalogTableMetadata[layout.Name]; ok && expected != encodeColumns(layout.Columns) {
				return fmt.Errorf("%w: %s column metadata %q, expected %q", ErrSchemaMismatch, layout.Name, encodeColumns(layout.Columns), expected)
			}
		}
		return fmt.Errorf("%w: catalog fingerprint %s, expected %s", ErrSchemaMismatch, gotFingerprint, catalogFingerprint)
	}
	return nil
}

func validateEmbeddedDocbank(ctx context.Context, driver docsqlite.Driver, path string) error {
	db, err := driver.Open(path, docsqlite.OpenOptions{Access: docsqlite.ReadOnlyImmutable, TransactionMode: docsqlite.Deferred})
	if err != nil {
		return fmt.Errorf("opening embedded Docbank database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	var version int64
	if err := db.QueryRowContext(ctx, `SELECT schema_version FROM vault_metadata WHERE singleton=1`).Scan(&version); err != nil {
		return fmt.Errorf("%w: reading schema version: %w", ErrEmbeddedSchemaMismatch, err)
	}
	if version != 16 {
		return fmt.Errorf("%w: version %d, expected 16", ErrEmbeddedSchemaMismatch, version)
	}
	expectedColumns := map[string][]string{
		"vault_metadata": {"singleton", "vault_uid", "schema_version"},
		"blobs":          {"hash", "size", "created_at"},
	}
	expectedMetadata := map[string]string{
		"vault_metadata": "schema_version:INTEGER:1::0,singleton:INTEGER:0::1,vault_uid:TEXT:1::0",
		"blobs":          "created_at:TEXT:1::0,hash:TEXT:0::1,size:INTEGER:1::0",
	}
	for table, expected := range expectedColumns {
		got, err := tableColumns(ctx, db, table)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrEmbeddedSchemaMismatch, table, err)
		}
		if !sameStrings(columnNames(got), expected) {
			return fmt.Errorf("%w: %s columns %v, expected %v", ErrEmbeddedSchemaMismatch, table, columnNames(got), expected)
		}
		if gotMetadata := encodeColumns(got); gotMetadata != expectedMetadata[table] {
			return fmt.Errorf("%w: %s column metadata %q, expected %q", ErrEmbeddedSchemaMismatch, table, gotMetadata, expectedMetadata[table])
		}
	}
	return nil
}
