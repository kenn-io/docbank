package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

// ErrExportVisibilityChanged means a retained source no longer has its sealed,
// live membership. The archive must be withheld even if its bytes are intact.
var ErrExportVisibilityChanged = errors.New("export source visibility changed")

// CheckExportSourceVisibility checks the complete retained membership in one
// read snapshot. Pages cap memory and the ancestor query checks whole pages,
// rather than issuing a query for each of up to MaxMembers members.
func (s *Store) CheckExportSourceVisibility(ctx context.Context, owner, id string) error {
	if owner == "" {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkExportSourceVisibilityTx(ctx, tx, owner, id); err != nil {
		return err
	}
	return tx.Commit()
}

func checkExportSourceVisibilityTx(ctx context.Context, tx *sql.Tx, owner, id string) error {
	source, err := loadExportSource(ctx, tx, owner, id)
	if err != nil {
		return err
	}
	if source.State != "sealed" || source.Total < 1 || source.Total > bundle.MaxMembers {
		return ErrExportVisibilityChanged
	}
	var lastNode int64
	lastVersion := ""
	count := 0
	totalBytes := int64(0)
	memberHash := sha256.New()
	for {
		rows, err := tx.QueryContext(ctx, `SELECT m.node_id,m.version_id,m.blob_hash,m.canonical_json,v.node_id,v.blob_hash,v.size,n.id
			FROM export_members m
			LEFT JOIN content_versions v ON v.version_id=m.version_id
			LEFT JOIN nodes n ON n.id=m.node_id
			WHERE m.source_id=? AND (m.node_id>? OR (m.node_id=? AND m.version_id>?))
			ORDER BY m.node_id,m.version_id LIMIT 250`, id, lastNode, lastNode, lastVersion)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		ids := make([]int64, 0, 250)
		page := 0
		for rows.Next() {
			var nodeID int64
			var versionID, hash string
			var raw []byte
			var versionNode, versionSize, liveNode sql.NullInt64
			var versionHash sql.NullString
			if err = rows.Scan(&nodeID, &versionID, &hash, &raw, &versionNode, &versionHash, &versionSize, &liveNode); err != nil {
				break
			}
			if nodeID < 1 || !versionNode.Valid || versionNode.Int64 != nodeID ||
				!liveNode.Valid || !versionHash.Valid || versionHash.String != hash ||
				!versionSize.Valid || versionSize.Int64 < 0 ||
				len(raw) > bundle.MaxMemberBytes {
				err = ErrExportVisibilityChanged
				break
			}
			var member bundle.Member
			if json.Unmarshal(raw, &member, json.RejectUnknownMembers(true)) != nil ||
				member.NodeID != nodeID || member.VersionID != versionID ||
				member.SHA256 != hash || member.Size != versionSize.Int64 ||
				member.Size > bundle.MaxRoleBytes-totalBytes {
				err = ErrExportVisibilityChanged
				break
			}
			canonicalRaw, marshalErr := canonical.Marshal(member)
			if marshalErr != nil || !bytes.Equal(raw, canonicalRaw) {
				err = ErrExportVisibilityChanged
				break
			}
			_, _ = fmt.Fprintf(memberHash, "%d:%s\n", nodeID, versionID)
			totalBytes += member.Size
			if len(ids) == 0 || ids[len(ids)-1] != nodeID {
				ids = append(ids, nodeID)
			}
			lastNode, lastVersion = nodeID, versionID
			page++
			count++
			if count > source.Total {
				err = ErrExportVisibilityChanged
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		err = errors.Join(err, rows.Close())
		if err != nil {
			return err
		}
		if page > 0 {
			if err = checkExportLiveAncestors(ctx, tx, ids); err != nil {
				return err
			}
		}
		if page < 250 {
			break
		}
	}
	if count != source.Total || totalBytes != source.SourceBytes ||
		hex.EncodeToString(memberHash.Sum(nil)) != source.MemberHash {
		return ErrExportVisibilityChanged
	}
	return nil
}

// CheckExportPlanVisibility binds the plan, its exact source and every planned
// attachment child to one read snapshot. It verifies the retained plan rows
// before reporting that all file endpoints are still live.
func (s *Store) CheckExportPlanVisibility(ctx context.Context, owner, id string) error {
	if owner == "" {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var actual, retainedUntil string
	if err = tx.QueryRowContext(ctx, `SELECT owner,expires_at FROM export_plans WHERE id=?`, id).Scan(&actual, &retainedUntil); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if actual != owner {
		return ErrNotFound
	}
	if exportExpired(retainedUntil) {
		return bundle.ErrExpired
	}
	plan, err := loadExportPlan(ctx, tx, id)
	if err != nil {
		return err
	}
	if plan.ID != id || plan.Fingerprint == "" || plan.Source.ID == "" {
		return ErrExportVisibilityChanged
	}
	source, err := loadExportSource(ctx, tx, owner, plan.Source.ID)
	if err != nil {
		return err
	}
	if source != plan.Source {
		return ErrExportVisibilityChanged
	}
	if err = checkExportSourceVisibilityTx(ctx, tx, owner, source.ID); err != nil {
		return err
	}
	children := make([]document.EmailDocumentIdentity, 0, 200)
	flushChildren := func() error {
		if len(children) == 0 {
			return nil
		}
		if err := checkExportChildBatch(ctx, tx, children); err != nil {
			return err
		}
		children = children[:0]
		return nil
	}
	validator := bundle.RowValidator{Plan: plan}
	fingerprint, err := bundle.Fingerprint(plan, func(visit func(bundle.Document) error) error {
		return walkExportDocumentBytes(ctx, tx, id, func(raw []byte) error {
			if len(raw) > bundle.MaxMemberBytes {
				return ErrExportVisibilityChanged
			}
			var row bundle.Document
			if json.Unmarshal(raw, &row, json.RejectUnknownMembers(true)) != nil || validator.Add(row) != nil {
				return ErrExportVisibilityChanged
			}
			if row.Attachment != nil && row.Attachment.Child != nil {
				children = append(children, *row.Attachment.Child)
				if len(children) == cap(children) {
					if err := flushChildren(); err != nil {
						return err
					}
				}
			}
			return visit(row)
		})
	})
	if err != nil {
		return err
	}
	if validator.Finish() != nil || fingerprint != plan.Fingerprint {
		return ErrExportVisibilityChanged
	}
	if err = flushChildren(); err != nil {
		return err
	}
	return tx.Commit()
}

func checkExportChildBatch(ctx context.Context, tx *sql.Tx, children []document.EmailDocumentIdentity) error {
	values := strings.TrimSuffix(strings.Repeat("(?,?,?,?),", len(children)), ",")
	args := make([]any, 0, 4*len(children))
	for _, child := range children {
		args = append(args, child.NodeID, child.VersionID, child.SHA256, child.Size)
	}
	query := `WITH RECURSIVE expected(node_id,version_id,hash,size) AS (VALUES ` + values + `),
		ancestry(id,parent_id,trashed_at) AS (
			SELECT n.id,n.parent_id,n.trashed_at FROM nodes n JOIN expected e ON e.node_id=n.id
			UNION
			SELECT n.id,n.parent_id,n.trashed_at FROM nodes n JOIN ancestry a ON n.id=a.parent_id
		), root_connected(id) AS (
			SELECT id FROM ancestry WHERE parent_id IS NULL
			UNION
			SELECT a.id FROM ancestry a JOIN root_connected r ON a.parent_id=r.id
		) SELECT (SELECT COUNT(*) FROM expected e
			JOIN content_versions v ON v.version_id=e.version_id AND v.node_id=e.node_id
			JOIN nodes n ON n.id=e.node_id
			WHERE v.blob_hash=e.hash AND v.size=e.size),
			EXISTS(SELECT 1 FROM ancestry WHERE trashed_at IS NOT NULL),
			EXISTS(SELECT 1 FROM ancestry a WHERE a.parent_id IS NOT NULL
				AND NOT EXISTS(SELECT 1 FROM nodes p WHERE p.id=a.parent_id)),
			(SELECT COUNT(*) FROM expected e JOIN root_connected r ON r.id=e.node_id)`
	var valid, withdrawn, missingParent, rooted int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&valid, &withdrawn, &missingParent, &rooted); err != nil {
		return err
	}
	if valid != len(children) || withdrawn != 0 || missingParent != 0 || rooted != len(children) {
		return ErrExportVisibilityChanged
	}
	return nil
}

func checkExportLiveAncestors(ctx context.Context, tx *sql.Tx, ids []int64) error {
	values := strings.TrimSuffix(strings.Repeat("(?),", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var found, withdrawn, missingParent, rooted int
	query := `WITH RECURSIVE seed(id) AS (VALUES ` + values + `),
		ancestry(id,parent_id,trashed_at) AS (
		SELECT n.id,n.parent_id,n.trashed_at FROM nodes n JOIN seed s ON s.id=n.id
		UNION
		SELECT n.id,n.parent_id,n.trashed_at FROM nodes n JOIN ancestry a ON n.id=a.parent_id
	), root_connected(id) AS (
		SELECT id FROM ancestry WHERE parent_id IS NULL
		UNION
		SELECT a.id FROM ancestry a JOIN root_connected r ON a.parent_id=r.id
	) SELECT (SELECT COUNT(*) FROM seed s JOIN nodes n ON n.id=s.id),
		EXISTS(SELECT 1 FROM ancestry WHERE trashed_at IS NOT NULL),
		EXISTS(SELECT 1 FROM ancestry a WHERE a.parent_id IS NOT NULL
			AND NOT EXISTS(SELECT 1 FROM nodes p WHERE p.id=a.parent_id)),
		(SELECT COUNT(*) FROM seed s JOIN root_connected r ON r.id=s.id)`
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&found, &withdrawn, &missingParent, &rooted); err != nil {
		return err
	}
	if found != len(ids) || withdrawn != 0 || missingParent != 0 || rooted != len(ids) {
		return ErrExportVisibilityChanged
	}
	return nil
}
