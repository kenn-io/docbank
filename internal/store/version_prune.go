package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// MaxVersionPruneIDs bounds explicit point selections. Larger history cleanup
// should use an age, count, or all-prior selector instead of holding a write
// transaction for an unbounded request body.
const MaxVersionPruneIDs = 1000

// VersionPruneSelector chooses historical content versions. Exactly one mode
// must be set. The current head is never removable; AllPrior may replace a
// current revert with a same-byte checkpoint so its complete source graph can
// be released.
type VersionPruneSelector struct {
	VersionIDs []string
	KeepNewest int
	OlderThan  time.Duration
	AllPrior   bool
}

// VersionPruneResult is the complete dry-run inventory or execution receipt.
// LogicalBytes counts version references and may count a deduplicated blob
// more than once. UniqueBlobs includes both selected content blobs and visual
// preview outputs owned by the selected versions. ReleasableBytes counts
// unique blobs that become eligible for a later GC; pruning itself never
// reports physical bytes as reclaimed. Loose and packed maintenance counts may
// overlap when one blob has both location kinds across stores;
// MixedBlobsPendingMaintenance names that intersection.
type VersionPruneResult struct {
	Node                         Node
	Candidates                   []ContentVersion
	DependencyRetained           []ContentVersion
	Checkpoint                   *ContentVersion
	Cutoff                       string
	LogicalBytes                 int64
	UniqueBlobs                  int
	SharedBlobs                  int
	ReleasableBlobs              int
	ReleasableBytes              int64
	LooseBlobsPendingGC          int
	LooseBytesPendingGC          int64
	PackedBlobsPendingRepack     int
	PackedBytesPendingRepack     int64
	MixedBlobsPendingMaintenance int
	DeletedVersions              int
	CheckpointRequired           bool
	Changed                      bool
	Run                          bool
}

type pruneBlobStats struct {
	refs            int
	size            int64
	looseLocations  int
	looseStored     int64
	packedLocations int
	packedStored    int64
}

// PruneContentVersions previews or removes selected non-current history under
// an optimistic node revision. A run that changes history advances the node
// revision once. Revert-source dependencies remain retained unless AllPrior
// creates a new source-free checkpoint head in the same transaction.
func (s *Store) PruneContentVersions(
	ctx context.Context, nodeID, ifRev int64, selector VersionPruneSelector, run bool,
) (VersionPruneResult, error) {
	if err := ValidateVersionPruneSelector(selector); err != nil {
		return VersionPruneResult{}, err
	}
	result := VersionPruneResult{Run: run}
	runTx := s.withStorageTx
	if run {
		runTx = s.withLogicalTx
	}
	err := runTx(ctx, func(tx *sql.Tx) error {
		node, err := nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if err := validateContentReplacementTarget(node, ifRev); err != nil {
			return err
		}
		versions, err := contentVersionsForNodeTx(tx, nodeID)
		if err != nil {
			return err
		}
		if len(versions) == 0 || versions[0].ID != node.CurrentVersionID {
			return fmt.Errorf("node %d current content version is not its newest history row", nodeID)
		}

		candidateSet, retainedSet, cutoff, checkpointRequired, err :=
			selectVersionPruneCandidates(tx, node, versions, selector)
		if err != nil {
			return err
		}
		result.Node = node
		result.Cutoff = cutoff
		result.CheckpointRequired = checkpointRequired
		for _, version := range versions {
			if candidateSet[version.ID] {
				result.Candidates = append(result.Candidates, version)
				if version.Size > math.MaxInt64-result.LogicalBytes {
					return errors.New("selected content-version bytes exceed the reportable range")
				}
				result.LogicalBytes += version.Size
			}
			if retainedSet[version.ID] {
				result.DependencyRetained = append(result.DependencyRetained, version)
			}
		}
		if err := populateVersionPruneBlobStats(tx, &result, checkpointRequired); err != nil {
			return err
		}
		if !run || len(candidateSet) == 0 {
			return nil
		}
		if checkpointRequired {
			updated, checkpoint, err := installContentVersionTx(
				ctx, tx, node, versions[0].BlobHash, versions[0].Size, versions[0].MimeType,
				"content_replace", nil,
			)
			if err != nil {
				return fmt.Errorf("checkpointing node %d before pruning: %w", node.ID, err)
			}
			result.Node = updated
			result.Checkpoint = &checkpoint
		} else {
			now := nowRFC3339()
			if err := bumpRevisionTx(tx, node.ID, now); err != nil {
				return err
			}
			result.Node.Revision++
			result.Node.ModifiedAt = now
		}
		versionIDs := make([]string, len(result.Candidates))
		for index, version := range result.Candidates {
			versionIDs[index] = version.ID
		}
		if err := deleteRenditionAuthorityForVersionsTx(ctx, tx, versionIDs); err != nil {
			return err
		}
		for _, version := range result.Candidates {
			if _, err := tx.Exec(`DELETE FROM content_versions WHERE version_id = ?`, version.ID); err != nil {
				return fmt.Errorf("pruning content version %s: %w", version.ID, err)
			}
		}
		result.DeletedVersions = len(result.Candidates)
		result.Changed = true
		return nil
	})
	if err != nil {
		return VersionPruneResult{}, err
	}
	if result.Candidates == nil {
		result.Candidates = []ContentVersion{}
	}
	if result.DependencyRetained == nil {
		result.DependencyRetained = []ContentVersion{}
	}
	return result, nil
}

