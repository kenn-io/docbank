package store

import (
	"context"
	"database/sql"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const maxQMDExportSources = 100_000

const qmdExportCandidatePageSize = 256

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
	if ctx == nil {
		return SearchOptions{}, errors.New("QMD search scope requires context")
	}
	if err := ctx.Err(); err != nil {
		return SearchOptions{}, err
	}
	return s.normalizeSearchOptions(ctx, opts)
}

// QMDExportSources lists a bounded, deterministic snapshot of active retained
// sanitized Markdown for current versions of live nodes.
func (s *Store) QMDExportSources(ctx context.Context, limit int) ([]QMDExportSource, error) {
	return s.qmdExportSources(ctx, limit, maxQMDExportSources, qmdExportCandidatePageSize)
}

// RevalidateQMDExportCandidates rejects the batch unless every supplied
// manifest identity remains live, current, and inside the operator scope in
// one storage snapshot.
func (s *Store) RevalidateQMDExportCandidates(
	ctx context.Context, candidates []QMDExportSource, opts SearchOptions,
) ([]QMDExportLiveCandidate, error) {
	if ctx == nil {
		return nil, errors.New("QMD candidate revalidation requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) > document.MaxRetrievalCandidateLimit {
		return nil, errors.New("QMD candidate revalidation exceeds the retrieval limit")
	}
	if len(opts.ContentVersionIDs) > MaxSearchSourceFenceIDs {
		return nil, errors.New("search source fence exceeds 4096 content versions")
	}
	owned := slices.Clone(candidates)
	opts.ContentVersionIDs = slices.Clone(opts.ContentVersionIDs)
	seen := make(map[qmdExportCandidateIdentity]struct{}, len(owned))
	for _, candidate := range owned {
		if !validQMDExportSource(candidate) {
			return nil, errors.New("QMD candidate authority is invalid")
		}
		identity := qmdCandidateIdentity(candidate)
		if _, duplicate := seen[identity]; duplicate {
			return nil, errors.New("QMD candidate authority is duplicated")
		}
		seen[identity] = struct{}{}
	}
	result := make([]QMDExportLiveCandidate, 0, len(owned))
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		normalized, err := s.normalizeSearchOptionsWithQuerier(ctx, tx, opts)
		if err != nil {
			return err
		}
		filterSQL, filterArgs := searchFilterSQL(normalized)
		matched := make([]qmdExportCandidate, 0, len(owned))
		for _, candidate := range owned {
			args := []any{candidate.NodeID, candidate.ContentVersionID,
				candidate.ProcessingProfileFingerprint, candidate.AttachmentID,
				candidate.VaultUID, candidate.BuildID, candidate.ArtifactID,
				candidate.BlobSHA256, candidate.BlobSize, candidate.ArtifactChecksum,
				candidate.MarkdownChecksum}
			args = append(args, filterArgs...)
			var live QMDExportLiveCandidate
			var sourceSHA256 string
			err := tx.QueryRowContext(ctx, `SELECT n.id,n.revision,cv.version_id,b.source_sha256
				FROM nodes n
				JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
				JOIN rendition_heads h ON h.content_version_id=cv.version_id
				JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
					AND a.content_version_id=h.content_version_id
					AND a.profile_fingerprint=h.profile_fingerprint
				JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
				JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
				WHERE n.id=? AND cv.version_id=? AND h.profile_fingerprint=? AND h.attachment_id=?
					AND a.vault_uid=? AND a.build_id=? AND artifact.artifact_id=?
					AND artifact.role='sanitized_markdown'
					AND artifact.blob_hash=? AND artifact.size=? AND artifact.checksum=?
					AND b.markdown_checksum=? AND n.kind='file' AND n.trashed_at IS NULL `+filterSQL,
				args...).Scan(&live.NodeID, &live.NodeRevision, &live.ContentVersionID, &sourceSHA256)
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
			matched = append(matched, qmdExportCandidate{Source: candidate, SourceSHA256: sourceSHA256})
		}
		for start := 0; start < len(matched); start += qmdExportCandidatePageSize {
			end := min(start+qmdExportCandidatePageSize, len(matched))
			suppressed, err := qmdExportSuppressedCandidates(ctx, tx, matched[start:end])
			if err != nil {
				return err
			}
			if len(suppressed) != 0 {
				return ErrQMDExportAuthorityStale
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type qmdExportCandidateIdentity struct {
	VaultUID                     string
	NodeID                       int64
	ContentVersionID             string
	ProcessingProfileFingerprint string
	AttachmentID                 string
	BuildID                      string
	ArtifactID                   string
	BlobSHA256                   string
	ArtifactChecksum             string
}

func qmdCandidateIdentity(source QMDExportSource) qmdExportCandidateIdentity {
	return qmdExportCandidateIdentity{
		VaultUID: source.VaultUID, NodeID: source.NodeID, ContentVersionID: source.ContentVersionID,
		ProcessingProfileFingerprint: source.ProcessingProfileFingerprint,
		AttachmentID:                 source.AttachmentID, BuildID: source.BuildID, ArtifactID: source.ArtifactID,
		BlobSHA256: source.BlobSHA256, ArtifactChecksum: source.ArtifactChecksum,
	}
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

func (s *Store) qmdExportSources(
	ctx context.Context, limit, scanLimit, pageSize int,
) (sources []QMDExportSource, retErr error) {
	if ctx == nil {
		return nil, errors.New("qmd export requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > maxQMDExportSources {
		return nil, fmt.Errorf("QMD export source limit must be between 1 and %d", maxQMDExportSources)
	}
	if scanLimit < 1 || scanLimit > maxQMDExportSources || pageSize < 1 || pageSize > qmdExportCandidatePageSize {
		return nil, errors.New("QMD export source scan bounds are invalid")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("starting QMD export source snapshot: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		retErr = errors.Join(retErr, rollbackErr)
		if retErr != nil {
			sources = nil
		}
	}()

	sources = make([]QMDExportSource, 0, min(limit, 128))
	cursor := qmdExportCursor{}
	examined := 0
	for {
		pageLimit := min(pageSize, scanLimit-examined+1)
		page, err := queryQMDExportCandidatePage(ctx, tx, cursor, pageLimit)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for range page {
			examined++
			if examined > scanLimit {
				return nil, errors.New("QMD export source candidate scan exceeds bound")
			}
		}
		suppressed, err := qmdExportSuppressedCandidates(ctx, tx, page)
		if err != nil {
			return nil, err
		}
		for index, candidate := range page {
			if suppressed[index] {
				continue
			}
			if len(sources) == limit {
				return nil, errors.New("QMD export source membership exceeds limit")
			}
			sources = append(sources, candidate.Source)
		}
		last := page[len(page)-1]
		cursor = qmdExportCursor{NodeID: last.Source.NodeID,
			ProfileFingerprint: last.Source.ProcessingProfileFingerprint,
			AttachmentID:       last.Source.AttachmentID, ArtifactID: last.Source.ArtifactID}
		if len(page) < pageLimit {
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("closing QMD export source snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sources, nil
}

type qmdExportCursor struct {
	NodeID             int64
	ProfileFingerprint string
	AttachmentID       string
	ArtifactID         string
}

type qmdExportCandidate struct {
	Source       QMDExportSource
	SourceSHA256 string
}

func queryQMDExportCandidatePage(
	ctx context.Context, tx *sql.Tx, cursor qmdExportCursor, limit int,
) (_ []qmdExportCandidate, retErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.vault_uid,n.id,h.content_version_id,h.profile_fingerprint,
		       a.attachment_id,a.build_id,artifact.artifact_id,artifact.blob_hash,
		       artifact.size,artifact.checksum,b.markdown_checksum,b.source_sha256
		FROM nodes n
		JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
		JOIN rendition_heads h ON h.content_version_id=cv.version_id
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
			AND a.content_version_id=h.content_version_id
			AND a.profile_fingerprint=h.profile_fingerprint
		JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
		JOIN rendition_artifacts artifact ON artifact.build_id=b.build_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
		  AND artifact.role='sanitized_markdown'
		  AND (n.id,h.profile_fingerprint,a.attachment_id,artifact.artifact_id) > (?,?,?,?)
		ORDER BY n.id,h.profile_fingerprint,a.attachment_id,artifact.artifact_id
		LIMIT ?`, cursor.NodeID, cursor.ProfileFingerprint, cursor.AttachmentID, cursor.ArtifactID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing QMD export sources: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	candidates := make([]qmdExportCandidate, 0, limit)
	for rows.Next() {
		var candidate qmdExportCandidate
		if err := rows.Scan(&candidate.Source.VaultUID, &candidate.Source.NodeID,
			&candidate.Source.ContentVersionID, &candidate.Source.ProcessingProfileFingerprint,
			&candidate.Source.AttachmentID, &candidate.Source.BuildID, &candidate.Source.ArtifactID,
			&candidate.Source.BlobSHA256, &candidate.Source.BlobSize,
			&candidate.Source.ArtifactChecksum, &candidate.Source.MarkdownChecksum,
			&candidate.SourceSHA256); err != nil {
			return nil, fmt.Errorf("scanning QMD export source: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing QMD export sources: %w", err)
	}
	return candidates, nil
}

type qmdExportSuppressionProbe struct {
	Index        int    `json:"index"`
	SourceSHA256 string `json:"source_sha256"`
	BuildID      string `json:"build_id"`
	Scope        string `json:"scope"`
}

func qmdExportSuppressedCandidates(
	ctx context.Context, tx *sql.Tx, candidates []qmdExportCandidate,
) (_ map[int]bool, retErr error) {
	probes := make([]qmdExportSuppressionProbe, len(candidates))
	for index, candidate := range candidates {
		probes[index] = qmdExportSuppressionProbe{Index: index,
			SourceSHA256: candidate.SourceSHA256, BuildID: candidate.Source.BuildID,
			Scope: derivativeAttachmentSuppressionScope(candidate.Source.ContentVersionID,
				candidate.Source.ProcessingProfileFingerprint)}
	}
	encoded, err := json.Marshal(probes, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encoding QMD export suppression probes: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		WITH probes AS (
			SELECT json_extract(value,'$.index') AS candidate_index,
			       json_extract(value,'$.source_sha256') AS source_sha256,
			       json_extract(value,'$.build_id') AS build_id,
			       json_extract(value,'$.scope') AS attachment_scope
			FROM json_each(?)
		)
		SELECT probes.candidate_index
		FROM probes
		WHERE EXISTS (
			SELECT 1 FROM derivative_purge_suppressions suppression
			WHERE suppression.source_sha256=probes.source_sha256
			  AND suppression.build_id=probes.build_id AND suppression.active=1
			  AND suppression.profile_fingerprint IN (probes.attachment_scope,?)
		)
		ORDER BY probes.candidate_index`, encoded, derivativeBuildSuppressionProfile)
	if err != nil {
		return nil, fmt.Errorf("checking QMD export source suppression: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	suppressed := make(map[int]bool)
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, fmt.Errorf("scanning QMD export source suppression: %w", err)
		}
		if index < 0 || index >= len(candidates) {
			return nil, errors.New("QMD export source suppression result is invalid")
		}
		suppressed[index] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking QMD export source suppression: %w", err)
	}
	return suppressed, nil
}
