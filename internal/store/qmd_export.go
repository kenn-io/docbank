package store

import (
	"context"
	"errors"
	"fmt"
)

const maxQMDExportSources = 100_000

// QMDExportSource is one active live-node rendition head's retained
// sanitized-Markdown artifact. Original vault paths are intentionally absent.
type QMDExportSource struct {
	VaultUID                     string
	NodeID                       int64
	ContentVersionID             string
	ProcessingProfileFingerprint string
	AttachmentID                 string
	BuildID                      string
	ArtifactID                   string
	BlobSHA256                   string
	BlobSize                     int64
	ArtifactChecksum             string
	MarkdownChecksum             string
}

// QMDExportSources lists a bounded, deterministic snapshot of active retained
// sanitized Markdown for current versions of live nodes.
func (s *Store) QMDExportSources(ctx context.Context, limit int) (_ []QMDExportSource, retErr error) {
	if limit < 1 || limit > maxQMDExportSources {
		return nil, fmt.Errorf("QMD export source limit must be between 1 and %d", maxQMDExportSources)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.vault_uid,n.id,h.content_version_id,h.profile_fingerprint,
		       a.attachment_id,a.build_id,artifact.artifact_id,artifact.blob_hash,
		       artifact.size,artifact.checksum,b.markdown_checksum
		FROM nodes n
		JOIN rendition_heads h ON h.content_version_id=n.current_version_id
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
		JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
		JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
		  AND artifact.role='sanitized_markdown' AND artifact.state='verified'
		ORDER BY n.id,h.profile_fingerprint,a.attachment_id,artifact.artifact_id
		LIMIT ?`, limit+1)
	if err != nil {
		return nil, fmt.Errorf("listing QMD export sources: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	sources := make([]QMDExportSource, 0, min(limit, 128))
	for rows.Next() {
		var source QMDExportSource
		if err := rows.Scan(&source.VaultUID, &source.NodeID, &source.ContentVersionID,
			&source.ProcessingProfileFingerprint, &source.AttachmentID, &source.BuildID,
			&source.ArtifactID, &source.BlobSHA256, &source.BlobSize,
			&source.ArtifactChecksum, &source.MarkdownChecksum); err != nil {
			return nil, fmt.Errorf("scanning QMD export source: %w", err)
		}
		if len(sources) == limit {
			return nil, errors.New("QMD export source membership exceeds limit")
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing QMD export sources: %w", err)
	}
	return sources, nil
}