// ValidateVersionPruneSelector applies the authoritative store-level selector
// rules. HTTP and CLI adapters translate their inputs into this type and reuse
// the same validation before opening a transaction.
func ValidateVersionPruneSelector(selector VersionPruneSelector) error {
	if len(selector.VersionIDs) > MaxVersionPruneIDs {
		return fmt.Errorf("at most %d explicit version IDs may be pruned at once: %w",
			MaxVersionPruneIDs, ErrInvalidVersionPrune)
	}
	modes := 0
	if len(selector.VersionIDs) > 0 {
		modes++
	}
	if selector.KeepNewest != 0 {
		modes++
	}
	if selector.OlderThan != 0 {
		modes++
	}
	if selector.AllPrior {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("version pruning requires exactly one selector: version IDs, keep newest, older than, or all prior: %w",
			ErrInvalidVersionPrune)
	}
	if selector.KeepNewest < 0 {
		return fmt.Errorf("versions to keep must be positive: %w", ErrInvalidVersionPrune)
	}
	if selector.OlderThan < 0 {
		return fmt.Errorf("version age must not be negative: %w", ErrInvalidVersionPrune)
	}
	seen := make(map[string]bool, len(selector.VersionIDs))
	for _, id := range selector.VersionIDs {
		if err := validateUUIDv4(id); err != nil {
			return fmt.Errorf("content version %q must be a canonical UUIDv4: %w",
				id, ErrInvalidVersionPrune)
		}
		if seen[id] {
			return fmt.Errorf("content version %s is selected more than once: %w", id, ErrInvalidVersionPrune)
		}
		seen[id] = true
	}
	return nil
}

