package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// publishedNoteEligibilitySQL is the single current-source relation. Callers
// supply nodes n and its retained current content_versions cv. This predicate
// belongs before ranking, limits and aggregates, never on a returned page.
func publishedNoteEligibilitySQL(selector *PublishedNoteSelector) (string, []any) {
	return publishedNoteEligibilitySQLParameter(selector, "?")
}

// parameter is a caller-owned placeholder, never user input. Numbered callers
// share the eligibility relation without relying on SQLite's anonymous binds.
func publishedNoteEligibilitySQLParameter(selector *PublishedNoteSelector, parameter string) (string, []any) {
	if selector == nil {
		return "", nil
	}
	name, args := "", []any(nil)
	if selector.Publication != "" {
		name = " AND np.name=" + parameter
		args = append(args, selector.Publication)
	}
	return `EXISTS(SELECT 1 FROM published_note_sources pns
 JOIN note_publications np ON np.name=pns.publication_name
 WHERE pns.node_id=n.id AND pns.content_version_id=cv.version_id
 AND pns.blob_hash=cv.blob_hash AND pns.size=cv.size
 AND n.current_version_id=cv.version_id AND n.kind='file' AND n.trashed_at IS NULL
 AND pns.presence='present' AND pns.availability='retained' AND np.active=1` + name + `)`, args
}

func normalizePublishedNoteSelector(selector *PublishedNoteSelector) (*PublishedNoteSelector, error) {
	if selector == nil {
		return nil, nil //nolint:nilnil // nil is the meaningful unscoped selector.
	}
	if selector.Publication != "" {
		if err := ValidatePushName(selector.Publication); err != nil {
			return nil, fmt.Errorf("%w: published note selector: %w", ErrInvalidDocumentQuery, err)
		}
	}
	normalized := *selector
	return &normalized, nil
}

// PublishedNotesRevision binds a collection cursor to its database epoch and
// active namespace revisions without retaining a collection of source IDs.
func (s *Store) PublishedNotesRevision(ctx context.Context, selector *PublishedNoteSelector) (string, error) {
	normalized, err := normalizePublishedNoteSelector(selector)
	if err != nil {
		return "", err
	}
	var stamp string
	err = s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		stamp, err = publishedNotesRevisionTx(ctx, tx, normalized)
		return err
	})
	return stamp, err
}

func publishedNotesRevisionTx(ctx context.Context, q metadataQuerier, selector *PublishedNoteSelector) (string, error) {
	if selector == nil {
		return "", nil
	}
	var epoch string
	if err := q.QueryRowContext(ctx, `SELECT database_epoch FROM note_publication_state WHERE singleton=1`).Scan(&epoch); err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%q\n", epoch)
	where, args := "", []any(nil)
	if selector.Publication != "" {
		where = " AND name=$1"
		args = append(args, selector.Publication)
	}
	rows, err := q.QueryContext(ctx, `SELECT name,namespace_revision FROM note_publications WHERE active=1`+where+` ORDER BY name`, args...)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var revision int64
		if err := rows.Scan(&name, &revision); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(digest, "%q %d\n", name, revision)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// ResolvePublishedNoteIdentities resolves a bounded result batch in one read.
// A changed or ineligible identity fails closed rather than acquiring a new
// source label after the search's final authority check.
func (s *Store) ResolvePublishedNoteIdentities(ctx context.Context, selector PublishedNoteSelector, identities []DocumentCatalogIdentity) ([]PublishedNoteIdentity, error) {
	normalized, err := normalizePublishedNoteSelector(&selector)
	if err != nil {
		return nil, err
	}
	if len(identities) > 1000 {
		return nil, errors.New("published note identity batch exceeds 1000")
	}
	var result []PublishedNoteIdentity
	err = s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.publishedNoteIdentitiesTx(ctx, tx, normalized, identities)
		return err
	})
	return result, err
}

func (s *Store) publishedNoteIdentitiesTx(ctx context.Context, q metadataQuerier, selector *PublishedNoteSelector, identities []DocumentCatalogIdentity) ([]PublishedNoteIdentity, error) {
	if len(identities) == 0 {
		return []PublishedNoteIdentity{}, nil
	}
	type binding struct {
		NodeID  int64  `json:"node_id"`
		Version string `json:"version"`
		Ordinal int    `json:"ordinal"`
	}
	payload := make([]binding, len(identities))
	for i, id := range identities {
		if id.NodeID <= 0 {
			return nil, ErrInvalidDocumentQuery
		}
		if err := validateUUIDv4(id.ContentVersionID); err != nil {
			return nil, err
		}
		payload[i] = binding{id.NodeID, id.ContentVersionID, i}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	predicate, args := publishedNoteEligibilitySQLParameter(selector, "$2")
	args = append([]any{string(encoded)}, args...)
	d := metadataJSONDialect{postgres: s.postgresSchema != ""}
	rows, err := q.QueryContext(ctx, `WITH requested AS (SELECT `+d.number("value", "node_id")+` node_id,`+d.text("value", "version")+` version_id,`+d.number("value", "ordinal")+` ordinal FROM `+d.arrayValues("$1")+`)
 SELECT p.publication_name,p.source_ref,n.id,cv.version_id,cv.blob_hash,cv.size,p.source_revision,np.namespace_revision
 FROM requested r CROSS JOIN `+nodeFrom+`
 JOIN published_note_sources p ON p.node_id=n.id JOIN note_publications np ON np.name=p.publication_name
 WHERE n.id=r.node_id AND cv.version_id=r.version_id AND `+predicate+` ORDER BY r.ordinal`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]PublishedNoteIdentity, 0, len(identities))
	for rows.Next() {
		id := PublishedNoteIdentity{VaultID: s.VaultID()}
		if err := rows.Scan(&id.Publication, &id.SourceRef, &id.NodeID, &id.ContentVersionID, &id.Hash, &id.Size, &id.SourceRevision, &id.NamespaceRevision); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != len(identities) {
		return nil, ErrProcessingSourceFenceStaleVersion
	}
	return result, nil
}

func (s *Store) attachPublishedSearchIdentities(ctx context.Context, hits []SearchHit, selector *PublishedNoteSelector) error {
	if selector == nil || len(hits) == 0 {
		return nil
	}
	ids := make([]DocumentCatalogIdentity, len(hits))
	for i, hit := range hits {
		ids[i] = DocumentCatalogIdentity{NodeID: hit.Node.ID, ContentVersionID: hit.Node.CurrentVersionID}
	}
	result, err := s.ResolvePublishedNoteIdentities(ctx, *selector, ids)
	if err != nil {
		return err
	}
	for i := range hits {
		hits[i].PublishedNote = &result[i]
	}
	return nil
}
