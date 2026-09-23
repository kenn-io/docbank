package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	MaxSnapshotMembers = 100_000
	MaxSnapshotPages   = 1_000_000
	maxInlineSnapshot  = 64 << 10
	// Production snapshots are server-derived, so the 1 MiB command limit does
	// not apply. Bound their canonical manifest for at most 100,000 compact
	// occurrences, below the plan's 512 MiB renderer memory limit.
	maxProductionSnapshotManifest = 128 << 20
)

var ErrPackageConflict = errors.New("package_conflict: package request conflicts with recorded authority")

var ErrPackageRetained = errors.New("package_retained: package authority retains this content version")

// CollectionSnapshotRepresentation freezes one verified role of an occurrence.
type CollectionSnapshotRepresentation struct {
	OccurrenceID        string `json:"occurrence_id"`
	Role                string `json:"role"`
	Ordinal             int    `json:"ordinal"`
	Status              string `json:"status"`
	TextAuthority       string `json:"text_authority"`
	ContentVersionID    string `json:"content_version_id"`
	BlobSHA256          string `json:"blob_sha256"`
	MediaType           string `json:"media_type"`
	Size                int64  `json:"size"`
	PageNumber          int    `json:"page_number"`
	VerifiedPageCount   int    `json:"verified_page_count"`
	RecipeSHA256        string `json:"recipe_sha256"`
	RenditionBuildID    string `json:"rendition_build_id"`
	LexicalGenerationID string `json:"lexical_generation_id"`
}

// CollectionSnapshotMember is durable package membership. Query snapshots use
// a separate SnapshotMember projection and have a different lifetime.
type CollectionSnapshotMember struct {
	Ordinal             int                                `json:"ordinal"`
	OccurrenceID        string                             `json:"occurrence_id"`
	NodeID              int64                              `json:"node_id"`
	ContentVersionID    string                             `json:"content_version_id"`
	BlobSHA256          string                             `json:"blob_sha256"`
	Size                int64                              `json:"size"`
	FamilyID            string                             `json:"family_id"`
	ParentOccurrenceID  string                             `json:"parent_occurrence_id"`
	FamilyOrder         int                                `json:"family_order"`
	DisplayName         string                             `json:"display_name"`
	FrozenFieldsJSON    string                             `json:"frozen_fields_json"`
	DocumentKind        string                             `json:"document_kind"`
	SelectedSourcePages []int                              `json:"selected_source_pages"`
	SelectedPDFSHA256   string                             `json:"selected_pdf_sha256"`
	SourcePageCount     int                                `json:"source_page_count"`
	SourceCollectionIDs []string                           `json:"source_collection_ids"`
	Representations     []CollectionSnapshotRepresentation `json:"representations"`
}

type SnapshotSealRequest struct {
	SnapshotID          string                     `json:"snapshot_id"`
	PredecessorID       string                     `json:"predecessor_id"`
	SourceCollectionIDs []string                   `json:"source_collection_ids"`
	Members             []CollectionSnapshotMember `json:"members"`
}

// CollectionSnapshot is the immutable header of a sealed package selection.
type CollectionSnapshot struct {
	SnapshotID          string   `json:"snapshot_id"`
	PredecessorID       string   `json:"predecessor_id"`
	SourceCollectionIDs []string `json:"source_collection_ids"`
	MemberHash          string   `json:"member_hash"`
	ManifestSHA256      string   `json:"manifest_sha256"`
	Checksum            string   `json:"checksum"`
	SealedAt            string   `json:"sealed_at"`
	MemberCount         int      `json:"member_count"`
	PageCount           int      `json:"page_count"`
}

