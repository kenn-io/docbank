package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document/notemarkdown"
)

const maxNoteArtifactBytes = 16 << 20

var ErrInvalidNoteProjection = errors.New("invalid authored-note projection")
var ErrNoteIndexStale = errors.New("authored-note index inputs changed")

type NoteParseRecord struct {
	ContentVersionID, Hash, Recipe, Checksum, State, Reason string
	Size                                                    int64
	Artifact                                                []byte
}
type NoteIndexTarget struct {
	Name, DatabaseEpoch string
	NamespaceRevision   int64
}

func validateNoteParse(ctx context.Context, record NoteParseRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.Recipe != notemarkdown.Recipe || record.Size < 0 || len(record.Artifact) > maxNoteArtifactBytes {
		return ErrInvalidNoteProjection
	}
	if err := validateUUIDv4(record.ContentVersionID); err != nil {
		return ErrInvalidNoteProjection
	}
	if err := validateCatalogSHA256(record.Hash, "note hash"); err != nil {
		return ErrInvalidNoteProjection
	}
	switch record.State {
	case "ready":
		if record.Reason != "" || len(record.Artifact) == 0 {
			return ErrInvalidNoteProjection
		}
		digest := sha256.Sum256(record.Artifact)
		if hex.EncodeToString(digest[:]) != record.Checksum {
			return ErrInvalidNoteProjection
		}
		var note notemarkdown.Note
		if err := json.Unmarshal(record.Artifact, &note); err != nil {
			return ErrInvalidNoteProjection
		}
		if note.SourceSHA256 != record.Hash || int64(note.SourceBytes) != record.Size || note.Recipe != record.Recipe {
			return ErrInvalidNoteProjection
		}
		canonical, err := notemarkdown.CanonicalArtifact(ctx, note)
		if err != nil {
			return errors.Join(ErrInvalidNoteProjection, err)
		}
		if !bytes.Equal(canonical, record.Artifact) {
			return ErrInvalidNoteProjection
		}
	case "limit":
		if record.Checksum != "" || len(record.Artifact) != 0 || (record.Reason != "input_limit" && record.Reason != "parser_limit") {
			return ErrInvalidNoteProjection
		}
	case "failed":
		if record.Checksum != "" || len(record.Artifact) != 0 || record.Reason != "invalid_markdown" {
			return ErrInvalidNoteProjection
		}
	default:
		return ErrInvalidNoteProjection
	}
	return nil
}

