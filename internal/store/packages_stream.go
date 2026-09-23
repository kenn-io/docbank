package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	maxSnapshotMemberRow  = 1 << 20
	snapshotInsertBatch   = 1_000
	maxSnapshotBatchBytes = 4 << 20
)

// SnapshotSealHeader names the immutable snapshot being sealed. Members are
// supplied separately as canonical CollectionSnapshotMember JSONL.
type SnapshotSealHeader struct {
	SnapshotID          string
	PredecessorID       string
	SourceCollectionIDs []string
}

// SealCollectionSnapshotStream seals a JSONL member stream atomically. Each
// line is bounded to 1 MiB; batches of at most 1,000 rows are retained in
// memory while the canonical manifest digest covers every member in order.
// The inline SealCollectionSnapshot request keeps its 64 KiB manifest cap.
func (s *Store) SealCollectionSnapshotStream(ctx context.Context, header SnapshotSealHeader, reader io.Reader) (CollectionSnapshot, error) {
	if reader == nil || validateUUIDv4(header.SnapshotID) != nil || len(header.SourceCollectionIDs) > 64 ||
		header.PredecessorID != "" && validateUUIDv4(header.PredecessorID) != nil {
		return CollectionSnapshot{}, ErrPackageConflict
	}
	header.SourceCollectionIDs = append([]string{}, header.SourceCollectionIDs...)
	slices.Sort(header.SourceCollectionIDs)
	header.SourceCollectionIDs = slices.Compact(header.SourceCollectionIDs)
	for _, id := range header.SourceCollectionIDs {
		if validateUUIDv4(id) != nil {
			return CollectionSnapshot{}, ErrPackageConflict
		}
	}
	sources, err := canonical.Marshal(header.SourceCollectionIDs)
	if err != nil {
		return CollectionSnapshot{}, err
	}
	var result CollectionSnapshot
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		// Children can be inserted before the immutable header only inside this
		// transaction. The deferred foreign keys are checked at commit.
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return err
		}
		existing, loadErr := loadCollectionSnapshotTx(ctx, tx, header.SnapshotID)
		if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
			return loadErr
		}
		replay := loadErr == nil
		digest := sha256.New()
		_, _ = digest.Write([]byte(`{"members":[`))
		input := bufio.NewReaderSize(reader, maxSnapshotMemberRow+1)
		parents := make(map[string]string)
		families := make(map[string]string)
		nodeVersions := make(map[int64]string)
		matchedSources := make(map[string]bool, len(header.SourceCollectionIDs))
		batch := make([]CollectionSnapshotMember, 0, snapshotInsertBatch)
		batchBytes := 0
		memberCount, pageCount := 0, 0
		flush := func() error {
			if replay || len(batch) == 0 {
				batch = batch[:0]
				batchBytes = 0
				return nil
			}
			if err := matchSnapshotSourcesBatch(ctx, tx, header.SourceCollectionIDs, matchedSources, batch); err != nil {
				return err
			}
			if err := validateSnapshotMembersTx(ctx, tx, SnapshotSealRequest{Members: batch}); err != nil {
				return err
			}
			if err := insertCollectionSnapshotMembersTx(ctx, tx, header.SnapshotID, batch); err != nil {
				return err
			}
			batch = batch[:0]
			batchBytes = 0
			return nil
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			line, readErr := input.ReadSlice('\n')
			if errors.Is(readErr, bufio.ErrBufferFull) || len(line) > maxSnapshotMemberRow+1 {
				return ErrPackageConflict
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read snapshot member: %w", readErr)
			}
			if len(line) == 0 && errors.Is(readErr, io.EOF) {
				break
			}
			if memberCount == MaxSnapshotMembers {
				return ErrPackageConflict
			}
			line = bytes.TrimSuffix(line, []byte{'\n'})
			if len(line) == 0 || len(line) > maxSnapshotMemberRow {
				return ErrPackageConflict
			}
			member, err := decodeSnapshotMemberLine(line, memberCount+1)
			if err != nil {
				return err
			}
			if len(member.SourceCollectionIDs) == 0 && len(header.SourceCollectionIDs) == 1 {
				member.SourceCollectionIDs = slices.Clone(header.SourceCollectionIDs)
			}
			if len(header.SourceCollectionIDs) == 0 && len(member.SourceCollectionIDs) != 0 ||
				len(header.SourceCollectionIDs) != 0 && (len(member.SourceCollectionIDs) == 0 ||
					!sourceIDsContained(header.SourceCollectionIDs, member.SourceCollectionIDs)) {
				return ErrPackageConflict
			}
			if _, exists := parents[member.OccurrenceID]; exists || nodeVersions[member.NodeID] != "" || member.NodeID <= 0 ||
				validateUUIDv4(member.ContentVersionID) != nil {
				return ErrPackageConflict
			}
			parents[member.OccurrenceID] = member.ParentOccurrenceID
			families[member.OccurrenceID] = member.FamilyID
			nodeVersions[member.NodeID] = member.ContentVersionID
			encoded, err := canonical.Marshal(member)
			if err != nil || len(encoded) > maxSnapshotMemberRow {
				return ErrPackageConflict
			}
			if memberCount > 0 {
				_, _ = digest.Write([]byte{','})
			}
			_, _ = digest.Write(encoded)
			memberCount++
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
				return ErrPackageConflict
			}
			batch = append(batch, member)
			batchBytes += len(encoded)
			if len(batch) == snapshotInsertBatch || batchBytes >= maxSnapshotBatchBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
		if memberCount == 0 || !validSnapshotFamilyGraph(parents, families) {
			return ErrPackageConflict
		}
		if err := flush(); err != nil {
			return err
		}
		for _, id := range header.SourceCollectionIDs {
			if !replay && !matchedSources[id] {
				return ErrPackageConflict
			}
		}
		_, _ = digest.Write([]byte(`],"source_collection_ids":`))
		_, _ = digest.Write(sources)
		_, _ = digest.Write([]byte{'}'})
		manifestHash := hex.EncodeToString(digest.Sum(nil))
		if replay {
			if existing.ManifestSHA256 != manifestHash || existing.PredecessorID != header.PredecessorID {
				return ErrPackageConflict
			}
			result = existing
			return nil
		}
		result = CollectionSnapshot{
			SnapshotID: header.SnapshotID, PredecessorID: header.PredecessorID,
			SourceCollectionIDs: header.SourceCollectionIDs, MemberCount: memberCount,
			PageCount: pageCount, MemberHash: snapshotNodeVersionHash(nodeVersions),
			ManifestSHA256: manifestHash, SealedAt: nowRFC3339(),
		}
		result.Checksum, _, err = snapshotRowChecksum(result)
		if err != nil {
			return err
		}
		return insertCollectionSnapshotHeaderTx(ctx, tx, s.vaultID, result)
	})
	return result, err
}

