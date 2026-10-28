package store

import (
	"context"
	"database/sql"
	"errors"
)

// ReplaySnapshotBatesRange finds a semantic reservation before any expensive
// source admission. The snapshot ID and operation UUID come from the prior row.
func (s *Store) ReplaySnapshotBatesRange(ctx context.Context, request BatesPlanRequest) (BatesAllocation, error) {
	return replaySnapshotBatesRange(ctx, s.db, request)
}

func replaySnapshotBatesRange(ctx context.Context, q metadataQuerier, r BatesPlanRequest) (BatesAllocation, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT allocation_id FROM bates_allocations
		WHERE namespace_id=? AND start_sequence=?`, r.NamespaceID, r.StartAt).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return BatesAllocation{}, ErrNotFound
	}
	if err != nil {
		return BatesAllocation{}, err
	}
	a, err := loadBatesAllocation(ctx, q, id)
	if err != nil {
		return BatesAllocation{}, err
	}
	r.SnapshotID = a.SnapshotID
	digest, err := BatesRequestDigest(r)
	if err != nil {
		return BatesAllocation{}, err
	}
	if a.RecipeSHA256 != r.RecipeSHA256 || a.RequestSHA256 != digest {
		return BatesAllocation{}, ErrBatesReservationConflict
	}
	return a, nil
}

// ReserveSnapshotBatesRange retains a real single-document snapshot and reserves
// its original pages in one transaction. Replay never requires a live source.
func (s *Store) ReserveSnapshotBatesRange(ctx context.Context, snapshot SnapshotSealRequest,
	request BatesPlanRequest,
) (result BatesAllocation, err error) {
	if len(snapshot.Members) != 1 || snapshot.SnapshotID != request.SnapshotID {
		return result, ErrPackageConflict
	}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		prior, replayErr := replaySnapshotBatesRange(ctx, tx, request)
		if replayErr == nil {
			result = prior
			return nil
		}
		if !errors.Is(replayErr, ErrNotFound) {
			return replayErr
		}
		member := snapshot.Members[0]
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT n.name FROM nodes n JOIN content_versions v ON v.node_id=n.id
			WHERE n.id=? AND n.kind='file' AND n.trashed_at IS NULL AND v.version_id=?
			AND v.blob_hash=? AND v.size=? AND v.mime_type='application/pdf'`,
			member.NodeID, member.ContentVersionID, member.BlobSHA256, member.Size).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// Freeze the current display name; it is not part of semantic replay.
		snapshot.Members = append([]CollectionSnapshotMember(nil), snapshot.Members...)
		snapshot.Members[0].DisplayName = name
		if _, err := s.sealCollectionSnapshotTx(ctx, tx, snapshot); err != nil {
			return err
		}
		result, err = s.reserveBatesRangeTx(ctx, tx, request)
		return err
	})
	return result, err
}
