package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ImportOwner names the kind of record that owns an import's action claim.
type ImportOwner string

const (
	MailboxImportOwner ImportOwner = "mailbox_job_id"
	PackageImportOwner ImportOwner = "package_job_id"
	PhotoImportOwner   ImportOwner = "operation_id"
)

func validActionSurface(surface string) bool {
	switch surface {
	case "web", "tui", "cli":
		return true
	default:
		return false
	}
}

// insertImportClaimTx records the interface that started a new import. An
// import started by a script has no surface and gets no claim.
func insertImportClaimTx(ctx context.Context, tx *sql.Tx, owner ImportOwner, id, surface string) error {
	if surface == "" {
		return nil
	}
	if !validActionSurface(surface) {
		return fmt.Errorf("unsupported import action surface %q", surface)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO import_action_claims(`+string(owner)+`,surface) VALUES(?,?)`, id, surface)
	return err
}

// ConsumeImportClaim returns the interface that started import id the first
// time it is called, and "" afterward or when the import has no claim. Call it
// after the import confirms a new document; the claim survives a daemon
// restart until then.
func (s *Store) ConsumeImportClaim(ctx context.Context, owner ImportOwner, id string) (string, error) {
	// Most additions find the claim already consumed, so read before writing.
	var surface string
	err := s.db.QueryRowContext(ctx, `SELECT surface FROM import_action_claims
		WHERE `+string(owner)+`=? AND consumed=0`, id).Scan(&surface)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading import claim %s: %w", id, err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE import_action_claims SET consumed=1
		WHERE `+string(owner)+`=? AND consumed=0`, id)
	if err != nil {
		return "", fmt.Errorf("consuming import claim %s: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return "", err
	}
	return surface, nil
}

// ClaimPushRun reports whether this call recorded the first addition of push
// run runID. Concurrent and later uploads in the run find the claim taken.
func (s *Store) ClaimPushRun(ctx context.Context, runID, surface string) (bool, error) {
	if !validActionSurface(surface) {
		return false, fmt.Errorf("unsupported push action surface %q", surface)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO import_action_claims(ingest_id,surface,consumed)
		VALUES(?,?,1) ON CONFLICT DO NOTHING`, runID, surface)
	if err != nil {
		return false, fmt.Errorf("claiming push run %s: %w", runID, err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