func collectionSnapshotMemberHash(members []CollectionSnapshotMember) (string, error) {
	byNode := make(map[int64]string, len(members))
	for _, member := range members {
		if member.NodeID <= 0 || validateUUIDv4(member.ContentVersionID) != nil {
			return "", ErrPackageConflict
		}
		if version, ok := byNode[member.NodeID]; ok && version != member.ContentVersionID {
			return "", ErrPackageConflict
		}
		byNode[member.NodeID] = member.ContentVersionID
	}
	ids := make([]int64, 0, len(byNode))
	for id := range byNode {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	digest := sha256.New()
	for _, id := range ids {
		_, _ = fmt.Fprintf(digest, "%d:%s\n", id, byNode[id])
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func snapshotRowChecksum(snapshot CollectionSnapshot) (string, []byte, error) {
	snapshot.Checksum = ""
	encoded, err := canonical.Marshal(snapshot)
	if err != nil {
		return "", nil, err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), encoded, nil
}

func snapshotRowBytes(value any) ([]byte, string, error) {
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(hash[:]), nil
}

func normalizeSnapshotMember(value CollectionSnapshotMember, ordinal int) (CollectionSnapshotMember, error) {
	member := &value
	member.Representations = slices.Clone(member.Representations)
	if member.SourceCollectionIDs != nil {
		member.SourceCollectionIDs = slices.Clone(member.SourceCollectionIDs)
	}
	slices.Sort(member.SourceCollectionIDs)
	member.SourceCollectionIDs = slices.Compact(member.SourceCollectionIDs)
	for _, id := range member.SourceCollectionIDs {
		if validateUUIDv4(id) != nil {
			return CollectionSnapshotMember{}, ErrPackageConflict
		}
	}
	if member.Ordinal != ordinal || member.OccurrenceID == "" || member.FamilyID == "" || member.FamilyOrder < 0 || member.DocumentKind == "" ||
		!canonical.IsSHA256Hex(member.BlobSHA256) || member.Size < 0 {
		return CollectionSnapshotMember{}, ErrPackageConflict
	}
	if member.SourcePageCount > 0 && member.SelectedSourcePages == nil {
		if member.SourcePageCount > MaxSnapshotPages {
			return CollectionSnapshotMember{}, ErrPackageConflict
		}
		member.SelectedSourcePages = make([]int, member.SourcePageCount)
		for page := range member.SelectedSourcePages {
			member.SelectedSourcePages[page] = page + 1
		}
	}
	if member.SelectedSourcePages != nil {
		member.SelectedSourcePages = slices.Clone(member.SelectedSourcePages)
		if len(member.SelectedSourcePages) == 0 || member.SourcePageCount <= 0 ||
			!canonical.IsSHA256Hex(member.SelectedPDFSHA256) {
			return CollectionSnapshotMember{}, ErrPackageConflict
		}
		for pageIndex, page := range member.SelectedSourcePages {
			if page < 1 || page > member.SourcePageCount ||
				pageIndex > 0 && page <= member.SelectedSourcePages[pageIndex-1] {
				return CollectionSnapshotMember{}, ErrPackageConflict
			}
		}
	} else if member.SourcePageCount != 0 || member.SelectedPDFSHA256 != "" {
		return CollectionSnapshotMember{}, ErrPackageConflict
	}
	roles := make(map[string]bool, len(member.Representations))
	slices.SortFunc(member.Representations, func(left, right CollectionSnapshotRepresentation) int {
		if order := strings.Compare(left.Role, right.Role); order != 0 {
			return order
		}
		if left.Ordinal < right.Ordinal {
			return -1
		}
		if left.Ordinal > right.Ordinal {
			return 1
		}
		return 0
	})
	for j := range member.Representations {
		rep := &member.Representations[j]
		if rep.OccurrenceID != member.OccurrenceID || rep.Role == "" || rep.Ordinal < 0 ||
			!slices.Contains([]string{roleAvailable, "missing", "omitted", mediaCoverageUnavailable}, rep.Status) ||
			!slices.Contains([]string{"supplied", "ocr", "ordinary", "none"}, rep.TextAuthority) {
			return CollectionSnapshotMember{}, ErrPackageConflict
		}
		roleKey := fmt.Sprintf("%s/%d", rep.Role, rep.Ordinal)
		if roles[roleKey] {
			return CollectionSnapshotMember{}, ErrPackageConflict
		}
		roles[roleKey] = true
		if rep.Status == roleAvailable && (!canonical.IsSHA256Hex(rep.BlobSHA256) || rep.Size < 0) {
			return CollectionSnapshotMember{}, ErrPackageConflict
		}
	}
	return value, nil
}

func normalizeSnapshotRequest(request SnapshotSealRequest, allowRepeatedNodes bool) (SnapshotSealRequest, error) {
	if validateUUIDv4(request.SnapshotID) != nil || len(request.Members) == 0 || len(request.Members) > MaxSnapshotMembers || len(request.SourceCollectionIDs) > 64 {
		return request, ErrPackageConflict
	}
	if request.PredecessorID != "" && validateUUIDv4(request.PredecessorID) != nil {
		return request, ErrPackageConflict
	}
	request.SourceCollectionIDs = append([]string{}, request.SourceCollectionIDs...)
	slices.Sort(request.SourceCollectionIDs)
	request.SourceCollectionIDs = slices.Compact(request.SourceCollectionIDs)
	for _, id := range request.SourceCollectionIDs {
		if validateUUIDv4(id) != nil {
			return request, ErrPackageConflict
		}
	}
	request.Members = slices.Clone(request.Members)
	seen := make(map[string]bool, len(request.Members))
	seenNodes := make(map[int64]bool, len(request.Members))
	for i, member := range request.Members {
		normalized, err := normalizeSnapshotMember(member, i+1)
		if err == nil && len(normalized.SourceCollectionIDs) == 0 && len(request.SourceCollectionIDs) == 1 {
			normalized.SourceCollectionIDs = slices.Clone(request.SourceCollectionIDs)
		}
		if err != nil || seen[normalized.OccurrenceID] || !allowRepeatedNodes && seenNodes[normalized.NodeID] ||
			len(request.SourceCollectionIDs) == 0 && len(normalized.SourceCollectionIDs) != 0 ||
			len(request.SourceCollectionIDs) != 0 && (len(normalized.SourceCollectionIDs) == 0 ||
				!sourceIDsContained(request.SourceCollectionIDs, normalized.SourceCollectionIDs)) {
			return request, ErrPackageConflict
		}
		request.Members[i] = normalized
		seen[normalized.OccurrenceID] = true
		seenNodes[normalized.NodeID] = true
	}
	parents := make(map[string]string, len(request.Members))
	families := make(map[string]string, len(request.Members))
	for _, member := range request.Members {
		parents[member.OccurrenceID] = member.ParentOccurrenceID
		families[member.OccurrenceID] = member.FamilyID
	}
	if !validSnapshotFamilyGraph(parents, families) {
		return request, ErrPackageConflict
	}
	return request, nil
}

func sourceIDsContained(snapshotIDs, memberIDs []string) bool {
	for _, id := range memberIDs {
		if _, found := slices.BinarySearch(snapshotIDs, id); !found {
			return false
		}
	}
	return true
}

// SealCollectionSnapshot freezes exact content and origin before an import or
// export worker starts. Replaying the same ID with different contents fails.
func (s *Store) SealCollectionSnapshot(ctx context.Context, request SnapshotSealRequest) (CollectionSnapshot, error) {
	return s.sealCollectionSnapshot(ctx, request, false, maxInlineSnapshot, func(ctx context.Context, tx *sql.Tx, request SnapshotSealRequest) error {
		return validateSnapshotMembersTx(ctx, tx, request)
	})
}

func (s *Store) sealCollectionSnapshot(ctx context.Context, request SnapshotSealRequest, allowRepeatedNodes bool, maxManifestBytes int,
	validate func(context.Context, *sql.Tx, SnapshotSealRequest) error) (CollectionSnapshot, error) {
	request, err := normalizeSnapshotRequest(request, allowRepeatedNodes)
	if err != nil {
		return CollectionSnapshot{}, err
	}
	manifest, err := canonical.Marshal(struct {
		SourceCollectionIDs []string                   `json:"source_collection_ids"`
		Members             []CollectionSnapshotMember `json:"members"`
	}{request.SourceCollectionIDs, request.Members})
	if err != nil {
		return CollectionSnapshot{}, err
	}
	if len(manifest) > maxManifestBytes {
		return CollectionSnapshot{}, ErrPackageConflict
	}
	manifestHash := sha256.Sum256(manifest)
	memberHash, err := collectionSnapshotMemberHash(request.Members)
	if err != nil {
		return CollectionSnapshot{}, err
	}
	pageCount := 0
	for _, member := range request.Members {
		if member.SelectedSourcePages != nil {
			pageCount += len(member.SelectedSourcePages)
		} else {
			for _, rep := range member.Representations {
				if rep.Role == "page_image" && rep.Status == roleAvailable {
					pageCount++
				}
			}
		}
		if pageCount > MaxSnapshotPages {
			return CollectionSnapshot{}, ErrPackageConflict
		}
	}
	result := CollectionSnapshot{
		SnapshotID: request.SnapshotID, PredecessorID: request.PredecessorID,
		SourceCollectionIDs: request.SourceCollectionIDs, MemberCount: len(request.Members),
		PageCount: pageCount, MemberHash: memberHash,
		ManifestSHA256: hex.EncodeToString(manifestHash[:]), SealedAt: nowRFC3339(),
	}
	result.Checksum, _, err = snapshotRowChecksum(result)
	if err != nil {
		return CollectionSnapshot{}, err
	}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		existing, loadErr := loadCollectionSnapshotTx(ctx, tx, request.SnapshotID)
		if loadErr == nil {
			if existing.ManifestSHA256 != result.ManifestSHA256 || existing.PredecessorID != result.PredecessorID {
				return ErrPackageConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(loadErr, ErrNotFound) {
			return loadErr
		}
		if err := validate(ctx, tx, request); err != nil {
			return err
		}
		return insertCollectionSnapshotTx(ctx, tx, s.vaultID, result, request.Members)
	})
	return result, err
}

func validateSnapshotMembersTx(ctx context.Context, tx metadataQuerier, request SnapshotSealRequest) error {
	for _, member := range request.Members {
		var hash string
		var size int64
		err := tx.QueryRowContext(ctx, `SELECT blob_hash,size FROM content_versions WHERE version_id=? AND node_id=?`, member.ContentVersionID, member.NodeID).Scan(&hash, &size)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (hash != member.BlobSHA256 || size != member.Size) {
			return ErrPackageConflict
		}
		if err != nil {
			return err
		}
		if member.SourcePageCount > 0 {
			pageDocument, pageErr := loadPageDocument(ctx, tx, member.ContentVersionID)
			if errors.Is(pageErr, ErrNotFound) && snapshotPDFRepresentationValid(member) {
				pageErr = nil
			}
			if pageErr != nil {
				return pageErr
			}
			if pageDocument.PageCount != 0 && (pageDocument.PageCount != member.SourcePageCount ||
				pageDocument.Source.SHA256 != member.SelectedPDFSHA256 ||
				pageDocument.Source.Size != member.Size) {
				return ErrPackageConflict
			}
		}
		for _, rep := range member.Representations {
			if rep.Status != roleAvailable {
				continue
			}
			var size int64
			err := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, rep.BlobSHA256).Scan(&size)
			if errors.Is(err, sql.ErrNoRows) || err == nil && size != rep.Size {
				return ErrPackageConflict
			}
			if err != nil {
				return err
			}
			if rep.ContentVersionID != "" {
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_versions WHERE version_id=?`, rep.ContentVersionID).Scan(&count); err != nil {
					return err
				}
				if count != 1 {
					return ErrPackageConflict
				}
			}
		}
	}
	if len(request.SourceCollectionIDs) == 0 {
		return nil
	}
	matched := make(map[string]bool, len(request.SourceCollectionIDs))
	if err := matchSnapshotSourcesBatch(ctx, tx, request.SourceCollectionIDs, matched, request.Members); err != nil {
		return err
	}
	for _, id := range request.SourceCollectionIDs {
		if !matched[id] {
			return ErrPackageConflict
		}
	}
	return nil
}

func snapshotPDFRepresentationValid(member CollectionSnapshotMember) bool {
	for _, rep := range member.Representations {
		if rep.Status == roleAvailable && (rep.Role == "produced_pdf" || rep.Role == "native") &&
			rep.ContentVersionID == member.ContentVersionID && rep.BlobSHA256 == member.SelectedPDFSHA256 &&
			rep.Size == member.Size && rep.MediaType == "application/pdf" &&
			rep.VerifiedPageCount == member.SourcePageCount {
			return true
		}
	}
	return false
}

func insertCollectionSnapshotTx(ctx context.Context, tx *sql.Tx, vaultID string, snapshot CollectionSnapshot, members []CollectionSnapshotMember) error {
	if err := insertCollectionSnapshotHeaderTx(ctx, tx, vaultID, snapshot); err != nil {
		return err
	}
	return insertCollectionSnapshotMembersTx(ctx, tx, snapshot.SnapshotID, members)
}

func insertCollectionSnapshotHeaderTx(ctx context.Context, tx *sql.Tx, vaultID string, snapshot CollectionSnapshot) error {
	_, header, err := snapshotRowChecksum(snapshot)
	if err != nil {
		return err
	}
	sourceJSON, err := canonical.Marshal(snapshot.SourceCollectionIDs)
	if err != nil {
		return err
	}
	var predecessor any
	if snapshot.PredecessorID != "" {
		predecessor = snapshot.PredecessorID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO collection_snapshots(
		snapshot_id,vault_uid,predecessor_id,source_collection_ids_json,
		member_count,page_count,member_hash,manifest_sha256,canonical_json,checksum,sealed_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, snapshot.SnapshotID, vaultID, predecessor,
		sourceJSON, snapshot.MemberCount, snapshot.PageCount, snapshot.MemberHash,
		snapshot.ManifestSHA256, header, snapshot.Checksum, snapshot.SealedAt)
	if err != nil {
		return err
	}
	return nil
}

func insertCollectionSnapshotMembersTx(ctx context.Context, tx *sql.Tx, snapshotID string, members []CollectionSnapshotMember) error {
	for _, member := range members {
		representations := member.Representations
		member.Representations = nil
		encoded, checksum, err := snapshotRowBytes(member)
		if err != nil {
			return err
		}
		selectedJSON, err := canonical.Marshal(member.SelectedSourcePages)
		if err != nil {
			return err
		}
		var selected any
		if member.SelectedSourcePages != nil {
			selected = selectedJSON
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO collection_snapshot_members(
			snapshot_id,ordinal,occurrence_id,node_id,content_version_id,blob_sha256,size,
			family_id,parent_occurrence_id,family_order,display_name,frozen_fields_json,
			document_kind,selected_source_pages_json,selected_pdf_sha256,source_page_count,
			canonical_json,checksum
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, snapshotID, member.Ordinal,
			member.OccurrenceID, member.NodeID, member.ContentVersionID, member.BlobSHA256,
			member.Size, member.FamilyID, member.ParentOccurrenceID, member.FamilyOrder,
			member.DisplayName, member.FrozenFieldsJSON, member.DocumentKind, selected,
			member.SelectedPDFSHA256, member.SourcePageCount, encoded, checksum)
		if err != nil {
			return err
		}
		for _, rep := range representations {
			encoded, checksum, err := snapshotRowBytes(rep)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO collection_snapshot_representations(
				snapshot_id,occurrence_id,role,ordinal,status,text_authority,content_version_id,
				blob_sha256,size,media_type,page_number,verified_page_count,rendition_build_id,
				lexical_generation_id,recipe_sha256,canonical_json,checksum
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, snapshotID, rep.OccurrenceID,
				rep.Role, rep.Ordinal, rep.Status, rep.TextAuthority, rep.ContentVersionID,
				rep.BlobSHA256, rep.Size, rep.MediaType, rep.PageNumber, rep.VerifiedPageCount,
				rep.RenditionBuildID, rep.LexicalGenerationID, rep.RecipeSHA256, encoded, checksum)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func loadCollectionSnapshotTx(ctx context.Context, tx metadataQuerier, id string) (CollectionSnapshot, error) {
	var encoded []byte
	var checksum string
	err := tx.QueryRowContext(ctx, `SELECT canonical_json,checksum FROM collection_snapshots WHERE snapshot_id=?`, id).Scan(&encoded, &checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return CollectionSnapshot{}, ErrNotFound
	}
	if err != nil {
		return CollectionSnapshot{}, err
	}
	snapshot, err := canonical.Decode[CollectionSnapshot](encoded)
	if err != nil {
		return CollectionSnapshot{}, err
	}
	hash := sha256.Sum256(encoded)
	if hex.EncodeToString(hash[:]) != checksum || snapshot.SnapshotID != id {
		return CollectionSnapshot{}, ErrPackageConflict
	}
	snapshot.Checksum = checksum
	return snapshot, nil
}

func (s *Store) CollectionSnapshot(ctx context.Context, id string) (CollectionSnapshot, error) {
	if validateUUIDv4(id) != nil {
		return CollectionSnapshot{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CollectionSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot, err := loadCollectionSnapshotTx(ctx, tx, id)
	if err != nil {
		return CollectionSnapshot{}, err
	}
	return snapshot, tx.Commit()
}

func (s *Store) SnapshotMembers(ctx context.Context, id string, afterOrdinal, limit int) ([]CollectionSnapshotMember, error) {
	if validateUUIDv4(id) != nil || afterOrdinal < 0 || limit < 1 || limit > 250 {
		return nil, ErrPackageConflict
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	members, err := loadSnapshotMemberRows(ctx, tx, id, afterOrdinal, limit)
	if err != nil {
		return nil, err
	}
	for i := range members {
		members[i].Representations, err = loadSnapshotRepresentationRows(ctx, tx, id, members[i].OccurrenceID)
		if err != nil {
			return nil, err
		}
	}
	return members, tx.Commit()
}

func loadSnapshotMemberRows(ctx context.Context, tx metadataQuerier, id string, afterOrdinal, limit int) ([]CollectionSnapshotMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT canonical_json,checksum,selected_source_pages_json FROM collection_snapshot_members
		WHERE snapshot_id=? AND ordinal>? ORDER BY ordinal LIMIT ?`, id, afterOrdinal, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	members := make([]CollectionSnapshotMember, 0)
	for rows.Next() {
		var encoded []byte
		var checksum string
		var selected []byte
		if err := rows.Scan(&encoded, &checksum, &selected); err != nil {
			return nil, err
		}
		member, err := canonical.Decode[CollectionSnapshotMember](encoded)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(encoded)
		if hex.EncodeToString(hash[:]) != checksum {
			return nil, ErrPackageConflict
		}
		if selected == nil {
			member.SelectedSourcePages = nil
		}
		if len(member.SourceCollectionIDs) == 0 {
			member.SourceCollectionIDs = nil
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return members, nil
}

func loadSnapshotRepresentationRows(ctx context.Context, tx metadataQuerier, snapshotID, occurrenceID string) ([]CollectionSnapshotRepresentation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT canonical_json,checksum FROM collection_snapshot_representations
		WHERE snapshot_id=? AND occurrence_id=? ORDER BY role,ordinal`, snapshotID, occurrenceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var representations []CollectionSnapshotRepresentation
	for rows.Next() {
		var encoded []byte
		var checksum string
		if err := rows.Scan(&encoded, &checksum); err != nil {
			return nil, err
		}
		rep, err := canonical.Decode[CollectionSnapshotRepresentation](encoded)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(encoded)
		if hex.EncodeToString(hash[:]) != checksum || rep.OccurrenceID != occurrenceID {
			return nil, ErrPackageConflict
		}
		representations = append(representations, rep)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return representations, nil
}
