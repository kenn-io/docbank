package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

var ErrAmbiguousContractDocument = errors.New("multiple contract document versions match; supply a version")

type ContractDocument struct {
	ContractID, ContentVersionID, BlobSHA256, Role, AddedAt string
	NodeID, Size                                            int64
	Current, Available                                      bool
}

func validContractDocumentRole(role string) bool {
	switch role {
	case "draft", "signed", "audit_trail", "amendment", "attachment":
		return true
	}
	return false
}

func (s *Store) LinkContractDocument(ctx context.Context, id string, rev, nodeID int64, role, versionID string) (Contract, error) {
	return s.changeContractDocument(ctx, id, rev, nodeID, role, versionID, true)
}
func (s *Store) UnlinkContractDocument(ctx context.Context, id string, rev, nodeID int64, role, versionID string) (Contract, error) {
	return s.changeContractDocument(ctx, id, rev, nodeID, role, versionID, false)
}
func (s *Store) changeContractDocument(ctx context.Context, id string, rev, nodeID int64, role, versionID string, link bool) (Contract, error) {
	var out Contract
	asOf := time.Now().Format(time.DateOnly)
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		c, err := scanContract(tx.QueryRowContext(ctx, `SELECT `+contractColumns+` FROM contracts WHERE contract_id=$1`, id), asOf)
		if err != nil {
			return err
		}
		if c.Revision != rev {
			return ErrStaleRevision
		}
		if nodeID < 1 {
			return fmt.Errorf("%w: node ID must be positive", ErrInvalidContract)
		}
		if !validContractDocumentRole(role) {
			return fmt.Errorf("%w: document role must be draft, signed, audit_trail, amendment or attachment", ErrInvalidContract)
		}
		if versionID != "" && validateUUIDv4(versionID) != nil {
			return fmt.Errorf("%w: invalid content version ID", ErrInvalidContract)
		}
		rows, err := tx.QueryContext(ctx, `SELECT content_version_id FROM contract_documents WHERE contract_id=$1 AND node_id=$2 AND role=$3 ORDER BY content_version_id`, id, nodeID, role)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var versions []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			versions = append(versions, v)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		exists := func(v string) bool {
			return slices.Contains(versions, v)
		}
		if link && versionID != "" && exists(versionID) {
			out, err = getContractTx(ctx, tx, id, asOf)
			return err
		}
		if link {
			n, e := nodeByIDQuery(ctx, tx, nodeID)
			if errors.Is(e, ErrNotFound) {
				return fmt.Errorf("%w: node not found or content version does not belong to node", ErrInvalidContract)
			}
			if e != nil {
				return e
			}
			if n.TrashedAt != nil {
				return fmt.Errorf("%w: node is trashed", ErrInvalidContract)
			}
			if n.Kind != nodeKindFile {
				return ErrNotFile
			}
			if versionID == "" {
				versionID = n.CurrentVersionID
			}
			if exists(versionID) {
				out, err = getContractTx(ctx, tx, id, asOf)
				return err
			}
			v, e := scanContentVersion(tx.QueryRowContext(ctx, `SELECT `+contentVersionCols+` FROM content_versions WHERE version_id=$1 AND node_id=$2`, versionID, nodeID))
			if errors.Is(e, ErrNotFound) {
				return fmt.Errorf("%w: node not found or content version does not belong to node", ErrInvalidContract)
			}
			if e != nil {
				return e
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO contract_documents(contract_id,node_id,content_version_id,blob_sha256,size,role,added_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, nodeID, v.ID, v.BlobHash, v.Size, role, nowRFC3339())
		} else {
			if versionID == "" {
				if len(versions) == 0 {
					return ErrNotFound
				}
				if len(versions) > 1 {
					return ErrAmbiguousContractDocument
				}
				versionID = versions[0]
			} else if !exists(versionID) {
				return ErrNotFound
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM contract_documents WHERE contract_id=$1 AND node_id=$2 AND content_version_id=$3 AND role=$4`, id, nodeID, versionID, role)
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE contracts SET revision=$1,updated_at=$2 WHERE contract_id=$3`, rev+1, nowRFC3339(), id); err != nil {
			return err
		}
		out, err = getContractTx(ctx, tx, id, asOf)
		return err
	})
	return out, err
}
