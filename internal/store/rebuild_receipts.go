package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// rebuildReceipt is the row shape the full-rebuild receipt tables share. A
// running receipt has no finish time.
type rebuildReceipt DocumentEventBuild

// rebuildCounts is one progress observation. Scanned is published + failed + unavailable.
type rebuildCounts struct{ selected, published, failed, unavailable, retrying int64 }

// rebuildReceiptTable is one receipt table. Its statements read and bind the
// shared columns in rebuildReceipt order.
type rebuildReceiptTable struct {
	table, label                    string // SQL table; error text such as "document event rebuild"
	selectSQL, insertSQL, updateSQL string
	conflict, invalid               error
	count                           func(context.Context, metadataQuerier, rebuildReceipt) (rebuildCounts, error)
}

// start creates or replays one receipt. The epoch advance and the receipt
// insert share one transaction.
func (t rebuildReceiptTable) start(
	ctx context.Context, s *Store, operationID, digest, fingerprint string,
	advance func(*sql.Tx) (int64, error),
) (rebuildReceipt, error) {
	var build rebuildReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		stored, err := t.read(ctx, tx, operationID)
		replayed, found, err := replayReceipt(stored, err, stored.RequestSHA256 == digest, t.conflict)
		if err != nil || found {
			build = replayed
			return err
		}
		epoch, err := advance(tx)
		if err != nil {
			return err
		}
		now := nowRFC3339()
		build = rebuildReceipt{
			OperationID: operationID, RequestSHA256: digest, DeriverFingerprint: fingerprint,
			State: "running", TargetEpoch: epoch, StartedAt: now, UpdatedAt: now,
		}
		if _, err := tx.ExecContext(ctx, t.insertSQL, operationID, digest, fingerprint, "running", epoch, now, now); err != nil {
			return fmt.Errorf("recording %s: %w", t.label, err)
		}
		return nil
	})
	return build, err
}

// get returns one receipt by operation ID; a malformed ID is not found.
func (t rebuildReceiptTable) get(ctx context.Context, s *Store, operationID string) (rebuildReceipt, error) {
	if err := validateUUIDv4(operationID); err != nil {
		return rebuildReceipt{}, fmt.Errorf("%s %q: %w", t.label, operationID, ErrNotFound)
	}
	return t.read(ctx, s.db, operationID)
}

func (t rebuildReceiptTable) read(ctx context.Context, q metadataQuerier, operationID string) (rebuildReceipt, error) {
	var b rebuildReceipt
	err := q.QueryRowContext(ctx, t.selectSQL, operationID).Scan(
		&b.OperationID, &b.RequestSHA256, &b.DeriverFingerprint, &b.State,
		&b.TargetEpoch, &b.Scanned, &b.Published, &b.Failed,
		&b.Unavailable, &b.StartedAt, &b.UpdatedAt, &b.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rebuildReceipt{}, fmt.Errorf("%s %q: %w", t.label, operationID, ErrNotFound)
	}
	if err != nil {
		return rebuildReceipt{}, fmt.Errorf("reading %s: %w", t.label, err)
	}
	if err := t.validate(b); err != nil {
		return rebuildReceipt{}, err
	}
	return b, nil
}

func (t rebuildReceiptTable) validate(b rebuildReceipt) error {
	if validateUUIDv4(b.OperationID) != nil ||
		validateCatalogSHA256(b.RequestSHA256, t.label+" request digest") != nil ||
		validateCatalogSHA256(b.DeriverFingerprint, t.label+" fingerprint") != nil {
		return t.invalid
	}
	if b.TargetEpoch < 1 || b.Scanned < 0 || b.Published < 0 || b.Failed < 0 || b.Unavailable < 0 ||
		b.Scanned != b.Published+b.Failed+b.Unavailable {
		return t.invalid
	}
	running := b.State == "running"
	if !running && b.State != "completed" && b.State != "failed" || running != (b.FinishedAt == nil) {
		return t.invalid
	}
	return nil
}

// progress recomputes a running receipt and reports whether its stored row
// must change. Terminal receipts never change.
func (t rebuildReceiptTable) progress(
	ctx context.Context, q metadataQuerier, operationID string,
) (rebuildReceipt, bool, error) {
	b, err := t.read(ctx, q, operationID)
	if err != nil || b.State != "running" {
		return b, false, err
	}
	c, err := t.count(ctx, q, b)
	if err != nil {
		return rebuildReceipt{}, false, err
	}
	scanned := c.published + c.failed + c.unavailable
	state := "running"
	if scanned == c.selected && c.retrying == 0 {
		state = "completed"
		if c.failed+c.unavailable > 0 {
			state = "failed"
		}
	}
	if b.Scanned == scanned && b.Published == c.published && b.Failed == c.failed &&
		b.Unavailable == c.unavailable && state == "running" {
		return b, false, nil
	}
	b.State, b.Scanned, b.Published, b.Failed, b.Unavailable = state, scanned, c.published, c.failed, c.unavailable
	b.UpdatedAt = nowRFC3339()
	if state != "running" {
		b.FinishedAt = new(b.UpdatedAt)
	}
	return b, true, nil
}

// refresh updates every running receipt whose progress changed.
func (t rebuildReceiptTable) refresh(ctx context.Context, s *Store) error {
	operationIDs, err := pageMetadataKeys(ctx, s.db, "SELECT operation_id FROM "+t.table+" WHERE state='running' ORDER BY started_at,operation_id")
	if err != nil {
		return fmt.Errorf("listing running %ss: %w", t.label, err)
	}
	for _, operationID := range operationIDs {
		if _, changed, err := t.progress(ctx, s.db, operationID); err != nil {
			return err
		} else if !changed {
			continue
		}
		// Recheck after acquiring the writer so concurrent publications and
		// refreshes cannot overwrite newer receipt progress.
		if err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
			b, changed, err := t.progress(ctx, tx, operationID)
			if err != nil || !changed {
				return err
			}
			if _, err := tx.ExecContext(ctx, t.updateSQL, b.State, b.Scanned, b.Published, b.Failed,
				b.Unavailable, b.UpdatedAt, b.FinishedAt, b.OperationID); err != nil {
				return fmt.Errorf("refreshing %s: %w", t.label, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