func contentVersionsForNodeTx(tx *sql.Tx, nodeID int64) ([]ContentVersion, error) {
	rows, err := tx.Query(
		`SELECT `+contentVersionCols+` FROM content_versions
		 WHERE node_id = ? ORDER BY node_revision DESC, version_id`, nodeID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing content versions of node %d for pruning: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	var versions []ContentVersion
	for rows.Next() {
		version, err := scanContentVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing content versions of node %d for pruning: %w", nodeID, err)
	}
	return versions, nil
}

func selectVersionPruneCandidates(
	tx *sql.Tx, node Node, versions []ContentVersion, selector VersionPruneSelector,
) (map[string]bool, map[string]bool, string, bool, error) {
	candidates := make(map[string]bool)
	cutoff := ""
	checkpointRequired := selector.AllPrior && versions[0].TransitionKind == "content_revert"
	switch {
	case len(selector.VersionIDs) > 0:
		byID := make(map[string]ContentVersion, len(versions))
		for _, version := range versions {
			byID[version.ID] = version
		}
		missing := make([]string, 0)
		for _, id := range selector.VersionIDs {
			_, ok := byID[id]
			if !ok {
				missing = append(missing, id)
				continue
			}
			if id == node.CurrentVersionID {
				return nil, nil, "", false, fmt.Errorf(
					"content version %s is the current head of node %d: %w",
					id, node.ID, ErrVersionAlreadyCurrent,
				)
			}
			candidates[id] = true
		}
		if len(missing) > 0 {
			owners, err := contentVersionOwnersTx(tx, missing)
			if err != nil {
				return nil, nil, "", false, err
			}
			id := missing[0]
			if owner, ok := owners[id]; ok {
				return nil, nil, "", false, fmt.Errorf(
					"content version %s belongs to node %d, not node %d: %w",
					id, owner, node.ID, ErrVersionNodeMismatch,
				)
			}
			return nil, nil, "", false, fmt.Errorf("content version %q: %w", id, ErrNotFound)
		}
	case selector.KeepNewest > 0:
		for index := selector.KeepNewest; index < len(versions); index++ {
			candidates[versions[index].ID] = true
		}
	case selector.OlderThan > 0:
		cutoff = time.Now().UTC().Add(-selector.OlderThan).Format(timestampLayout)
		for _, version := range versions[1:] {
			if version.RecordedAt <= cutoff {
				candidates[version.ID] = true
			}
		}
	case selector.AllPrior:
		start := 1
		if checkpointRequired {
			start = 0
		}
		for _, version := range versions[start:] {
			candidates[version.ID] = true
		}
	}

	dependencyRetained := make(map[string]bool)
	if !checkpointRequired {
		changed := true
		for changed {
			changed = false
			for _, version := range versions {
				if candidates[version.ID] || version.SourceVersionID == nil {
					continue
				}
				sourceID := *version.SourceVersionID
				if candidates[sourceID] {
					delete(candidates, sourceID)
					dependencyRetained[sourceID] = true
					changed = true
				}
			}
		}
	}
	return candidates, dependencyRetained, cutoff, checkpointRequired, nil
}

func contentVersionOwnersTx(tx *sql.Tx, versionIDs []string) (map[string]int64, error) {
	args := make([]any, len(versionIDs))
	for index, id := range versionIDs {
		args[index] = id
	}
	rows, err := tx.Query(`SELECT version_id, node_id FROM content_versions
		WHERE version_id IN (`+placeholders(len(versionIDs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("resolving selected content versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owners := make(map[string]int64, len(versionIDs))
	for rows.Next() {
		var id string
		var nodeID int64
		if err := rows.Scan(&id, &nodeID); err != nil {
			return nil, fmt.Errorf("resolving selected content versions: %w", err)
		}
		owners[id] = nodeID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolving selected content versions: %w", err)
	}
	return owners, nil
}

func populateVersionPruneBlobStats(
	tx *sql.Tx, result *VersionPruneResult, checkpointRequired bool,
) error {
	selectedByHash := make(map[string]int)
	versionIDs := make([]string, 0, len(result.Candidates))
	for _, version := range result.Candidates {
		selectedByHash[version.BlobHash]++
		versionIDs = append(versionIDs, version.ID)
	}
	if err := addVersionPruneVisualPreviewRefsTx(tx, versionIDs, selectedByHash); err != nil {
		return err
	}
	result.UniqueBlobs = len(selectedByHash)
	if len(selectedByHash) == 0 {
		return nil
	}
	hashes := make([]string, 0, len(selectedByHash))
	for hash := range selectedByHash {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	stats, err := versionPruneBlobStatsTx(tx, hashes)
	if err != nil {
		return err
	}
	for hash, selected := range selectedByHash {
		item, ok := stats[hash]
		if !ok {
			return fmt.Errorf("candidate blob %s lacks catalog authority", hash)
		}
		retained := item.refs - selected
		if checkpointRequired && hash == result.Node.BlobHash {
			retained++
		}
		if retained > 0 {
			result.SharedBlobs++
			continue
		}
		result.ReleasableBlobs++
		if item.size > math.MaxInt64-result.ReleasableBytes {
			return errors.New("releasable content-version bytes exceed the reportable range")
		}
		result.ReleasableBytes += item.size
		hasLoose := item.looseLocations > 0
		hasPacked := item.packedLocations > 0
		if !hasLoose && !hasPacked {
			return fmt.Errorf("candidate blob %s lacks physical authority", hash)
		}
		if hasLoose {
			result.LooseBlobsPendingGC++
			if item.looseStored > math.MaxInt64-result.LooseBytesPendingGC {
				return errors.New("loose content-version bytes exceed the reportable range")
			}
			result.LooseBytesPendingGC += item.looseStored
		}
		if hasPacked {
			result.PackedBlobsPendingRepack++
			if item.packedStored > math.MaxInt64-result.PackedBytesPendingRepack {
				return errors.New("packed content-version bytes exceed the reportable range")
			}
			result.PackedBytesPendingRepack += item.packedStored
		}
		if hasLoose && hasPacked {
			result.MixedBlobsPendingMaintenance++
		}
	}
	return nil
}

func addVersionPruneVisualPreviewRefsTx(
	tx *sql.Tx, versionIDs []string, selectedByHash map[string]int,
) error {
	const batchSize = 500
	for start := 0; start < len(versionIDs); start += batchSize {
		end := min(start+batchSize, len(versionIDs))
		if err := addVersionPruneVisualPreviewRefsBatchTx(
			tx, versionIDs[start:end], selectedByHash,
		); err != nil {
			return err
		}
	}
	return nil
}

func addVersionPruneVisualPreviewRefsBatchTx(
	tx *sql.Tx, versionIDs []string, selectedByHash map[string]int,
) (retErr error) {
	args := make([]any, len(versionIDs))
	for index, id := range versionIDs {
		args[index] = id
	}
	rows, err := tx.Query(`
		SELECT output_blob_hash, COUNT(*)
		FROM visual_preview_generations
		WHERE content_version_id IN (`+placeholders(len(args))+`)
		  AND output_blob_hash IS NOT NULL
		GROUP BY output_blob_hash`, args...)
	if err != nil {
		return fmt.Errorf("inventorying version-prune visual previews: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr,
				fmt.Errorf("closing version-prune visual preview inventory: %w", err))
		}
	}()
	for rows.Next() {
		var hash string
		var refs int
		if err := rows.Scan(&hash, &refs); err != nil {
			return fmt.Errorf("inventorying version-prune visual previews: %w", err)
		}
		selectedByHash[hash] += refs
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inventorying version-prune visual previews: %w", err)
	}
	return nil
}

func versionPruneBlobStatsTx(tx *sql.Tx, hashes []string) (map[string]pruneBlobStats, error) {
	const batchSize = 500
	stats := make(map[string]pruneBlobStats, len(hashes))
	for start := 0; start < len(hashes); start += batchSize {
		end := min(start+batchSize, len(hashes))
		if err := versionPruneBlobStatsBatchTx(tx, hashes[start:end], stats); err != nil {
			return nil, err
		}
	}
	return stats, nil
}

func versionPruneBlobStatsBatchTx(
	tx *sql.Tx, hashes []string, stats map[string]pruneBlobStats,
) error {
	args := make([]any, len(hashes))
	for index, hash := range hashes {
		args[index] = hash
	}
	rows, err := tx.Query(`
			SELECT refs.blob_hash, COUNT(*), b.size
			FROM (
			`+blobReferenceRowsSQL(blobRootReferences)+`
			) refs
			JOIN blobs b ON b.hash = refs.blob_hash
			WHERE refs.blob_hash IN (`+placeholders(len(hashes))+`)
			GROUP BY refs.blob_hash, b.size`, args...)
	if err != nil {
		return fmt.Errorf("inventorying version-prune blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash string
		var item pruneBlobStats
		if err := rows.Scan(&hash, &item.refs, &item.size); err != nil {
			return fmt.Errorf("inventorying version-prune blobs: %w", err)
		}
		stats[hash] = item
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inventorying version-prune blobs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing version-prune blob inventory: %w", err)
	}
	return inventoryVersionPruneLocationsTx(tx, hashes, stats)
}

func inventoryVersionPruneLocationsTx(
	tx *sql.Tx, hashes []string, stats map[string]pruneBlobStats,
) error {
	args := make([]any, len(hashes))
	for index, hash := range hashes {
		args[index] = hash
	}
	rows, err := tx.Query(`
		SELECT l.blob_hash, l.kind, l.stored_size, p.stored_len
		FROM blob_locations l
		LEFT JOIN blob_pack_entries p
		  ON p.blob_hash = l.blob_hash AND p.store_id = l.store_id
		WHERE l.blob_hash IN (`+placeholders(len(hashes))+`)
		ORDER BY l.blob_hash, l.store_id`, args...)
	if err != nil {
		return fmt.Errorf("inventorying version-prune locations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash, kind string
		var storedSize int64
		var packStored sql.NullInt64
		if err := rows.Scan(&hash, &kind, &storedSize, &packStored); err != nil {
			return fmt.Errorf("inventorying version-prune locations: %w", err)
		}
		item, ok := stats[hash]
		if !ok {
			return fmt.Errorf("version-prune location names unselected blob %s", hash)
		}
		switch kind {
		case blobLocationKindLoose:
			item.looseLocations++
			if storedSize > math.MaxInt64-item.looseStored {
				return errors.New("loose content-version bytes exceed the reportable range")
			}
			item.looseStored += storedSize
		case blobLocationKindPacked:
			if !packStored.Valid {
				return fmt.Errorf("inventorying version-prune blob %s: packed location lacks store-scoped pack entry", hash)
			}
			item.packedLocations++
			if packStored.Int64 > math.MaxInt64-item.packedStored {
				return errors.New("packed content-version bytes exceed the reportable range")
			}
			item.packedStored += packStored.Int64
		default:
			return fmt.Errorf("inventorying version-prune blob %s: invalid location kind %q", hash, kind)
		}
		stats[hash] = item
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inventorying version-prune locations: %w", err)
	}
	return nil
}
