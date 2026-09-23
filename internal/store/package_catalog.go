package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
)

// PackageVolume preserves the sender's root and the approved mapped root.
type PackageVolume struct {
	Ordinal            int    `json:"ordinal"`
	VolumeName         string `json:"volume_name"`
	DeclaredRoot       string `json:"declared_root"`
	MappedRoot         string `json:"mapped_root"`
	ResolvedRootSHA256 string `json:"resolved_root_sha256"`
}

// PackageVolumeRootBinding binds one mapped volume root to the approved source
// root digest. Admission records it and the import worker recomputes it from
// the live source before staging any content.
func PackageVolumeRootBinding(sourceRootDigest, mappedRoot string) (string, error) {
	encoded, err := canonical.Marshal([]string{"package-volume-root/v1", sourceRootDigest, mappedRoot})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

type PackageRequest struct {
	PackageID            string          `json:"package_id"`
	SnapshotID           string          `json:"snapshot_id"`
	Direction            string          `json:"direction"`
	PackageName          string          `json:"package_name"`
	PartyLabel           string          `json:"party_label"`
	ProfileSHA256        string          `json:"profile_sha256"`
	ProfileJSON          string          `json:"profile_json"`
	MappingSHA256        string          `json:"mapping_sha256"`
	MappingJSON          string          `json:"mapping_json"`
	ManifestSHA256       string          `json:"manifest_sha256"`
	ManifestBlobSHA256   string          `json:"manifest_blob_sha256"`
	PredecessorPackageID string          `json:"predecessor_package_id"`
	Relation             string          `json:"relation"`
	IngestID             string          `json:"ingest_id"`
	ExportPlanID         string          `json:"export_plan_id"`
	ProducedOn           string          `json:"produced_on"`
	State                string          `json:"state"`
	Volumes              []PackageVolume `json:"volumes"`
}

// Package is the durable identity and state of one received or produced set.
type Package struct {
	PackageRequest

	CreatedAt   string `json:"created_at"`
	CompletedAt string `json:"completed_at"`
	MemberCount int    `json:"member_count"`
	PageCount   int    `json:"page_count"`
}

const (
	packageDirectionReceived = "received"
	packageStateComplete     = "complete"
	packageStateFailed       = "failed"
	packageStatePartial      = "partial"
	packageStateSealed       = "sealed"
)

func validatePackageRequest(request PackageRequest) error {
	if validateUUIDv4(request.PackageID) != nil ||
		request.SnapshotID != "" && validateUUIDv4(request.SnapshotID) != nil ||
		request.PredecessorPackageID != "" && validateUUIDv4(request.PredecessorPackageID) != nil ||
		request.IngestID != "" && validateUUIDv4(request.IngestID) != nil ||
		request.ExportPlanID != "" && validateUUIDv4(request.ExportPlanID) != nil {
		return ErrPackageConflict
	}
	if request.Direction != packageDirectionReceived && request.Direction != "produced" ||
		request.Direction == packageDirectionReceived && request.IngestID == "" ||
		request.Direction == "produced" && (request.SnapshotID == "" || request.IngestID != "") ||
		request.Direction == packageDirectionReceived && !slices.Contains([]string{"importing", packageStateComplete, packageStatePartial, packageStateFailed, "cancelled", "purged"}, request.State) ||
		request.Direction == "produced" && !slices.Contains([]string{packageStateSealed, packageStateComplete, packageStateFailed, "cancelled", "purged"}, request.State) ||
		(request.State == packageStateSealed || request.State == packageStateComplete || request.State == packageStatePartial) && request.SnapshotID == "" {
		return ErrPackageConflict
	}
	if len(request.PackageName) == 0 || len(request.PackageName) > 128 ||
		utf8.RuneCountInString(request.PartyLabel) > 64 || len(request.Volumes) > 64 {
		return ErrPackageConflict
	}
	for _, char := range request.PackageName {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			continue
		}
		return ErrPackageConflict
	}
	for _, identity := range []string{request.ProfileSHA256, request.MappingSHA256,
		request.ManifestSHA256, request.ManifestBlobSHA256} {
		if !canonical.IsSHA256Hex(identity) {
			return ErrPackageConflict
		}
	}
	for _, jsonValue := range []struct{ body, hash string }{
		{request.ProfileJSON, request.ProfileSHA256},
		{request.MappingJSON, request.MappingSHA256},
	} {
		if _, err := canonical.Decode[map[string]any]([]byte(jsonValue.body)); err != nil {
			return ErrPackageConflict
		}
		digest := sha256.Sum256([]byte(jsonValue.body))
		if hex.EncodeToString(digest[:]) != jsonValue.hash {
			return ErrPackageConflict
		}
	}
	volumeNames := make(map[string]bool, len(request.Volumes))
	for index, volume := range request.Volumes {
		if volume.Ordinal != index+1 || volume.VolumeName == "" || volumeNames[volume.VolumeName] ||
			volume.DeclaredRoot == "" || !canonical.IsSHA256Hex(volume.ResolvedRootSHA256) {
			return ErrPackageConflict
		}
		volumeNames[volume.VolumeName] = true
	}
	return nil
}

