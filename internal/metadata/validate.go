package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Validate verifies a complete metadata-v1 logical database without changing
// it. Physical pack metadata is outside this authority.
func Validate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("validating metadata v1: nil database")
	}
	if err := requireForeignKeys(ctx, db); err != nil {
		return fmt.Errorf("validating metadata v1: %w", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("beginning metadata v1 validation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'nodes'), 0)`,
	).Scan(&sequence); err != nil {
		return fmt.Errorf("reading metadata v1 node sequence: %w", err)
	}
	if err := validateTx(ctx, tx, sequence); err != nil {
		return err
	}
	return tx.Commit()
}

func validateTx(ctx context.Context, tx *sql.Tx, nodeSequence int64) error {
	var vaultID string
	if err := tx.QueryRowContext(ctx, `
		SELECT vault_id FROM vault_metadata
		WHERE singleton = 1 AND format_version = 1
	`).Scan(&vaultID); err != nil {
		return fmt.Errorf("reading v1 vault identity: %w", err)
	}
	if err := validateUUID("vault ID", vaultID); err != nil {
		return err
	}
	var maxNodeID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM nodes`).Scan(&maxNodeID); err != nil {
		return fmt.Errorf("reading v1 maximum node ID: %w", err)
	}
	if nodeSequence < maxNodeID {
		return fmt.Errorf("node ID high-water mark %d is below maximum node ID %d", nodeSequence, maxNodeID)
	}

	checks := []struct {
		name  string
		query string
	}{
		{"vault metadata cardinality is not one", `SELECT COUNT(*) != 1 FROM vault_metadata`},
		{"tree root cardinality is not one", `SELECT COUNT(*) != 1 FROM nodes WHERE parent_id IS NULL`},
		{"tree contains unreachable nodes", `
			WITH RECURSIVE reachable(id) AS (
			  SELECT id FROM nodes WHERE parent_id IS NULL
			  UNION SELECT n.id FROM nodes n JOIN reachable r ON n.parent_id = r.id
			)
			SELECT (SELECT COUNT(*) FROM reachable) != (SELECT COUNT(*) FROM nodes)`},
		{"node parent is not a directory", `
			SELECT EXISTS(SELECT 1 FROM nodes n JOIN nodes p ON p.id=n.parent_id WHERE p.kind!='dir')`},
		{"trash parent is not a directory", `
			SELECT EXISTS(SELECT 1 FROM nodes n JOIN nodes p ON p.id=n.trash_parent WHERE p.kind!='dir')`},
		{"trash root is not detached beneath the tree root", `
			SELECT EXISTS(
			  SELECT 1 FROM nodes n
			  WHERE n.trash_name IS NOT NULL
			    AND n.parent_id != (SELECT id FROM nodes WHERE parent_id IS NULL)
			)`},
		{"trash parent points inside its subtree", `
			WITH RECURSIVE trash_subtree(trash_root,id) AS (
			  SELECT id,id FROM nodes WHERE trash_name IS NOT NULL
			  UNION ALL
			  SELECT s.trash_root,n.id FROM trash_subtree s JOIN nodes n ON n.parent_id=s.id
			)
			SELECT EXISTS(
			  SELECT 1 FROM nodes root JOIN trash_subtree s
			    ON s.trash_root=root.id AND s.id=root.trash_parent
			)`},
		{"trashed node does not belong to exactly one trash root", `
			WITH RECURSIVE trash_subtree(trash_root,id) AS (
			  SELECT id,id FROM nodes WHERE trash_name IS NOT NULL
			  UNION ALL
			  SELECT s.trash_root,n.id FROM trash_subtree s JOIN nodes n ON n.parent_id=s.id
			)
			SELECT EXISTS(
			  SELECT 1 FROM nodes n LEFT JOIN trash_subtree s ON s.id=n.id
			  WHERE n.trashed_at IS NOT NULL
			  GROUP BY n.id HAVING COUNT(s.trash_root)!=1
			)`},
		{"trash subtree contains live node or mismatched timestamp", `
			WITH RECURSIVE trash_subtree(trash_root,root_stamp,id) AS (
			  SELECT id,trashed_at,id FROM nodes WHERE trash_name IS NOT NULL
			  UNION ALL
			  SELECT s.trash_root,s.root_stamp,n.id
			  FROM trash_subtree s JOIN nodes n ON n.parent_id=s.id
			)
			SELECT EXISTS(
			  SELECT 1 FROM trash_subtree s JOIN nodes n ON n.id=s.id
			  WHERE n.trashed_at IS NULL OR n.trashed_at!=s.root_stamp
			)`},
		{"content version size differs from blob authority", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN blobs b ON b.hash=v.blob_hash
			  WHERE v.size!=b.size
			)`},
		{"directory retains a content version", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id
			  WHERE n.kind!='file'
			)`},
		{"file current version is missing, historical, or belongs to another node", `
			SELECT EXISTS(
			  SELECT 1 FROM nodes n LEFT JOIN content_versions v ON v.version_id=n.current_version_id
			  WHERE n.kind='file' AND (v.version_id IS NULL OR v.node_id!=n.id OR v.node_revision IS NULL)
			)`},
		{"version revision exceeds current node revision", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id
			  WHERE v.node_revision>n.revision
			)`},
		{"current version is older than another retained known version", `
			SELECT EXISTS(
			  SELECT 1 FROM nodes n
			  JOIN content_versions current ON current.version_id=n.current_version_id
			  JOIN content_versions later ON later.node_id=n.id
			  WHERE later.node_revision>current.node_revision
			)`},
		{"content-create chronology is invalid", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v JOIN nodes n ON n.id=v.node_id
			  WHERE v.transition_kind='content_create'
			    AND (v.node_revision!=1 OR v.recorded_at!=n.created_at)
			)`},
		{"content replace/revert revision is not greater than one", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions
			  WHERE transition_kind IN ('content_replace','content_revert') AND node_revision<=1
			)`},
		{"revert source relationship is invalid", `
			SELECT EXISTS(
			  SELECT 1 FROM content_versions v
			  JOIN content_versions source ON source.version_id=v.source_version_id
			  JOIN nodes n ON n.id=v.node_id
			  WHERE v.transition_kind='content_revert' AND (
			    source.node_id!=v.node_id OR source.version_id=v.version_id
			    OR source.blob_hash!=v.blob_hash OR source.size!=v.size
			    OR NOT (source.media_type IS v.media_type)
			    OR (source.node_revision IS NOT NULL AND source.node_revision>=v.node_revision)
			    OR n.current_version_id=source.version_id
			  )
			)`},
		{"extracted text references missing blob authority", `
			SELECT EXISTS(
			  SELECT 1 FROM extracted_text e LEFT JOIN blobs b ON b.hash=e.blob_hash
			  WHERE b.hash IS NULL
			)`},
		{"provenance supersedes a fact on another node", `
			SELECT EXISTS(
			  SELECT 1 FROM provenance p JOIN provenance prior ON prior.identity=p.supersedes
			  WHERE p.node_id!=prior.node_id
			)`},
		{"provenance supersession graph contains a cycle", `
			WITH RECURSIVE chain(start,current) AS (
			  SELECT identity,supersedes FROM provenance WHERE supersedes IS NOT NULL
			  UNION ALL
			  SELECT chain.start,p.supersedes
			  FROM chain JOIN provenance p ON p.identity=chain.current
			  WHERE chain.current IS NOT NULL AND chain.current!=chain.start
			)
			SELECT EXISTS(SELECT 1 FROM chain WHERE current=start)
		`},
	}
	for _, check := range checks {
		var failed bool
		if err := tx.QueryRowContext(ctx, check.query).Scan(&failed); err != nil {
			return fmt.Errorf("checking %s: %w", check.name, err)
		}
		if failed {
			return errors.New(check.name)
		}
	}

	if err := validateStoredRecords(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking metadata v1 foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var table string
		var rowID, parent any
		var fk int64
		if err := rows.Scan(&table, &rowID, &parent, &fk); err != nil {
			return fmt.Errorf("reading metadata v1 foreign-key failure: %w", err)
		}
		return fmt.Errorf("metadata v1 violates foreign key %s[%v] constraint %d", table, rowID, fk)
	}
	return rows.Err()
}

