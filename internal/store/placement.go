package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	"go.kenn.io/kit/packstore"
)

type PlacementRequest struct {
	TargetNodeID           int64  `json:"target_node_id"`
	SourceStoreID          string `json:"source_store_id"`
	DestinationStoreID     string `json:"destination_store_id"`
	RetireSource           bool   `json:"retire_source"`
	Evacuate               bool   `json:"evacuate,omitempty"`
	AllowAuditedRemoteOnly bool   `json:"allow_audited_remote_only"`
}

type PlacementHash struct {
	Hash                string                  `json:"hash"`
	Size                int64                   `json:"size"`
	SelectedReferences  int64                   `json:"selected_references"`
	TotalReferences     int64                   `json:"total_references"`
	Source              packstore.ReadLocation  `json:"source"`
	Destination         *packstore.ReadLocation `json:"destination,omitempty"`
	SharedReference     bool                    `json:"shared_reference"`
	AuditPinned         bool                    `json:"audit_pinned"`
	RetireSource        bool                    `json:"retire_source"`
	PackRepackRequired  bool                    `json:"pack_repack_required"`
	UnavailableAtSource bool                    `json:"unavailable_at_source"`
	ScratchBytes        int64                   `json:"scratch_bytes,omitempty"`
}

type PlacementPlan struct {
	Version             int              `json:"version"`
	Request             PlacementRequest `json:"request"`
	Digest              string           `json:"digest"`
	Hashes              []PlacementHash  `json:"hashes"`
	SelectedNodes       int64            `json:"selected_nodes"`
	SelectedVersions    int64            `json:"selected_versions"`
	LogicalBytes        int64            `json:"logical_bytes"`
	TransferBytes       int64            `json:"transfer_bytes"`
	ReadBackBytes       int64            `json:"read_back_bytes"`
	RemoteEgressBytes   int64            `json:"remote_egress_bytes"`
	ScratchBytes        int64            `json:"scratch_bytes"`
	AlreadyPresentBytes int64            `json:"already_present_bytes"`
	RetirableBytes      int64            `json:"retirable_bytes"`
	SharedBytes         int64            `json:"shared_bytes"`
	AuditPinnedBytes    int64            `json:"audit_pinned_bytes"`
	PackBlockedBytes    int64            `json:"pack_blocked_bytes"`
}

type PlacementCommit struct {
	DestinationAuthorized bool                 `json:"destination_authorized"`
	SourceRevoked         bool                 `json:"source_revoked"`
	ReferenceDrift        bool                 `json:"reference_drift"`
	AuditPinned           bool                 `json:"audit_pinned"`
	PackRepackRequired    bool                 `json:"pack_repack_required"`
	Retire                *packstore.ObjectRef `json:"-"`
}