func decodeSnapshotMemberLine(line []byte, ordinal int) (CollectionSnapshotMember, error) {
	member, err := canonical.Decode[CollectionSnapshotMember](line)
	if err != nil {
		return CollectionSnapshotMember{}, fmt.Errorf("snapshot member %d: %w: %w", ordinal, ErrPackageConflict, err)
	}
	// canonical.Marshal emits an empty array for a nil slice. With no
	// source-page selection, decoding that array restores the nil meaning.
	if len(member.SelectedSourcePages) == 0 {
		member.SelectedSourcePages = nil
	}
	normalized, err := normalizeSnapshotMember(member, ordinal)
	if err != nil {
		return CollectionSnapshotMember{}, fmt.Errorf("snapshot member %d validation: %w", ordinal, err)
	}
	return normalized, nil
}

func snapshotNodeVersionHash(nodes map[int64]string) string {
	ids := make([]int64, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	digest := sha256.New()
	for _, id := range ids {
		_, _ = fmt.Fprintf(digest, "%d:%s\n", id, nodes[id])
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func validSnapshotFamilyGraph(parents, families map[string]string) bool {
	for _, family := range families {
		if parent, ok := parents[family]; !ok || parent != "" || families[family] != family {
			return false
		}
	}
	validated := make(map[string]bool, len(parents))
	for occurrence := range parents {
		path := make(map[string]bool)
		current := occurrence
		for current != "" && !validated[current] {
			if path[current] {
				return false
			}
			path[current] = true
			parent, ok := parents[current]
			if !ok || parent != "" && families[current] != families[parent] {
				return false
			}
			current = parent
		}
		for id := range path {
			validated[id] = true
		}
	}
	return true
}

// matchSnapshotSourcesBatch checks active, current-version membership for a
// bounded set of members. The chunk limit stays below SQLite's 999-parameter
// floor even with all 64 declared source collections.
func matchSnapshotSourcesBatch(ctx context.Context, q metadataQuerier, sourceIDs []string, matched map[string]bool, members []CollectionSnapshotMember) error {
	if len(sourceIDs) == 0 || len(members) == 0 {
		return nil
	}
	const membershipBatch = 400
	for start := 0; start < len(members); start += membershipBatch {
		chunk := members[start:min(start+membershipBatch, len(members))]
		args := make([]any, 0, 2*len(chunk)+len(sourceIDs))
		for _, member := range chunk {
			args = append(args, member.NodeID, member.ContentVersionID)
		}
		for _, id := range sourceIDs {
			args = append(args, id)
		}
		query := `WITH requested_members(node_id,version_id) AS (VALUES ` +
			strings.TrimSuffix(strings.Repeat("(?,?),", len(chunk)), ",") +
			`), requested_sources(ingest_id) AS (VALUES ` +
			strings.TrimSuffix(strings.Repeat("(?),", len(sourceIDs)), ",") +
			`), ` + CollectionMembershipCTE + `
			SELECT DISTINCT rm.node_id, cm.ingest_id FROM requested_members rm
			JOIN nodes n ON n.id=rm.node_id AND n.current_version_id=rm.version_id
			JOIN collection_members cm ON cm.node_id=rm.node_id
			JOIN requested_sources rs ON rs.ingest_id=cm.ingest_id`
		memberSources := make(map[int64][]string, len(chunk))
		if err := func() error {
			rows, err := q.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var nodeID int64
				var sourceID string
				if err := rows.Scan(&nodeID, &sourceID); err != nil {
					return err
				}
				memberSources[nodeID] = append(memberSources[nodeID], sourceID)
				matched[sourceID] = true
			}
			return rows.Err()
		}(); err != nil {
			return err
		}
		for _, member := range chunk {
			sources := memberSources[member.NodeID]
			slices.Sort(sources)
			if !slices.Equal(sources, member.SourceCollectionIDs) {
				return ErrPackageConflict
			}
		}
	}
	return nil
}
