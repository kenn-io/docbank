package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

const (
	TagAssignmentModeDocument = "document"
	TagAssignmentModePassage  = "passage"
	TagAssignmentModeEither   = "either"
)

// PassageTagChange records one assignment against a complete immutable
// passage identity. The source Markdown and rendition are never rewritten.
type PassageTagChange struct {
	Tag       Tag                   `json:"tag"`
	PassageID string                `json:"passage_id"`
	Ref       document.PassageRefV1 `json:"ref"`
	Changed   bool                  `json:"changed"`
}

func (s *Store) AssignPassageTag(ctx context.Context, tagID string, ref document.PassageRefV1) (PassageTagChange, error) {
	return s.changePassageTag(ctx, tagID, ref, true)
}

func (s *Store) RemovePassageTag(ctx context.Context, tagID string, ref document.PassageRefV1) (PassageTagChange, error) {
	return s.changePassageTag(ctx, tagID, ref, false)
}

func (s *Store) changePassageTag(ctx context.Context, tagID string, ref document.PassageRefV1, assign bool) (PassageTagChange, error) {
	passageID, err := document.PassageIdentityV1(ref)
	if err != nil {
		return PassageTagChange{}, ErrPassageAuthorityUnavailable
	}
	var authority PassageAuthority
	var encoded []byte
	if assign {
		authority, err = s.ResolvePassageAuthority(ctx, ref)
		if err != nil {
			return PassageTagChange{}, err
		}
		encoded, err = json.Marshal(ref)
		if err != nil {
			return PassageTagChange{}, err
		}
	}
	var result PassageTagChange
	err = s.withConceptTx(ctx, func(tx *sql.Tx) error {
		tag, err := tagByIDTx(tx, tagID)
		if err != nil {
			return err
		}
		// Recheck the exact local tuple under the write lock. A source prune or
		// attachment retirement between preflight and commit must fail closed.
		if assign {
			var retained bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM content_versions v
			JOIN nodes n ON n.id=v.node_id AND n.kind='file' AND n.trashed_at IS NULL
			JOIN rendition_attachments a ON a.content_version_id=v.version_id
			JOIN rendition_builds b ON b.build_id=a.build_id AND b.source_sha256=v.blob_hash
			JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
			WHERE v.version_id=? AND v.node_id=? AND v.blob_hash=?
			AND a.attachment_id=? AND a.build_id=?
			AND artifact.role=?
			AND artifact.blob_hash=artifact.checksum AND artifact.blob_hash=b.markdown_checksum
		)`, authority.Version.ID, authority.Node.ID, ref.SourceSHA256,
				authority.Attachment.ID, authority.Build.ID,
				catalogArtifactSanitizedMarkdown).Scan(&retained)
			if err != nil {
				return err
			}
			if !retained {
				return ErrPassageAuthorityUnavailable
			}
		}
		result = PassageTagChange{Tag: tag, PassageID: passageID, Ref: ref}
		if assign {
			res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO passage_tags(
				passage_id,tag_id,ref_json,document_uid,content_version_id
			) VALUES(?,?,?,?,?)`, passageID, tagID, encoded, authority.Identity.DocumentUID, authority.Version.ID)
			if err != nil {
				return err
			}
			count, err := res.RowsAffected()
			if err != nil {
				return err
			}
			result.Changed = count == 1
		} else {
			res, err := tx.ExecContext(ctx, `DELETE FROM passage_tags WHERE passage_id=? AND tag_id=?`, passageID, tagID)
			if err != nil {
				return err
			}
			count, err := res.RowsAffected()
			if err != nil {
				return err
			}
			result.Changed = count == 1
		}
		if result.Changed {
			if _, err := tx.ExecContext(ctx, `UPDATE tags SET revision=revision+1 WHERE id=?`, tagID); err != nil {
				return err
			}
			result.Tag, err = tagByIDTx(tx, tagID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

// PassageTags resolves the exact retained source before showing its sidecars.
// A historical version can still be read while retained; a changed current
// version never inherits the tags.
func (s *Store) PassageTags(ctx context.Context, ref document.PassageRefV1) ([]Tag, error) {
	passageID, err := document.PassageIdentityV1(ref)
	if err != nil {
		return nil, ErrPassageAuthorityUnavailable
	}
	if _, err = s.ResolvePassageAuthority(ctx, ref); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.name,t.revision,COUNT(nt.node_id)
		FROM passage_tags pt JOIN tags t ON t.id=pt.tag_id
		LEFT JOIN node_tags nt ON nt.tag_id=t.id
		WHERE pt.passage_id=? GROUP BY t.id,t.name,t.revision ORDER BY t.name,t.id`, passageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []Tag{}
	for rows.Next() {
		tag, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, tag)
	}
	return result, rows.Err()
}

// TagsByAssignmentMode selects direct document assignments, exact-version
// passage assignments, or their distinct union. Document is the default.
func (s *Store) TagsByAssignmentMode(ctx context.Context, nodeID int64, contentVersionID, mode string) ([]Tag, error) {
	if mode == "" {
		mode = TagAssignmentModeDocument
	}
	if mode != TagAssignmentModeDocument && mode != TagAssignmentModePassage && mode != TagAssignmentModeEither {
		return nil, ErrInvalidTag
	}
	if mode != TagAssignmentModeDocument && validateUUIDv4(contentVersionID) != nil {
		return nil, ErrPassageAuthorityUnavailable
	}
	node, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if node.IsDir() || node.TrashedAt != nil {
		return nil, ErrNotFound
	}
	if mode != TagAssignmentModeDocument {
		var belongs bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM content_versions WHERE node_id=? AND version_id=?)`,
			nodeID, contentVersionID).Scan(&belongs); err != nil {
			return nil, err
		}
		if !belongs {
			return nil, ErrPassageAuthorityUnavailable
		}
	}
	query := `SELECT t.id,t.name,t.revision,COUNT(nt.node_id) FROM tags t
		LEFT JOIN node_tags nt ON nt.tag_id=t.id WHERE `
	var args []any
	switch mode {
	case TagAssignmentModeDocument:
		query += `EXISTS(SELECT 1 FROM node_tags x WHERE x.node_id=? AND x.tag_id=t.id)`
		args = []any{nodeID}
	case TagAssignmentModePassage:
		query += `EXISTS(SELECT 1 FROM passage_tags pt JOIN document_identities di ON di.document_uid=pt.document_uid
			WHERE di.node_id=? AND pt.content_version_id=? AND pt.tag_id=t.id)`
		args = []any{nodeID, contentVersionID}
	case TagAssignmentModeEither:
		query += `(EXISTS(SELECT 1 FROM node_tags x WHERE x.node_id=? AND x.tag_id=t.id)
			OR EXISTS(SELECT 1 FROM passage_tags pt JOIN document_identities di ON di.document_uid=pt.document_uid
			WHERE di.node_id=? AND pt.content_version_id=? AND pt.tag_id=t.id))`
		args = []any{nodeID, nodeID, contentVersionID}
	}
	query += ` GROUP BY t.id,t.name,t.revision ORDER BY t.name,t.id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying tags by assignment mode: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []Tag{}
	for rows.Next() {
		tag, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// PassageTagAvailability reports whether the exact referenced authority still
// exists. No search for a replacement version or quote occurs here.
func (s *Store) PassageTagAvailability(ctx context.Context, ref document.PassageRefV1) (string, error) {
	_, err := s.ResolvePassageAuthority(ctx, ref)
	if errors.Is(err, ErrPassageAuthorityUnavailable) {
		return document.PassageTagAvailabilityUnavailable, nil
	}
	if err != nil {
		return "", err
	}
	return document.PassageTagAvailabilityExact, nil
}