func validateStoredRecords(ctx context.Context, tx *sql.Tx) error {
	if err := validateStoredBlobs(ctx, tx); err != nil {
		return err
	}
	if err := validateStoredNodes(ctx, tx); err != nil {
		return err
	}
	if err := validateStoredVersions(ctx, tx); err != nil {
		return err
	}
	if err := validateStoredIngests(ctx, tx); err != nil {
		return err
	}
	if err := validateStoredProvenance(ctx, tx); err != nil {
		return err
	}
	if err := validateStoredTags(ctx, tx); err != nil {
		return err
	}
	return validateStoredExtraction(ctx, tx)
}

func validateStoredBlobs(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT hash,size,created_at FROM blobs`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Blob{Type: "blob"}
		if err := rows.Scan(&record.Hash, &record.Size, &record.CreatedAt); err != nil {
			return err
		}
		if err := validateBlob(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateStoredNodes(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id,parent_id,name,kind,current_version_id,revision,created_at,modified_at,
		       trashed_at,trash_parent,trash_name FROM nodes`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Node{Type: "node"}
		var parent, trashParent sql.NullInt64
		var current, trashed sql.NullString
		var name, trashName any
		if err := rows.Scan(
			&record.ID, &parent, &name, &record.Kind, &current, &record.Revision,
			&record.CreatedAt, &record.ModifiedAt, &trashed, &trashParent, &trashName,
		); err != nil {
			return err
		}
		record.ParentID = nullInt64(parent)
		record.CurrentVersionID = nullString(current)
		record.TrashedAt = nullString(trashed)
		record.TrashParent = nullInt64(trashParent)
		record.Name, err = requiredBytes("node name", name)
		if err != nil {
			return err
		}
		record.TrashName, err = optionalBytes("node trash name", trashName)
		if err != nil {
			return err
		}
		if err := validateNode(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateStoredVersions(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT version_id,node_id,blob_hash,size,media_type,recorded_at,node_revision,
		       introduced_operation_id,transition_kind,source_version_id
		FROM content_versions`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := ContentVersion{Type: "content_version"}
		var media, source sql.NullString
		if err := rows.Scan(
			&record.VersionID, &record.NodeID, &record.BlobHash, &record.Size,
			&media, &record.RecordedAt, &record.NodeRevision,
			&record.IntroducedOperationID, &record.TransitionKind, &source,
		); err != nil {
			return err
		}
		record.MediaType = nullString(media)
		record.SourceVersionID = nullString(source)
		if err := validateContentVersion(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateStoredIngests(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT ingest_id,started_at,source_kind,source_desc FROM ingests`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Ingest{Type: "ingest"}
		var source any
		if err := rows.Scan(&record.IngestID, &record.StartedAt, &record.SourceKind, &source); err != nil {
			return err
		}
		record.SourceDesc, err = requiredBytes("ingest source description", source)
		if err != nil {
			return err
		}
		if err := validateIngest(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateStoredProvenance(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT identity,node_id,ingest_id,original_path,original_mtime,supersedes FROM provenance`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Provenance{Type: provenanceRecordType}
		var path any
		var mtime, supersedes sql.NullString
		if err := rows.Scan(
			&record.Identity, &record.NodeID, &record.IngestID, &path, &mtime, &supersedes,
		); err != nil {
			return err
		}
		record.OriginalPath, err = optionalBytes("provenance original path", path)
		if err != nil {
			return err
		}
		record.OriginalMTime = nullString(mtime)
		record.Supersedes = nullString(supersedes)
		if err := validateProvenance(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateStoredTags(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT tag_id,name FROM tags`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := Tag{Type: "tag"}
		if err := rows.Scan(&record.TagID, &record.Name); err != nil {
			return err
		}
		if err := validateTag(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateStoredExtraction(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT blob_hash,extractor,extractor_version,status,error,attempts,text,extracted_at
		FROM extracted_text`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := ExtractedText{Type: "extracted_text"}
		var errorText, text sql.NullString
		if err := rows.Scan(
			&record.BlobHash, &record.Extractor, &record.ExtractorVersion,
			&record.Status, &errorText, &record.Attempts, &text, &record.ExtractedAt,
		); err != nil {
			return err
		}
		record.Error = nullString(errorText)
		record.Text = nullString(text)
		if err := validateExtractedText(record); err != nil {
			return err
		}
	}
	return rows.Err()
}
