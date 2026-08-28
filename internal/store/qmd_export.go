package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const maxQMDExportSources = 100_000

// ErrQMDExportAuthorityStale means an exported identity no longer names the
// exact live node, content version, rendition head, and verified artifact.
var ErrQMDExportAuthorityStale = errors.New("QMD export authority is stale")

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

// QMDExportLiveCandidate is the current Docbank identity rejoined after an
// external QMD result has been mapped through one exact export manifest.
type QMDExportLiveCandidate struct {
	NodeID           int64
	NodeRevision     int64
	ContentVersionID string
	Path             string
}

// NormalizeQMDSearchScope resolves and validates the exact local scope before
// any private query is disclosed to an operator-hosted QMD endpoint.
func (s *Store) NormalizeQMDSearchScope(ctx context.Context, opts SearchOptions) (SearchOptions, error) {
	return s.normalizeSearchOptions(ctx, opts)
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

// RevalidateQMDExportCandidates rejects the batch unless every supplied
// manifest identity remains exact, live, current, and inside the operator
// scope in one storage snapshot.
func (s *Store) RevalidateQMDExportCandidates(ctx context.Context, candidates []QMDExportSource,
	opts SearchOptions,
) ([]QMDExportLiveCandidate, error) {
	if len(candidates) > document.MaxRetrievalCandidateLimit {
		return nil, errors.New("QMD candidate revalidation exceeds the retrieval limit")
	}
	normalized, err := s.normalizeSearchOptions(ctx, opts)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{}, len(candidates))
	for _, candidate := range candidates {
		if !validQMDExportSource(candidate) {
			return nil, errors.New("QMD candidate authority is invalid")
		}
		if _, duplicate := seen[candidate.NodeID]; duplicate {
			return nil, errors.New("QMD candidate authority is duplicated")
		}
		seen[candidate.NodeID] = struct{}{}
	}
	result := make([]QMDExportLiveCandidate, 0, len(candidates))
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		filterSQL, filterArgs := searchFilterSQL(normalized)
		for _, candidate := range candidates {
			args := []any{candidate.NodeID, candidate.ContentVersionID,
				candidate.ProcessingProfileFingerprint, candidate.AttachmentID,
				candidate.VaultUID, candidate.BuildID, candidate.ArtifactID,
				candidate.BlobSHA256, candidate.BlobSize, candidate.ArtifactChecksum,
				candidate.MarkdownChecksum}
			args = append(args, filterArgs...)
			var live QMDExportLiveCandidate
			err := tx.QueryRowContext(ctx, `SELECT n.id,n.revision,cv.version_id
				FROM nodes n
				JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
				JOIN rendition_heads h ON h.content_version_id=cv.version_id
				JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
					AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
				JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
				JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
				WHERE n.id=? AND cv.version_id=? AND h.profile_fingerprint=? AND h.attachment_id=?
					AND a.vault_uid=? AND a.build_id=? AND artifact.artifact_id=?
					AND artifact.role='sanitized_markdown' AND artifact.state='verified'
					AND artifact.blob_hash=? AND artifact.size=? AND artifact.checksum=?
					AND b.markdown_checksum=? AND n.kind='file' AND n.trashed_at IS NULL `+filterSQL,
				args...).Scan(&live.NodeID, &live.NodeRevision, &live.ContentVersionID)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrQMDExportAuthorityStale
			}
			if err != nil {
				return err
			}
			live.Path, err = pathOf(ctx, tx, live.NodeID)
			if err != nil {
				return err
			}
			result = append(result, live)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func validQMDExportSource(source QMDExportSource) bool {
	if source.NodeID < 1 || source.BlobSize < 0 || source.BlobSHA256 != source.ArtifactChecksum ||
		source.BlobSHA256 != source.MarkdownChecksum {
		return false
	}
	for _, value := range []string{source.VaultUID, source.ContentVersionID,
		source.ProcessingProfileFingerprint, source.AttachmentID, source.BuildID,
		source.ArtifactID, source.BlobSHA256, source.ArtifactChecksum, source.MarkdownChecksum} {
		if value == "" || len(value) > 1024 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return false
		}
	}
	for _, value := range []string{source.ProcessingProfileFingerprint, source.BuildID,
		source.BlobSHA256, source.ArtifactChecksum, source.MarkdownChecksum} {
		if len(value) != 64 || strings.IndexFunc(value, func(r rune) bool {
			return r < '0' || r > '9' && r < 'a' || r > 'f'
		}) >= 0 {
			return false
		}
	}
	return true
}
