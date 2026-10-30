package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// VaultChange identifies a committed metadata change. Revisions belong to nodes,
// while cursors order changes across the vault's current database incarnation.
type VaultChange struct {
	Cursor   string `json:"cursor"`
	Kind     string `json:"kind"`
	NodeID   int64  `json:"node_id"`
	Revision int64  `json:"revision"`
}

// ErrChangeCursorExpired requires a fresh scan using the returned head cursor.
var ErrChangeCursorExpired = errors.New("vault change cursor expired")

// ErrInvalidChangeCursor means the cursor is malformed.
var ErrInvalidChangeCursor = errors.New("invalid vault change cursor")
var vaultChangeRetention int64 = 100_000

func changeCursor(epoch string, seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(epoch + ":" + strconv.FormatInt(seq, 10)))
}

func recordVaultChangesTx(ctx context.Context, tx *sql.Tx, kind string, nodeIDs ...int64) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	var high int64
	if err := tx.QueryRowContext(ctx, `UPDATE vault_change_head SET high_water=high_water+$1 WHERE singleton=1 RETURNING high_water`, len(nodeIDs)).Scan(&high); err != nil {
		return fmt.Errorf("allocating vault changes: %w", err)
	}
	for i, id := range nodeIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO vault_changes(seq,kind,node_id,node_revision) SELECT $1,$2,id,revision FROM nodes WHERE id=$3`, high-int64(len(nodeIDs))+int64(i)+1, kind, id); err != nil {
			return fmt.Errorf("recording vault change: %w", err)
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM vault_changes WHERE seq <= $1`, high-vaultChangeRetention)
	return err
}

// VaultChanges reads one page and the current head cursor in a single snapshot.
// An empty after starts at the oldest retained change. On expiry, head is the
// cursor to save before rescanning, then resume after the scan finishes.
func (s *Store) VaultChanges(ctx context.Context, after string, limit int) (changes []VaultChange, head string, err error) {
	if limit < 1 || limit > 500 {
		return nil, "", errors.New("vault change limit must be between 1 and 500")
	}
	epoch := ""
	seq := int64(-1)
	if after != "" {
		raw, e := base64.RawURLEncoding.DecodeString(after)
		if e != nil {
			return nil, "", ErrInvalidChangeCursor
		}
		var number string
		var ok bool
		epoch, number, ok = strings.Cut(string(raw), ":")
		seq, e = strconv.ParseInt(number, 10, 64)
		if !ok || validateUUIDv4(epoch) != nil || e != nil || seq < 0 || changeCursor(epoch, seq) != after {
			return nil, "", ErrInvalidChangeCursor
		}
	}
	rows, err := s.db.QueryContext(ctx, `WITH position AS (
        SELECT epoch, high_water,
            CASE WHEN CAST($1 AS BIGINT) < 0 THEN COALESCE((SELECT MIN(seq)-1 FROM vault_changes), high_water) ELSE $1 END AS after_seq
        FROM vault_change_head WHERE singleton=1
    ) SELECT p.epoch,p.high_water,p.after_seq,c.seq,c.kind,c.node_id,c.node_revision
    FROM position p LEFT JOIN vault_changes c
        ON c.seq>p.after_seq
        AND c.seq <= CASE WHEN p.high_water-p.after_seq < $2 THEN p.high_water ELSE p.after_seq+$2 END
    ORDER BY c.seq`, seq, limit)
	if err != nil {
		return nil, "", err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var current string
		var high, start int64
		var eventSeq, id, revision sql.NullInt64
		var kind sql.NullString
		if err = rows.Scan(&current, &high, &start, &eventSeq, &kind, &id, &revision); err != nil {
			return nil, "", err
		}
		head = changeCursor(current, high)
		if (epoch != "" && epoch != current) || start > high || (len(changes) == 0 && start < high && (!eventSeq.Valid || eventSeq.Int64 != start+1)) {
			return nil, head, ErrChangeCursorExpired
		}
		if eventSeq.Valid {
			changes = append(changes, VaultChange{Cursor: changeCursor(current, eventSeq.Int64), Kind: kind.String, NodeID: id.Int64, Revision: revision.Int64})
		}
	}
	return changes, head, rows.Err()
}

func publishRenditionHeadTx(ctx context.Context, tx *sql.Tx, head RenditionHeadRecord) error {
	var prior string
	err := tx.QueryRowContext(ctx, `SELECT attachment_id FROM rendition_heads WHERE content_version_id=$1 AND profile_fingerprint=$2`, head.ContentVersionID, head.ProcessingProfileFingerprint).Scan(&prior)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading prior rendition head: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_heads(content_version_id,profile_fingerprint,attachment_id,published_at) VALUES($1,$2,$3,$4) ON CONFLICT(content_version_id,profile_fingerprint) DO UPDATE SET attachment_id=excluded.attachment_id,published_at=excluded.published_at`, head.ContentVersionID, head.ProcessingProfileFingerprint, head.AttachmentID, head.PublishedAt); err != nil {
		return fmt.Errorf("publishing rendition head: %w", err)
	}
	if prior == head.AttachmentID {
		return nil
	}
	return recordProcessedVersionTx(ctx, tx, head.ContentVersionID)
}

func recordProcessedVersionTx(ctx context.Context, tx *sql.Tx, version string) error {
	var nodeID int64
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM content_versions WHERE version_id=$1`, version).Scan(&nodeID); err != nil {
		return err
	}
	return recordVaultChangesTx(ctx, tx, "processed", nodeID)
}
