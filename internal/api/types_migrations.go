package api

import (
	"go.kenn.io/docbank/internal/photomigration"
	"go.kenn.io/docbank/internal/store"
)

type FotobankInventoryRequest struct {
	CatalogPath  string `json:"catalog_path,omitzero"`
	VaultRoot    string `json:"vault_root,omitzero"`
	ArchiveRoot  string `json:"archive_root,omitzero"`
	SnapshotID   string `json:"snapshot_id,omitzero"`
	OwnerMapPath string `json:"owner_map_path"`
}

type MigrationSource struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
}

type MigrationSchema struct {
	CatalogVersion         int64  `json:"catalog_version,omitzero"`
	CatalogFingerprint     string `json:"catalog_fingerprint,omitzero"`
	EmbeddedDocbankVersion int64  `json:"embedded_docbank_version,omitzero"`
	ArchiveMetadataFormat  string `json:"archive_metadata_format,omitzero"`
}

type MigrationCounts struct {
	Owners           int64 `json:"owners"`
	Assets           int64 `json:"assets"`
	Files            int64 `json:"files"`
	Bytes            int64 `json:"bytes"`
	Albums           int64 `json:"albums"`
	AlbumMemberships int64 `json:"album_memberships"`
	Shares           int64 `json:"shares"`
	Checkouts        int64 `json:"checkouts"`
	CheckoutEntries  int64 `json:"checkout_entries"`
	AIResults        int64 `json:"ai_results"`
	HiddenSetup      int64 `json:"hidden_setup"`
}

type MigrationVectorGeneration struct {
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Rebuildable bool   `json:"rebuildable"`
}

type MigrationCapacity struct {
	SourceBytes         int64 `json:"source_bytes"`
	UniqueBlobBytes     int64 `json:"unique_blob_bytes"`
	MinimumContentBytes int64 `json:"minimum_content_bytes"`
}

type MigrationReport struct {
	Source    MigrationSource             `json:"source"`
	Schema    MigrationSchema             `json:"schema"`
	Counts    MigrationCounts             `json:"counts"`
	Vectors   []MigrationVectorGeneration `json:"vectors,omitzero"`
	Capacity  MigrationCapacity           `json:"capacity"`
	CreatedAt string                      `json:"created_at" format:"date-time"`
}

type MigrationMapEntry struct {
	SourceHub      string `json:"source_hub"`
	SourceUserID   string `json:"source_user_id"`
	StorageKey     string `json:"storage_key"`
	DocbankOwnerID string `json:"docbank_owner_id,omitzero"`
}

type MigrationOwnerMap struct {
	Source  MigrationSource     `json:"source"`
	Entries []MigrationMapEntry `json:"entries"`
}

type MigrationRun struct {
	ID           string            `json:"id" format:"uuid"`
	Source       MigrationSource   `json:"source"`
	CreatedAt    string            `json:"created_at" format:"date-time"`
	Report       MigrationReport   `json:"report"`
	OwnerMap     MigrationOwnerMap `json:"owner_map"`
	OwnerMapPath string            `json:"owner_map_path,omitzero"`
}

type MigrationRunPage struct {
	Total int            `json:"total"`
	Items []MigrationRun `json:"items"`
}

type migrationRunOutput struct{ Body MigrationRun }
type migrationRunPageOutput struct{ Body MigrationRunPage }

func fromPhotoMigrationRun(run store.PhotoMigrationRun) MigrationRun {
	return MigrationRun{ID: run.ID, Source: MigrationSource{Kind: run.Source.Kind, Identity: run.Source.Identity}, CreatedAt: run.CreatedAt,
		Report: fromPhotoMigrationReport(run.Report), OwnerMap: fromPhotoMigrationMap(run.OwnerMap)}
}

func fromPhotoMigrationReport(report photomigration.Report) MigrationReport {
	result := MigrationReport{Source: MigrationSource{Kind: report.Source.Kind, Identity: report.Source.Identity},
		Schema:   MigrationSchema{CatalogVersion: report.Schema.CatalogVersion, CatalogFingerprint: report.Schema.CatalogFingerprint, EmbeddedDocbankVersion: report.Schema.EmbeddedDocbankVersion, ArchiveMetadataFormat: report.Schema.ArchiveMetadataFormat},
		Counts:   MigrationCounts{Owners: report.Counts.Owners, Assets: report.Counts.Assets, Files: report.Counts.Files, Bytes: report.Counts.Bytes, Albums: report.Counts.Albums, AlbumMemberships: report.Counts.AlbumMemberships, Shares: report.Counts.Shares, Checkouts: report.Counts.Checkouts, CheckoutEntries: report.Counts.CheckoutEntries, AIResults: report.Counts.AIResults, HiddenSetup: report.Counts.HiddenSetup},
		Capacity: MigrationCapacity{SourceBytes: report.Capacity.SourceBytes, UniqueBlobBytes: report.Capacity.UniqueBlobBytes, MinimumContentBytes: report.Capacity.MinimumContentBytes}, CreatedAt: report.CreatedAt}
	for _, vector := range report.Vectors {
		result.Vectors = append(result.Vectors, MigrationVectorGeneration{ID: vector.ID, Fingerprint: vector.Fingerprint, State: vector.State, Rebuildable: vector.Rebuildable})
	}
	return result
}

func fromPhotoMigrationMap(template photomigration.OwnerMapTemplate) MigrationOwnerMap {
	result := MigrationOwnerMap{Source: MigrationSource{Kind: template.Source.Kind, Identity: template.Source.Identity}}
	for _, entry := range template.Entries {
		result.Entries = append(result.Entries, MigrationMapEntry{SourceHub: entry.SourceHub, SourceUserID: entry.SourceUserID, StorageKey: entry.StorageKey, DocbankOwnerID: entry.DocbankOwnerID})
	}
	return result
}