func (s *Store) PlanPlacement(
	ctx context.Context, request PlacementRequest,
) (PlacementPlan, error) {
	if request.TargetNodeID <= 0 || request.SourceStoreID == "" ||
		request.DestinationStoreID == "" ||
		request.SourceStoreID == request.DestinationStoreID {
		return PlacementPlan{}, errors.New("placement requires a target and distinct source/destination stores")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PlacementPlan{}, fmt.Errorf("pinning placement preview: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	sourceStore, destinationStore, err := placementStoresTx(ctx, tx, request)
	if err != nil {
		return PlacementPlan{}, err
	}
	if request.Evacuate &&
		(!request.RetireSource || request.TargetNodeID != s.rootID ||
			sourceStore.Role == blobStoreRolePrimary ||
			destinationStore.Role != blobStoreRolePrimary) {
		return PlacementPlan{}, errors.New(
			"evacuation requires the vault root, a secondary source, the primary destination, and source retirement",
		)
	}
	members, err := placementMembersTx(ctx, tx, request.TargetNodeID)
	if err != nil {
		return PlacementPlan{}, err
	}
	selected := make(map[int64]bool, len(members))
	for _, id := range members {
		selected[id] = true
	}
	plan := PlacementPlan{Version: 1, Request: request, SelectedNodes: int64(len(members))}
	hashes, err := placementHashesTx(ctx, tx, selected)
	if err != nil {
		return PlacementPlan{}, err
	}
	if request.Evacuate {
		hashes, err = evacuationHashesTx(ctx, tx, request.SourceStoreID, hashes)
		if err != nil {
			return PlacementPlan{}, err
		}
	}
	plan.Hashes = make([]PlacementHash, 0, len(hashes))
	for index := range hashes {
		item := &hashes[index]
		source, destination, err := placementLocationsTx(
			ctx, tx, item.Hash, request.SourceStoreID, request.DestinationStoreID,
		)
		if err != nil {
			return PlacementPlan{}, err
		}
		// Evacuation owns only authority currently recorded in its source.
		// Preview happens before the source transitions to draining.
		if request.Evacuate && source.StoreID == "" {
			continue
		}
		item.Source = source
		if destination.StoreID != "" {
			item.Destination = &destination
		}
		item.UnavailableAtSource = source.StoreID == ""
		if item.UnavailableAtSource && item.Destination == nil {
			return PlacementPlan{}, fmt.Errorf(
				"placement blob %s is absent from both requested stores: %w",
				item.Hash, packstore.ErrPhysicalAuthorityMissing,
			)
		}
		item.SharedReference = item.TotalReferences > item.SelectedReferences
		item.RetireSource = request.RetireSource && !item.UnavailableAtSource &&
			!item.SharedReference &&
			(!item.AuditPinned || request.SourceStoreID != s.primaryStoreID ||
				request.AllowAuditedRemoteOnly)
		if item.RetireSource && source.Pack != nil {
			item.PackRepackRequired = true
		}
		plan.SelectedVersions += item.SelectedReferences
		plan.LogicalBytes += item.Size * item.SelectedReferences
		switch {
		case item.Destination != nil:
			plan.AlreadyPresentBytes += item.Size
			plan.ReadBackBytes += item.Size
			if destinationStore.Kind == blobStoreKindS3 {
				destinationReadBytes := item.Size
				if destination.Pack != nil {
					destinationReadBytes, err = blobPackStoredBytesTx(
						ctx, tx, request.DestinationStoreID,
						destination.Pack.PackID,
					)
					if err != nil {
						return PlacementPlan{}, err
					}
					item.ScratchBytes = destinationReadBytes
					plan.ScratchBytes = max(plan.ScratchBytes, destinationReadBytes)
				} else if destination.Loose != nil {
					destinationReadBytes = destination.Loose.StoredSize
				}
				plan.RemoteEgressBytes += destinationReadBytes
			}
		case !item.UnavailableAtSource:
			plan.TransferBytes += item.Size
			plan.ReadBackBytes += item.Size
			if sourceStore.Kind == blobStoreKindS3 {
				sourceReadBytes := item.Size
				if source.Pack != nil {
					sourceReadBytes, err = blobPackStoredBytesTx(
						ctx, tx, request.SourceStoreID, source.Pack.PackID,
					)
					if err != nil {
						return PlacementPlan{}, err
					}
					item.ScratchBytes = sourceReadBytes
					plan.ScratchBytes = max(plan.ScratchBytes, sourceReadBytes)
				}
				plan.RemoteEgressBytes += sourceReadBytes
			}
			if destinationStore.Kind == blobStoreKindS3 {
				plan.RemoteEgressBytes += item.Size
			}
		}
		switch {
		case item.PackRepackRequired:
			plan.PackBlockedBytes += item.Size
		case item.RetireSource:
			plan.RetirableBytes += item.Size
		case item.SharedReference:
			plan.SharedBytes += item.Size
		case item.AuditPinned && request.SourceStoreID == s.primaryStoreID &&
			!request.AllowAuditedRemoteOnly:
			plan.AuditPinnedBytes += item.Size
		}
		plan.Hashes = append(plan.Hashes, *item)
	}
	digest, err := placementPlanDigest(plan)
	if err != nil {
		return PlacementPlan{}, err
	}
	plan.Digest = digest
	if err := tx.Commit(); err != nil {
		return PlacementPlan{}, fmt.Errorf("committing placement preview snapshot: %w", err)
	}
	return plan, nil
}

func blobPackStoredBytesTx(
	ctx context.Context, tx *sql.Tx, storeID, packID string,
) (int64, error) {
	var storedBytes int64
	if err := tx.QueryRowContext(ctx, `
		SELECT stored_bytes FROM blob_packs
		WHERE store_id=? AND pack_id=?`,
		storeID, packID,
	).Scan(&storedBytes); err != nil {
		return 0, fmt.Errorf("reading placement source pack %s: %w", packID, err)
	}
	if storedBytes < 0 {
		return 0, fmt.Errorf("placement source pack %s has invalid size", packID)
	}
	return storedBytes, nil
}

func placementStoresTx(
	ctx context.Context, tx *sql.Tx, request PlacementRequest,
) (BlobStore, BlobStore, error) {
	var source, destination BlobStore
	for _, candidate := range []struct {
		role string
		id   string
	}{
		{role: "source", id: request.SourceStoreID},
		{role: "destination", id: request.DestinationStoreID},
	} {
		store, err := blobStoreBySelectorTx(ctx, tx, candidate.id)
		if err != nil {
			return BlobStore{}, BlobStore{}, fmt.Errorf(
				"%s blob store: %w", candidate.role, err,
			)
		}
		if store.ID != candidate.id {
			return BlobStore{}, BlobStore{}, fmt.Errorf(
				"%s blob store selector must be a stable ID", candidate.role,
			)
		}
		validLifecycle := store.Lifecycle == blobStoreLifecycleActive ||
			(candidate.role == "source" &&
				store.Lifecycle == blobStoreLifecycleDraining)
		if !validLifecycle {
			return BlobStore{}, BlobStore{}, fmt.Errorf("%s blob store is %s: %w",
				candidate.role, store.Lifecycle, ErrBlobStoreState)
		}
		if candidate.role == "source" {
			source = store
		} else {
			destination = store
		}
	}
	return source, destination, nil
}

func placementMembersTx(
	ctx context.Context, tx *sql.Tx, targetID int64,
) ([]int64, error) {
	node, err := nodeByIDTx(tx, targetID)
	if err != nil {
		return nil, err
	}
	if node.TrashedAt != nil {
		return nil, fmt.Errorf("placement target %d must be live: %w", targetID, ErrNotFound)
	}
	if !node.IsDir() {
		return []int64{node.ID}, nil
	}
	auditTargetID, err := positiveAuditNodeID(targetID)
	if err != nil {
		return nil, err
	}
	auditMembers, err := deriveInitialAuditMembers(ctx, tx, auditTargetID)
	if err != nil {
		return nil, err
	}
	members := make([]int64, len(auditMembers))
	for index, id := range auditMembers {
		if id > math.MaxInt64 {
			return nil, fmt.Errorf("placement member %d exceeds signed node range", id)
		}
		members[index] = int64(id)
	}
	return members, nil
}

func placementHashesTx(
	ctx context.Context, tx *sql.Tx, selected map[int64]bool,
) ([]PlacementHash, error) {
	audited := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT version.blob_hash
		FROM content_versions version
		JOIN audit_memberships membership ON membership.node_id=version.node_id
		ORDER BY version.blob_hash`)
	if err != nil {
		return nil, fmt.Errorf("reading audit-pinned placement hashes: %w", err)
	}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning audit-pinned placement hash: %w", err)
		}
		audited[hash] = true
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing audit-pinned placement hashes: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `
		SELECT version.node_id,version.blob_hash,version.size
		FROM content_versions version
		ORDER BY version.blob_hash,version.node_id,version.version_id`)
	if err != nil {
		return nil, fmt.Errorf("reading placement references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type counts struct {
		size     int64
		selected int64
		total    int64
	}
	byHash := make(map[string]counts)
	for rows.Next() {
		var nodeID, size int64
		var hash string
		if err := rows.Scan(&nodeID, &hash, &size); err != nil {
			return nil, fmt.Errorf("scanning placement reference: %w", err)
		}
		current := byHash[hash]
		if current.total > 0 && current.size != size {
			return nil, fmt.Errorf("blob %s has inconsistent version sizes", hash)
		}
		current.size = size
		current.total++
		if selected[nodeID] {
			current.selected++
		}
		byHash[hash] = current
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading placement references: %w", err)
	}
	result := make([]PlacementHash, 0, len(byHash))
	for hash, count := range byHash {
		if count.selected == 0 {
			continue
		}
		result = append(result, PlacementHash{
			Hash: hash, Size: count.size, SelectedReferences: count.selected,
			TotalReferences: count.total, AuditPinned: audited[hash],
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hash < result[j].Hash })
	return result, nil
}

func evacuationHashesTx(
	ctx context.Context,
	tx *sql.Tx,
	sourceStoreID string,
	referenced []PlacementHash,
) ([]PlacementHash, error) {
	byHash := make(map[string]PlacementHash, len(referenced))
	for _, item := range referenced {
		byHash[item.Hash] = item
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT blob.hash,blob.size
		FROM blobs blob
		JOIN blob_locations location ON location.blob_hash=blob.hash
		WHERE location.store_id=?
		ORDER BY blob.hash`, sourceStoreID)
	if err != nil {
		return nil, fmt.Errorf("reading evacuation source authority: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]PlacementHash, 0, len(referenced))
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return nil, fmt.Errorf("scanning evacuation source authority: %w", err)
		}
		item, ok := byHash[hash]
		if ok && item.Size != size {
			return nil, fmt.Errorf("blob %s has inconsistent evacuation size", hash)
		}
		if !ok {
			item = PlacementHash{Hash: hash, Size: size}
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading evacuation source authority: %w", err)
	}
	return result, nil
}

func placementLocationsTx(
	ctx context.Context, tx *sql.Tx, hash, sourceID, destinationID string,
) (packstore.ReadLocation, packstore.ReadLocation, error) {
	parsed, err := packstore.ParseHash(hash)
	if err != nil {
		return packstore.ReadLocation{}, packstore.ReadLocation{},
			fmt.Errorf("invalid placement hash: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT b.size,l.store_id,l.generation,l.kind,l.encoding,l.stored_size,l.pack_eligible,
		       e.pack_id,e.pack_offset,e.stored_len,e.raw_len,e.flags,e.crc32c
		FROM blobs b JOIN blob_locations l ON l.blob_hash=b.hash
		LEFT JOIN blob_pack_entries e
		  ON e.blob_hash=l.blob_hash AND e.store_id=l.store_id
		WHERE b.hash=? AND l.store_id IN (?,?)
		ORDER BY l.store_id`, hash, sourceID, destinationID)
	if err != nil {
		return packstore.ReadLocation{}, packstore.ReadLocation{},
			fmt.Errorf("reading placement locations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var source packstore.ReadLocation
	var destination packstore.ReadLocation
	for rows.Next() {
		location, present, err := scanBlobReadLocation(rows, parsed)
		if err != nil {
			return packstore.ReadLocation{}, packstore.ReadLocation{}, err
		}
		if !present {
			continue
		}
		switch string(location.StoreID) {
		case sourceID:
			source = location
		case destinationID:
			destination = location
		}
	}
	if err := rows.Err(); err != nil {
		return packstore.ReadLocation{}, packstore.ReadLocation{},
			fmt.Errorf("reading placement locations: %w", err)
	}
	return source, destination, nil
}

func placementPlanDigest(plan PlacementPlan) (string, error) {
	plan.Digest = ""
	data, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encoding placement plan: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func ValidatePlacementPlan(plan PlacementPlan) error {
	if plan.Version != 1 || plan.Digest == "" {
		return errors.New("placement plan version or digest is invalid")
	}
	for index := range plan.Hashes {
		if index > 0 && plan.Hashes[index-1].Hash >= plan.Hashes[index].Hash {
			return errors.New("placement plan hashes are not strictly ordered")
		}
		if plan.Hashes[index].ScratchBytes < 0 {
			return errors.New("placement plan scratch requirement is negative")
		}
		if _, err := packstore.ParseHash(plan.Hashes[index].Hash); err != nil {
			return fmt.Errorf("placement plan hash: %w", err)
		}
	}
	want, err := placementPlanDigest(plan)
	if err != nil {
		return err
	}
	if want != plan.Digest {
		return errors.New("placement plan digest does not match its contents")
	}
	return nil
}

// CommitPlacement grants a fully verified destination receipt and, when the
// current reference closure still permits it, revokes one loose source in the
// same short catalog transaction. Network I/O belongs before this boundary.
func (s *Store) CommitPlacement(
	ctx context.Context,
	operationID string,
	request PlacementRequest,
	planned PlacementHash,
	destination packstore.ReadLocation,
) (PlacementCommit, error) {
	if err := validateUUIDv4(operationID); err != nil {
		return PlacementCommit{}, fmt.Errorf("invalid storage operation ID: %w", err)
	}
	if destination.StoreID != packstore.StoreID(request.DestinationStoreID) {
		return PlacementCommit{}, errors.New(
			"placement destination receipt must name the requested store",
		)
	}
	if err := destination.Validate(); err != nil {
		return PlacementCommit{}, fmt.Errorf("validating placement destination: %w", err)
	}
	var committed PlacementCommit
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		destinationStore, err := blobStoreBySelectorTx(
			ctx, tx, request.DestinationStoreID,
		)
		if err != nil {
			return fmt.Errorf("rechecking placement destination: %w", err)
		}
		if destinationStore.ID != request.DestinationStoreID ||
			destinationStore.Lifecycle != blobStoreLifecycleActive {
			return fmt.Errorf("placement destination store %s is %s: %w",
				destinationStore.ID, destinationStore.Lifecycle, ErrBlobStoreState)
		}
		var size int64
		err = tx.QueryRowContext(ctx,
			`SELECT size FROM blobs WHERE hash=?`, planned.Hash,
		).Scan(&size)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("rechecking placement membership %s: %w", planned.Hash, err)
		}
		var destinationSize int64
		if destination.Loose != nil {
			destinationSize = destination.Loose.LogicalSize
		} else {
			destinationSize = destination.Pack.RawLen
		}
		if size != planned.Size || destinationSize != size {
			return fmt.Errorf("placement identity changed for %s: %w",
				planned.Hash, packstore.ErrPhysicalCorrupt)
		}
		_, currentDestination, err := placementLocationsTx(
			ctx, tx, planned.Hash, request.SourceStoreID, request.DestinationStoreID,
		)
		if err != nil {
			return err
		}
		if currentDestination.StoreID != "" {
			if !sameReadLocation(currentDestination, destination) {
				return fmt.Errorf("placement destination authority changed for %s: %w",
					planned.Hash, ErrStaleRevision)
			}
		} else {
			if destination.Loose == nil || destination.Pack != nil ||
				destination.Loose.LogicalSize != size {
				return fmt.Errorf(
					"new placement destination must be one verified loose location: %w",
					packstore.ErrPhysicalCorrupt,
				)
			}
			encoding, err := looseEncodingName(destination.Loose.Encoding)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO blob_locations(
					blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
				) VALUES(?,?,?,?,?,?,?)
				ON CONFLICT(blob_hash,store_id) DO UPDATE SET
					generation=excluded.generation,kind=excluded.kind,
					encoding=excluded.encoding,stored_size=excluded.stored_size,
					pack_eligible=excluded.pack_eligible`,
				planned.Hash, request.DestinationStoreID, destination.Generation,
				blobLocationKindLoose, encoding, destination.Loose.StoredSize,
				size <= maxPackEligibleBytes,
			)
			if err != nil {
				return fmt.Errorf(
					"authorizing placement destination %s: %w", planned.Hash, err,
				)
			}
		}
		committed.DestinationAuthorized = true

		source, _, err := placementLocationsTx(
			ctx, tx, planned.Hash, request.SourceStoreID, request.DestinationStoreID,
		)
		if err != nil {
			return err
		}
		if source.StoreID == "" {
			committed.SourceRevoked = planned.RetireSource
			return nil
		}
		if !sameReadLocation(source, planned.Source) {
			committed.ReferenceDrift = true
			return nil
		}
		if !request.Evacuate {
			members, err := placementMembersTx(ctx, tx, request.TargetNodeID)
			if err != nil {
				committed.ReferenceDrift = true
				// A target moved out of the selectable topology after preview. The
				// verified destination remains an additional safe copy, while the
				// source stays authoritative.
				return nil //nolint:nilerr
			}
			selected := make(map[int64]bool, len(members))
			for _, id := range members {
				selected[id] = true
			}
			currentHashes, err := placementHashesTx(ctx, tx, selected)
			if err != nil {
				return err
			}
			var current *PlacementHash
			for index := range currentHashes {
				if currentHashes[index].Hash == planned.Hash {
					current = &currentHashes[index]
					break
				}
			}
			if current == nil {
				committed.ReferenceDrift = true
				return nil
			}
			committed.ReferenceDrift = current.TotalReferences > current.SelectedReferences
			committed.AuditPinned = current.AuditPinned &&
				request.SourceStoreID == s.primaryStoreID &&
				!request.AllowAuditedRemoteOnly
		}
		if !request.RetireSource || committed.ReferenceDrift || committed.AuditPinned {
			return nil
		}
		if source.Pack != nil {
			committed.PackRepackRequired = true
			entryResult, err := tx.ExecContext(ctx, `
				DELETE FROM blob_pack_entries
				WHERE blob_hash=? AND store_id=? AND pack_id=?`,
				planned.Hash, request.SourceStoreID, source.Pack.PackID,
			)
			if err != nil {
				return fmt.Errorf(
					"revoking packed placement source %s: %w", planned.Hash, err,
				)
			}
			entryRows, err := entryResult.RowsAffected()
			if err != nil {
				return fmt.Errorf(
					"reading packed placement source revocation: %w", err,
				)
			}
			if entryRows != 1 {
				committed.ReferenceDrift = true
				return nil
			}
			locationResult, err := tx.ExecContext(ctx, `
				DELETE FROM blob_locations
				WHERE blob_hash=? AND store_id=? AND generation=? AND kind=?`,
				planned.Hash, request.SourceStoreID,
				source.Generation, blobLocationKindPacked,
			)
			if err != nil {
				return fmt.Errorf(
					"revoking packed placement location %s: %w", planned.Hash, err,
				)
			}
			locationRows, err := locationResult.RowsAffected()
			if err != nil {
				return fmt.Errorf(
					"reading packed placement location revocation: %w", err,
				)
			}
			if locationRows != 1 {
				return fmt.Errorf(
					"packed placement location %s changed during revocation: %w",
					planned.Hash, ErrStaleRevision,
				)
			}
			committed.SourceRevoked = true
			return nil
		}
		result, err := tx.ExecContext(ctx, `
			DELETE FROM blob_locations
			WHERE blob_hash=? AND store_id=? AND generation=? AND kind=?`,
			planned.Hash, request.SourceStoreID, source.Generation, blobLocationKindLoose,
		)
		if err != nil {
			return fmt.Errorf("revoking placement source %s: %w", planned.Hash, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reading placement source revocation: %w", err)
		}
		if affected != 1 {
			committed.ReferenceDrift = true
			return nil
		}
		ref := packstore.ObjectRef{
			LooseHash: packstore.Hash(planned.Hash), LooseEncoding: source.Loose.Encoding,
		}
		if err := recordStorageOperationCleanupTx(
			ctx, tx, operationID, request.SourceStoreID, []packstore.ObjectRef{ref},
		); err != nil {
			return err
		}
		committed.SourceRevoked = true
		committed.Retire = &ref
		return nil
	})
	return committed, err
}

func sameReadLocation(first, second packstore.ReadLocation) bool {
	if first.StoreID != second.StoreID || first.Generation != second.Generation ||
		(first.Loose == nil) != (second.Loose == nil) ||
		(first.Pack == nil) != (second.Pack == nil) {
		return false
	}
	if first.Loose != nil && *first.Loose != *second.Loose {
		return false
	}
	return first.Pack == nil || *first.Pack == *second.Pack
}

func looseEncodingName(value packstore.LooseEncoding) (string, error) {
	switch value {
	case packstore.LooseEncodingRaw:
		return looseEncodingRaw, nil
	case packstore.LooseEncodingZstd:
		return looseEncodingZstd, nil
	default:
		return "", fmt.Errorf("invalid placement loose encoding %d", value)
	}
}
