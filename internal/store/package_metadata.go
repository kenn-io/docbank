package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	metadataCollectionSnapshotType               = "collection_snapshot"
	metadataCollectionSnapshotMemberType         = "collection_snapshot_member"
	metadataCollectionSnapshotRepresentationType = "collection_snapshot_representation"
	metadataPackageType                          = "package"
	metadataPackageVolumeType                    = "package_volume"
)

type metadataCollectionSnapshot struct {
	Type          string         `json:"type"`
	SnapshotID    string         `json:"snapshot_id"`
	VaultID       string         `json:"vault_id"`
	CanonicalJSON jsontext.Value `json:"canonical_json"`
	Checksum      string         `json:"checksum"`
}

type metadataCollectionSnapshotMember struct {
	Type          string         `json:"type"`
	SnapshotID    string         `json:"snapshot_id"`
	Ordinal       int            `json:"ordinal"`
	CanonicalJSON jsontext.Value `json:"canonical_json"`
	Checksum      string         `json:"checksum"`
}

type metadataCollectionSnapshotRepresentation struct {
	Type          string         `json:"type"`
	SnapshotID    string         `json:"snapshot_id"`
	OccurrenceID  string         `json:"occurrence_id"`
	Role          string         `json:"role"`
	Ordinal       int            `json:"ordinal"`
	CanonicalJSON jsontext.Value `json:"canonical_json"`
	Checksum      string         `json:"checksum"`
}

type metadataPackage struct {
	Type          string         `json:"type"`
	PackageID     string         `json:"package_id"`
	CanonicalJSON jsontext.Value `json:"canonical_json"`
	Checksum      string         `json:"checksum"`
}

type metadataPackageVolume struct {
	Type          string         `json:"type"`
	PackageID     string         `json:"package_id"`
	Ordinal       int            `json:"ordinal"`
	CanonicalJSON jsontext.Value `json:"canonical_json"`
	Checksum      string         `json:"checksum"`
}

func exportPackageMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	snapshotIDs, err := pageMetadataKeys(ctx, q, `SELECT snapshot_id FROM collection_snapshots ORDER BY snapshot_id`)
	if err != nil {
		return err
	}
	for _, id := range snapshotIDs {
		snapshot, err := loadCollectionSnapshotTx(ctx, q, id)
		if err != nil {
			return err
		}
		var vaultID string
		if err := q.QueryRowContext(ctx, `SELECT vault_uid FROM collection_snapshots WHERE snapshot_id=?`, id).Scan(&vaultID); err != nil {
			return err
		}
		checksum, encoded, err := snapshotRowChecksum(snapshot)
		if err != nil || checksum != snapshot.Checksum {
			return ErrPackageConflict
		}
		if err := write(metadataCollectionSnapshot{
			Type: metadataCollectionSnapshotType, SnapshotID: id, VaultID: vaultID,
			CanonicalJSON: encoded, Checksum: checksum,
		}); err != nil {
			return err
		}
		ordinal := 0
		for {
			members, err := loadSnapshotMemberRows(ctx, q, id, ordinal, 250)
			if err != nil {
				return err
			}
			if len(members) == 0 {
				break
			}
			for _, member := range members {
				encoded, checksum, err := snapshotRowBytes(member)
				if err != nil {
					return err
				}
				if err := write(metadataCollectionSnapshotMember{
					Type: metadataCollectionSnapshotMemberType, SnapshotID: id,
					Ordinal: member.Ordinal, CanonicalJSON: encoded, Checksum: checksum,
				}); err != nil {
					return err
				}
				representations, err := loadSnapshotRepresentationRows(ctx, q, id, member.OccurrenceID)
				if err != nil {
					return err
				}
				for _, rep := range representations {
					encoded, checksum, err := snapshotRowBytes(rep)
					if err != nil {
						return err
					}
					if err := write(metadataCollectionSnapshotRepresentation{
						Type: metadataCollectionSnapshotRepresentationType, SnapshotID: id,
						OccurrenceID: rep.OccurrenceID, Role: rep.Role, Ordinal: rep.Ordinal,
						CanonicalJSON: encoded, Checksum: checksum,
					}); err != nil {
						return err
					}
				}
				ordinal = member.Ordinal
			}
		}
		if ordinal != snapshot.MemberCount {
			return ErrPackageConflict
		}
	}
	packageIDs, err := pageMetadataKeys(ctx, q, `SELECT package_id FROM packages ORDER BY package_id`)
	if err != nil {
		return err
	}
	for _, id := range packageIDs {
		pkg, err := loadPackageTx(ctx, q, id)
		if err != nil {
			return err
		}
		volumes := pkg.Volumes
		pkg.Volumes = nil
		encoded, checksum, err := snapshotRowBytes(pkg)
		if err != nil {
			return err
		}
		if err := write(metadataPackage{Type: metadataPackageType, PackageID: id,
			CanonicalJSON: encoded, Checksum: checksum}); err != nil {
			return err
		}
		for _, volume := range volumes {
			encoded, checksum, err := snapshotRowBytes(volume)
			if err != nil {
				return err
			}
			if err := write(metadataPackageVolume{Type: metadataPackageVolumeType, PackageID: id,
				Ordinal: volume.Ordinal, CanonicalJSON: encoded, Checksum: checksum}); err != nil {
				return err
			}
		}
	}
	return nil
}