func packageRequestMatches(existing Package, request PackageRequest) bool {
	return existing.PackageID == request.PackageID && existing.SnapshotID == request.SnapshotID &&
		existing.Direction == request.Direction && existing.PackageName == request.PackageName &&
		existing.PartyLabel == request.PartyLabel && existing.ProfileSHA256 == request.ProfileSHA256 &&
		existing.ProfileJSON == request.ProfileJSON && existing.MappingSHA256 == request.MappingSHA256 &&
		existing.MappingJSON == request.MappingJSON && existing.ManifestSHA256 == request.ManifestSHA256 &&
		existing.ManifestBlobSHA256 == request.ManifestBlobSHA256 &&
		existing.PredecessorPackageID == request.PredecessorPackageID && existing.Relation == request.Relation &&
		existing.IngestID == request.IngestID && existing.ExportPlanID == request.ExportPlanID &&
		existing.ProducedOn == request.ProducedOn && existing.State == request.State &&
		slices.Equal(existing.Volumes, request.Volumes)
}

// CreatePackage records one package identity and its declared volumes. An ID
// can be retried only with the same complete request.
func (s *Store) CreatePackage(ctx context.Context, request PackageRequest) (Package, error) {
	if err := validatePackageRequest(request); err != nil {
		return Package{}, err
	}
	if request.Direction == packageDirectionReceived && request.State != "importing" ||
		request.Direction == "produced" && request.State != packageStateSealed {
		return Package{}, ErrPackageConflict
	}
	var result Package
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		existing, err := loadPackageTx(ctx, tx, request.PackageID)
		if err == nil {
			if !packageRequestMatches(existing, request) {
				return ErrPackageConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var manifestCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs WHERE hash=?`, request.ManifestBlobSHA256).Scan(&manifestCount); err != nil {
			return err
		}
		if manifestCount != 1 {
			return ErrPackageConflict
		}
		if request.SnapshotID != "" {
			if _, err := loadCollectionSnapshotTx(ctx, tx, request.SnapshotID); err != nil {
				return err
			}
		}
		result = Package{PackageRequest: request, CreatedAt: nowRFC3339()}
		_, err = tx.ExecContext(ctx, `INSERT INTO packages(
			package_id,snapshot_id,direction,package_name,party_label,profile_sha256,profile_json,
			mapping_sha256,mapping_json,manifest_sha256,manifest_blob_sha256,
			predecessor_package_id,relation,ingest_id,export_plan_id,state,produced_on,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, request.PackageID,
			nullableString(request.SnapshotID), request.Direction, request.PackageName, request.PartyLabel,
			request.ProfileSHA256, request.ProfileJSON, request.MappingSHA256, request.MappingJSON,
			request.ManifestSHA256, request.ManifestBlobSHA256,
			nullableString(request.PredecessorPackageID), request.Relation,
			nullableString(request.IngestID), nullableString(request.ExportPlanID),
			request.State, nullableString(request.ProducedOn), result.CreatedAt)
		if err != nil {
			return err
		}
		for _, volume := range request.Volumes {
			_, err := tx.ExecContext(ctx, `INSERT INTO package_volumes(
				package_id,ordinal,volume_name,declared_root,mapped_root,resolved_root_sha256
			) VALUES(?,?,?,?,?,?)`, request.PackageID, volume.Ordinal, volume.VolumeName,
				volume.DeclaredRoot, volume.MappedRoot, volume.ResolvedRootSHA256)
			if err != nil {
				return err
			}
		}
		result, err = loadPackageTx(ctx, tx, request.PackageID)
		return err
	})
	return result, err
}