// RecordNoteParse installs an immutable content/recipe artifact and separately
// binds it to a retained exact version. Replays do not invalidate quiet graphs.
func (s *Store) RecordNoteParse(ctx context.Context, record NoteParseRecord) error {
	if err := validateNoteParse(ctx, record); err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var hash string
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT blob_hash,size FROM content_versions WHERE version_id=?`, record.ContentVersionID).Scan(&hash, &size); err != nil {
			return err
		}
		if hash != record.Hash || size != record.Size {
			return ErrInvalidNoteProjection
		}
		generation := publicationRequestDigest(struct{ Hash, Recipe string }{record.Hash, record.Recipe})
		_, err := tx.ExecContext(ctx, `INSERT INTO note_parse_generations(generation_id,blob_hash,source_size,recipe,state,reason,checksum,artifact) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(blob_hash,recipe) DO NOTHING`, generation, record.Hash, record.Size, record.Recipe, record.State, record.Reason, record.Checksum, record.Artifact)
		if err != nil {
			return err
		}
		var storedState, reason, checksum string
		var storedSize int64
		if err := tx.QueryRowContext(ctx, `SELECT source_size,state,reason,checksum FROM note_parse_generations WHERE generation_id=?`, generation).Scan(&storedSize, &storedState, &reason, &checksum); err != nil {
			return err
		}
		if storedSize != record.Size || storedState != record.State || reason != record.Reason || checksum != record.Checksum {
			return ErrInvalidNoteProjection
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO note_parse_bindings(content_version_id,recipe,generation_id) VALUES(?,?,?) ON CONFLICT(content_version_id,recipe) DO NOTHING`, record.ContentVersionID, record.Recipe, generation)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE note_publications SET namespace_revision=namespace_revision+1 WHERE name IN (SELECT publication_name FROM published_note_sources WHERE content_version_id=?)`, record.ContentVersionID)
		return err
	})
}

func (s *Store) NoteParseForVersion(ctx context.Context, version, recipe string) (NoteParseRecord, bool, error) {
	if err := validateUUIDv4(version); err != nil {
		return NoteParseRecord{}, false, ErrInvalidNoteProjection
	}
	if recipe != notemarkdown.Recipe {
		return NoteParseRecord{}, false, ErrInvalidNoteProjection
	}
	var record NoteParseRecord
	var found bool
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		record, found, err = noteParseForVersionTx(ctx, tx, version, recipe)
		return err
	})
	return record, found, err
}
func noteParseForVersionTx(ctx context.Context, q metadataQuerier, version, recipe string) (NoteParseRecord, bool, error) {
	record := NoteParseRecord{ContentVersionID: version, Recipe: recipe}
	var artifactSize int64
	err := q.QueryRowContext(ctx, `SELECT p.blob_hash,p.source_size,p.state,p.reason,p.checksum,COALESCE(length(p.artifact),0)
 FROM note_parse_bindings b JOIN note_parse_generations p ON p.generation_id=b.generation_id
 JOIN content_versions cv ON cv.version_id=b.content_version_id AND cv.blob_hash=p.blob_hash AND cv.size=p.source_size
 WHERE b.content_version_id=? AND b.recipe=? AND p.recipe=b.recipe`, version, recipe).Scan(&record.Hash, &record.Size, &record.State, &record.Reason, &record.Checksum, &artifactSize)
	if errors.Is(err, sql.ErrNoRows) {
		return NoteParseRecord{}, false, nil
	}
	if err != nil {
		return NoteParseRecord{}, false, err
	}
	if artifactSize < 0 || artifactSize > maxNoteArtifactBytes {
		return NoteParseRecord{}, false, ErrInvalidNoteProjection
	}
	if artifactSize > 0 {
		if err := q.QueryRowContext(ctx, `SELECT p.artifact FROM note_parse_bindings b JOIN note_parse_generations p ON p.generation_id=b.generation_id WHERE b.content_version_id=? AND b.recipe=?`, version, recipe).Scan(&record.Artifact); err != nil {
			return NoteParseRecord{}, false, err
		}
	}
	if err := validateNoteParse(ctx, record); err != nil {
		return NoteParseRecord{}, false, err
	}
	return record, true, nil
}

// NoteIndexTargets pages namespaces whose current epoch/revision/recipes have
// no completed matching head. A restored database never inherits a graph head.
func (s *Store) NoteIndexTargets(ctx context.Context, after string, limit int) ([]NoteIndexTarget, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalidNoteProjection
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.name,r.database_epoch,p.namespace_revision FROM note_publications p CROSS JOIN note_publication_state r
 LEFT JOIN note_index_heads h ON h.publication_name=p.name LEFT JOIN note_index_generations g ON g.generation_id=h.generation_id
 WHERE p.active=1 AND p.name>? AND (g.generation_id IS NULL OR g.completed<>1 OR g.database_epoch<>r.database_epoch OR g.namespace_revision<>p.namespace_revision OR g.parser_recipe<>? OR g.resolver_recipe<>?) ORDER BY p.name LIMIT ?`, after, notemarkdown.Recipe, notemarkdown.ResolverRecipe, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]NoteIndexTarget, 0, min(limit, 64))
	for rows.Next() {
		var item NoteIndexTarget
		if err := rows.Scan(&item.Name, &item.DatabaseEpoch, &item.NamespaceRevision); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) EligibleNoteSources(ctx context.Context, publication, after string, limit int) ([]NotePublicationSource, error) {
	if err := ValidatePushName(publication); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalidNoteProjection
	}
	predicate, args := publishedNoteEligibilitySQL(&PublishedNoteSelector{Publication: publication})
	args = append([]any{publication, after}, args...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT p.source_ref,n.id,cv.version_id,cv.blob_hash,cv.size,p.source_revision,np.namespace_revision FROM published_note_sources p JOIN note_publications np ON np.name=p.publication_name JOIN nodes n ON n.id=p.node_id JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE p.publication_name=? AND p.source_ref>? AND `+predicate+` ORDER BY p.source_ref LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]NotePublicationSource, 0, min(limit, 64))
	for rows.Next() {
		item := NotePublicationSource{Identity: PublishedNoteIdentity{VaultID: s.VaultID(), Publication: publication}, Presence: "present", Availability: "retained"}
		id := &item.Identity
		if err := rows.Scan(&id.SourceRef, &id.NodeID, &id.ContentVersionID, &id.Hash, &id.Size, &id.SourceRevision, &id.NamespaceRevision); err != nil {
			return nil, err
		}
		item.ArchiveHead = &NoteArchiveHead{NodeID: id.NodeID, ContentVersionID: id.ContentVersionID, Hash: id.Hash, Size: id.Size}
		result = append(result, item)
	}
	return result, rows.Err()
}