func importPackageMetadata(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataCollectionSnapshotType:
		var record metadataCollectionSnapshot
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if record.Type != kind || pageChecksum(record.CanonicalJSON) != record.Checksum || validateUUIDv4(record.SnapshotID) != nil {
			return ErrPackageConflict
		}
		snapshot, err := canonical.Decode[CollectionSnapshot](record.CanonicalJSON)
		if err != nil || snapshot.SnapshotID != record.SnapshotID || snapshot.Checksum != "" {
			return ErrPackageConflict
		}
		sourceJSON, err := canonical.Marshal(snapshot.SourceCollectionIDs)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO collection_snapshots(
			snapshot_id,vault_uid,predecessor_id,source_collection_ids_json,
			member_count,page_count,member_hash,manifest_sha256,canonical_json,checksum,sealed_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, snapshot.SnapshotID, record.VaultID,
			nullableString(snapshot.PredecessorID), sourceJSON, snapshot.MemberCount,
			snapshot.PageCount, snapshot.MemberHash, snapshot.ManifestSHA256,
			[]byte(record.CanonicalJSON), record.Checksum, snapshot.SealedAt)
		return err
	case metadataCollectionSnapshotMemberType:
		var record metadataCollectionSnapshotMember
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if record.Type != kind || pageChecksum(record.CanonicalJSON) != record.Checksum {
			return ErrPackageConflict
		}
		member, err := canonical.Decode[CollectionSnapshotMember](record.CanonicalJSON)
		if err != nil || member.Ordinal != record.Ordinal || len(member.Representations) != 0 {
			return ErrPackageConflict
		}
		var selected any
		if len(member.SelectedSourcePages) > 0 {
			selected, err = canonical.Marshal(member.SelectedSourcePages)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO collection_snapshot_members(
			snapshot_id,ordinal,occurrence_id,node_id,content_version_id,blob_sha256,size,
			family_id,parent_occurrence_id,family_order,display_name,frozen_fields_json,
			document_kind,selected_source_pages_json,selected_pdf_sha256,source_page_count,
			canonical_json,checksum
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.SnapshotID, member.Ordinal,
			member.OccurrenceID, member.NodeID, member.ContentVersionID, member.BlobSHA256,
			member.Size, member.FamilyID, member.ParentOccurrenceID, member.FamilyOrder,
			member.DisplayName, member.FrozenFieldsJSON, member.DocumentKind, selected,
			member.SelectedPDFSHA256, member.SourcePageCount, []byte(record.CanonicalJSON), record.Checksum)
		return err
	case metadataCollectionSnapshotRepresentationType:
		var record metadataCollectionSnapshotRepresentation
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if record.Type != kind || pageChecksum(record.CanonicalJSON) != record.Checksum {
			return ErrPackageConflict
		}
		rep, err := canonical.Decode[CollectionSnapshotRepresentation](record.CanonicalJSON)
		if err != nil || rep.OccurrenceID != record.OccurrenceID || rep.Role != record.Role || rep.Ordinal != record.Ordinal {
			return ErrPackageConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO collection_snapshot_representations(
			snapshot_id,occurrence_id,role,ordinal,status,text_authority,content_version_id,
			blob_sha256,size,media_type,page_number,verified_page_count,rendition_build_id,
			lexical_generation_id,recipe_sha256,canonical_json,checksum
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.SnapshotID, rep.OccurrenceID,
			rep.Role, rep.Ordinal, rep.Status, rep.TextAuthority, nullableString(rep.ContentVersionID),
			nullableString(rep.BlobSHA256), rep.Size, rep.MediaType, rep.PageNumber, rep.VerifiedPageCount,
			nullableString(rep.RenditionBuildID), nullableString(rep.LexicalGenerationID),
			nullableString(rep.RecipeSHA256), []byte(record.CanonicalJSON), record.Checksum)
		return err
	case metadataPackageType:
		var record metadataPackage
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if record.Type != kind || pageChecksum(record.CanonicalJSON) != record.Checksum {
			return ErrPackageConflict
		}
		pkg, err := canonical.Decode[Package](record.CanonicalJSON)
		if err != nil || pkg.PackageID != record.PackageID || len(pkg.Volumes) != 0 || validatePackageRequest(pkg.PackageRequest) != nil {
			return ErrPackageConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO packages(
			package_id,snapshot_id,direction,package_name,party_label,profile_sha256,profile_json,
			mapping_sha256,mapping_json,manifest_sha256,manifest_blob_sha256,
			predecessor_package_id,relation,ingest_id,export_plan_id,state,produced_on,created_at,completed_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, pkg.PackageID, nullableString(pkg.SnapshotID),
			pkg.Direction, pkg.PackageName, pkg.PartyLabel, pkg.ProfileSHA256, pkg.ProfileJSON,
			pkg.MappingSHA256, pkg.MappingJSON, pkg.ManifestSHA256, pkg.ManifestBlobSHA256,
			nullableString(pkg.PredecessorPackageID), pkg.Relation, nullableString(pkg.IngestID),
			nullableString(pkg.ExportPlanID), pkg.State, nullableString(pkg.ProducedOn),
			pkg.CreatedAt, nullableString(pkg.CompletedAt))
		return err
	case metadataPackageVolumeType:
		var record metadataPackageVolume
		if err := decodeMetadataRecord(raw, &record); err != nil {
			return err
		}
		if record.Type != kind || pageChecksum(record.CanonicalJSON) != record.Checksum {
			return ErrPackageConflict
		}
		volume, err := canonical.Decode[PackageVolume](record.CanonicalJSON)
		if err != nil || volume.Ordinal != record.Ordinal {
			return ErrPackageConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_volumes(
			package_id,ordinal,volume_name,declared_root,mapped_root,resolved_root_sha256
		) VALUES(?,?,?,?,?,?)`, record.PackageID, volume.Ordinal, volume.VolumeName,
			volume.DeclaredRoot, volume.MappedRoot, volume.ResolvedRootSHA256)
		return err
	default:
		return errors.New("unknown package authority record")
	}
}

func validatePackageMetadataState(ctx context.Context, q metadataQuerier, vaultID string) error {
	if err := exportPackageMetadata(ctx, q, func(any) error { return nil }); err != nil {
		return fmt.Errorf("validating package metadata: %w", err)
	}
	var foreignVault bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collection_snapshots WHERE vault_uid<>?)`, vaultID).Scan(&foreignVault); err != nil {
		return err
	}
	if foreignVault {
		return ErrPackageConflict
	}
	snapshotIDs, err := pageMetadataKeys(ctx, q, `SELECT snapshot_id FROM collection_snapshots ORDER BY snapshot_id`)
	if err != nil {
		return err
	}
	for _, id := range snapshotIDs {
		snapshot, err := loadCollectionSnapshotTx(ctx, q, id)
		if err != nil {
			return err
		}
		if snapshot.MemberCount < 1 || snapshot.MemberCount > MaxSnapshotMembers ||
			snapshot.PageCount < 0 || snapshot.PageCount > MaxSnapshotPages ||
			!canonical.IsSHA256Hex(snapshot.MemberHash) || !canonical.IsSHA256Hex(snapshot.ManifestSHA256) ||
			len(snapshot.SourceCollectionIDs) > 64 || !slices.IsSorted(snapshot.SourceCollectionIDs) ||
			!slices.Equal(slices.Compact(slices.Clone(snapshot.SourceCollectionIDs)), snapshot.SourceCollectionIDs) ||
			validateUUIDv4(snapshot.SnapshotID) != nil ||
			snapshot.PredecessorID != "" && validateUUIDv4(snapshot.PredecessorID) != nil {
			return ErrPackageConflict
		}
		for _, sourceID := range snapshot.SourceCollectionIDs {
			if validateUUIDv4(sourceID) != nil {
				return ErrPackageConflict
			}
			var exists bool
			if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ingests WHERE id=?)`, sourceID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrPackageConflict
			}
		}
		if err := validateSnapshotMetadataRows(ctx, q, snapshot); err != nil {
			return err
		}
	}
	packageIDs, err := pageMetadataKeys(ctx, q, `SELECT package_id FROM packages ORDER BY package_id`)
	if err != nil {
		return err
	}
	for _, id := range packageIDs {
		pkg, err := loadPackageTx(ctx, q, id)
		if err != nil {
			return err
		}
		if validatePackageRequest(pkg.PackageRequest) != nil {
			return ErrPackageConflict
		}
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=?)`, pkg.ManifestBlobSHA256).Scan(&exists); err != nil || !exists {
			return ErrPackageConflict
		}
	}
	return nil
}

// validateSnapshotMetadataRows keeps only one member page and compact graph
// and version indexes while checking the same canonical manifest as sealing.
// Historical collection membership is intentionally not rechecked here: an
// immutable snapshot keeps its original source IDs after membership changes.
func validateSnapshotMetadataRows(ctx context.Context, q metadataQuerier, snapshot CollectionSnapshot) error {
	digest := sha256.New()
	_, _ = digest.Write([]byte(`{"members":[`))
	parents := make(map[string]string)
	families := make(map[string]string)
	nodeVersions := make(map[int64]string)
	count, pages := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := loadSnapshotMemberRows(ctx, q, snapshot.SnapshotID, count, 250)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			member := &batch[i]
			member.Representations, err = loadSnapshotRepresentationRows(ctx, q, snapshot.SnapshotID, member.OccurrenceID)
			if err != nil {
				return err
			}
			if count == MaxSnapshotMembers || member.Ordinal != count+1 ||
				member.NodeID <= 0 || validateUUIDv4(member.ContentVersionID) != nil {
				return ErrPackageConflict
			}
			if _, exists := parents[member.OccurrenceID]; exists || nodeVersions[member.NodeID] != "" {
				return ErrPackageConflict
			}
			normalized, err := normalizeSnapshotMember(*member, count+1)
			if err != nil {
				return err
			}
			encoded, err := canonical.Marshal(member)
			if err != nil {
				return err
			}
			normalizedJSON, err := canonical.Marshal(normalized)
			if err != nil {
				return err
			}
			if !bytes.Equal(encoded, normalizedJSON) {
				return ErrPackageConflict
			}
			if count > 0 {
				_, _ = digest.Write([]byte{','})
			}
			_, _ = digest.Write(encoded)
			parents[member.OccurrenceID] = member.ParentOccurrenceID
			families[member.OccurrenceID] = member.FamilyID
			nodeVersions[member.NodeID] = member.ContentVersionID
			count++
			if member.SelectedSourcePages != nil {
				pages += len(member.SelectedSourcePages)
			} else {
				for _, rep := range member.Representations {
					if rep.Role == "page_image" && rep.Status == roleAvailable {
						pages++
					}
				}
			}
			if pages > MaxSnapshotPages {
				return ErrPackageConflict
			}
		}
		// The batch remains bounded while exact content versions and role blobs
		// are checked against the restored tables.
		if err := validateSnapshotMembersTx(ctx, q, SnapshotSealRequest{Members: batch}); err != nil {
			return err
		}
	}
	if count != snapshot.MemberCount || pages != snapshot.PageCount ||
		!validSnapshotFamilyGraph(parents, families) ||
		snapshotNodeVersionHash(nodeVersions) != snapshot.MemberHash {
		return ErrPackageConflict
	}
	sourceJSON, err := canonical.Marshal(snapshot.SourceCollectionIDs)
	if err != nil {
		return err
	}
	_, _ = digest.Write([]byte(`],"source_collection_ids":`))
	_, _ = digest.Write(sourceJSON)
	_, _ = digest.Write([]byte{'}'})
	if hex.EncodeToString(digest.Sum(nil)) != snapshot.ManifestSHA256 {
		return ErrPackageConflict
	}
	return nil
}
