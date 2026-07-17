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
// more than once. ReleasableBytes counts unique blobs that become eligible for
// a later GC; pruning itself never reports physical bytes as reclaimed.
type VersionPruneResult struct {
	Node                     Node
	Candidates               []ContentVersion
	DependencyRetained       []ContentVersion
	Checkpoint               *ContentVersion
	Cutoff                   string
	LogicalBytes             int64
	UniqueBlobs              int
	SharedBlobs              int
	ReleasableBlobs          int
	ReleasableBytes          int64
	LooseBlobsPendingGC      int
	LooseBytesPendingGC      int64
	PackedBlobsPendingRepack int
	PackedBytesPendingRepack int64
	DeletedVersions          int
	CheckpointRequired       bool
	Changed                  bool
	Run                      bool
}

type pruneBlobStats struct {
	refs      int
	size      int64
	packed    bool
	storedLen int64
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
				tx, node, versions[0].BlobHash, versions[0].Size, versions[0].MimeType,
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
	for _, version := range result.Candidates {
		selectedByHash[version.BlobHash]++
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
			return fmt.Errorf("candidate content blob %s lacks catalog authority", hash)
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
		if item.packed {
			result.PackedBlobsPendingRepack++
			if item.storedLen > math.MaxInt64-result.PackedBytesPendingRepack {
				return errors.New("packed content-version bytes exceed the reportable range")
			}
			result.PackedBytesPendingRepack += item.storedLen
		} else {
			result.LooseBlobsPendingGC++
			if item.size > math.MaxInt64-result.LooseBytesPendingGC {
				return errors.New("loose content-version bytes exceed the reportable range")
			}
			result.LooseBytesPendingGC += item.size
		}
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
			SELECT v.blob_hash, COUNT(*), b.size,
			       CASE WHEN p.blob_hash IS NULL THEN 0 ELSE 1 END,
			       COALESCE(p.stored_len, 0)
			FROM content_versions v
			JOIN blobs b ON b.hash = v.blob_hash
			LEFT JOIN blob_pack_index p ON p.blob_hash = v.blob_hash
			WHERE v.blob_hash IN (`+placeholders(len(hashes))+`)
			GROUP BY v.blob_hash, b.size, p.blob_hash, p.stored_len`, args...)
	if err != nil {
		return fmt.Errorf("inventorying version-prune blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var hash string
		var item pruneBlobStats
		if err := rows.Scan(&hash, &item.refs, &item.size, &item.packed, &item.storedLen); err != nil {
			return fmt.Errorf("inventorying version-prune blobs: %w", err)
		}
		stats[hash] = item
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inventorying version-prune blobs: %w", err)
	}
	return nil
}