func loadPackageTx(ctx context.Context, tx metadataQuerier, id string) (Package, error) {
	var result Package
	err := tx.QueryRowContext(ctx, `SELECT p.package_id,COALESCE(p.snapshot_id,''),p.direction,
		p.package_name,p.party_label,p.profile_sha256,p.profile_json,p.mapping_sha256,p.mapping_json,
		p.manifest_sha256,p.manifest_blob_sha256,COALESCE(p.predecessor_package_id,''),p.relation,
		COALESCE(p.ingest_id,''),COALESCE(p.export_plan_id,''),p.state,COALESCE(p.produced_on,''),
		p.created_at,COALESCE(p.completed_at,''),COALESCE(s.member_count,0),COALESCE(s.page_count,0)
		FROM packages p LEFT JOIN collection_snapshots s ON s.snapshot_id=p.snapshot_id
		WHERE p.package_id=?`, id).Scan(&result.PackageID, &result.SnapshotID, &result.Direction,
		&result.PackageName, &result.PartyLabel, &result.ProfileSHA256, &result.ProfileJSON,
		&result.MappingSHA256, &result.MappingJSON, &result.ManifestSHA256,
		&result.ManifestBlobSHA256, &result.PredecessorPackageID, &result.Relation,
		&result.IngestID, &result.ExportPlanID, &result.State, &result.ProducedOn,
		&result.CreatedAt, &result.CompletedAt, &result.MemberCount, &result.PageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Package{}, ErrNotFound
	}
	if err != nil {
		return Package{}, fmt.Errorf("reading package: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,volume_name,declared_root,mapped_root,resolved_root_sha256
		FROM package_volumes WHERE package_id=? ORDER BY ordinal`, id)
	if err != nil {
		return Package{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var volume PackageVolume
		if err := rows.Scan(&volume.Ordinal, &volume.VolumeName, &volume.DeclaredRoot,
			&volume.MappedRoot, &volume.ResolvedRootSHA256); err != nil {
			return Package{}, err
		}
		result.Volumes = append(result.Volumes, volume)
	}
	if err := rows.Err(); err != nil {
		return Package{}, err
	}
	return result, nil
}

func (s *Store) Package(ctx context.Context, id string) (Package, error) {
	if validateUUIDv4(id) != nil {
		return Package{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Package{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := loadPackageTx(ctx, tx, id)
	if err != nil {
		return Package{}, err
	}
	return result, tx.Commit()
}

// Packages returns a stable, bounded page of package identities.
func (s *Store) Packages(ctx context.Context, direction, after string, limit int) ([]Package, error) {
	if direction != "" && direction != packageDirectionReceived && direction != "produced" ||
		after != "" && validateUUIDv4(after) != nil || limit < 1 || limit > 250 {
		return nil, ErrPackageConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT package_id FROM packages
		WHERE package_id>? AND (?='' OR direction=?) ORDER BY package_id LIMIT ?`,
		after, direction, direction, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	packages := make([]Package, 0, len(ids))
	for _, id := range ids {
		pkg, err := loadPackageTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		packages = append(packages, pkg)
	}
	return packages, tx.Commit()
}
