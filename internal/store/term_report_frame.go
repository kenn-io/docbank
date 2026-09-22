package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/report"
)

var ErrUnknownReportCollection = errors.New("unknown report collection")

type termReportVersion struct {
	identity           report.Identity
	size               int64
	mimeType           string
	revision           int64
	createdAt          string
	recordedAt         string
	earliestRecordedAt string
	name               string
	hasSourceMetadata  bool
}

// MaterializeTermReportFrame binds every term to one lexical generation and
// one SQLite read snapshot. All current-version identities are captured before
// the read fence is released; later edits cannot alter its match bits.
// textBudget owns native bytes only and can close after date extraction.
func (s *Store) MaterializeTermReportFrame(
	ctx context.Context, request report.Request, selection report.CoverageSelection, budget, textBudget report.Budget,
) (report.Frame, error) {
	request, err := report.NormalizeRequest(request)
	if err != nil {
		return report.Frame{}, err
	}
	if budget == nil || textBudget == nil {
		return report.Frame{}, errors.New("missing report budget")
	}
	coverage, err := normalizeCoverageSelection(CoverageSelection{
		Configuration: selection.Configuration, ProfileFingerprint: selection.ProfileFingerprint,
	})
	if err != nil {
		return report.Frame{}, err
	}
	var frame report.Frame
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, generation LexicalGeneration) error {
		if coverage.Configuration == "configured" {
			if err := validateSnapshotCoverageProfile(ctx, q, coverage); err != nil {
				return err
			}
		}
		frame = report.Frame{VaultID: s.VaultID(), Request: request, ObservedAt: time.Now().UTC(),
			GenerationKind: "native", CoverageSelection: selection}
		if generation.ID != "" {
			frame.GenerationKind = "rendition"
			frame.GenerationID = generation.ID
		}
		witnesses, err := reportCollectionWitnesses(ctx, q, request, budget)
		if err != nil {
			return err
		}
		versions, versionInfo, err := readTermReportMembers(ctx, q, request, witnesses, budget, &frame)
		if err != nil {
			return err
		}
		for row, term := range request.Terms {
			if err := ctx.Err(); err != nil {
				return err
			}
			value := query.Query{V: 1, Text: term.Expression, Syntax: term.Syntax,
				Mode: "lexical", Sort: query.Sort{Field: string(DocumentCatalogSortName), Direction: "asc"}}
			compiled, err := compileQuery(ctx, value, queryResolver{q: q})
			if err != nil {
				return fmt.Errorf("term %d: %w", term.Number, err)
			}
			for _, dependency := range compiled.Dependencies {
				frame.Dependencies = append(frame.Dependencies, report.Dependency{
					Kind: string(dependency.Kind), ID: dependency.ID, Revision: dependency.Revision,
				})
			}
			population, err := matchedPopulation(compiled, generation.ID, &coverage.ProfileFingerprint)
			if err != nil {
				return err
			}
			statement, args, err := bindQueryPopulation(population, coverage, generation.ID)
			if err != nil {
				return err
			}
			rows, err := q.QueryContext(ctx, `SELECT node_id,content_version_id FROM (`+statement+`) report_matches`, args...)
			if err != nil {
				return fmt.Errorf("term %d population: %w", term.Number, err)
			}
			for rows.Next() {
				var nodeID int64
				var versionID string
				if err := rows.Scan(&nodeID, &versionID); err != nil {
					_ = rows.Close() //nolint:sqlclosecheck // Close the cursor immediately on scan failure.
					return err
				}
				if index, exists := versions[nodeID]; exists && frame.Members[index].Identity.VersionID == versionID {
					frame.Members[index].RawMatches[row] = true
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		for i, info := range versionInfo {
			if err := ctx.Err(); err != nil {
				return err
			}
			if info.hasSourceMetadata {
				// ponytail: load one metadata authority at a time; batch if measured
				// metadata-heavy reports outgrow the preparation deadline.
				if err := s.readTermReportDateAuthority(ctx, q, info, budget, &frame); err != nil {
					return fmt.Errorf("document %d date evidence: %w", info.identity.NodeID, err)
				}
			}
			if generation.ID == "" {
				if err := readTermReportNativeText(ctx, q, info, i, budget, textBudget, &frame); err != nil {
					return fmt.Errorf("document %d native text: %w", info.identity.NodeID, err)
				}
			} else {
				if err := readTermReportRendition(ctx, q, info, i, coverage, generation.ID, budget, &frame); err != nil {
					return fmt.Errorf("document %d rendition text: %w", info.identity.NodeID, err)
				}
			}
		}
		if err := readTermReportFamilies(ctx, q, budget, &frame); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return report.Frame{}, err
	}
	return frame, nil
}

func reportCollectionWitnesses(ctx context.Context, q metadataQuerier, request report.Request, budget report.Budget) (map[int64][]report.CollectionWitness, error) {
	result := make(map[int64][]report.CollectionWitness)
	if request.AllDocuments {
		return result, nil
	}
	collectionIDs, err := json.Marshal(request.CollectionIDs)
	if err != nil {
		return nil, err
	}
	var found int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM ingests
		WHERE source_kind NOT LIKE 'embedded:%' AND id IN (SELECT value FROM json_each(?))`,
		string(collectionIDs)).Scan(&found); err != nil {
		return nil, err
	}
	if found != len(request.CollectionIDs) {
		return nil, ErrUnknownReportCollection
	}
	rows, err := q.QueryContext(ctx, `SELECT p.ingest_id,p.node_id,p.identity,p.original_path,
		COALESCE(p.original_mtime,''),COALESCE(p.supersedes,'')
		FROM provenance p JOIN ingests i ON i.id=p.ingest_id JOIN nodes n ON n.id=p.node_id
		WHERE i.source_kind NOT LIKE 'embedded:%' AND n.kind='file' AND n.trashed_at IS NULL
		AND p.ingest_id IN (SELECT value FROM json_each(?))
		AND NOT EXISTS (SELECT 1 FROM provenance later WHERE later.supersedes=p.identity)
		ORDER BY p.node_id,p.ingest_id,p.identity`, string(collectionIDs))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // Close the cursor before returning early.
			return nil, err
		}
		var id, identity, path, mtime, supersedes string
		var nodeID int64
		if err := rows.Scan(&id, &nodeID, &identity, &path, &mtime, &supersedes); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if len(result) > 50000 {
			_ = rows.Close()
			return nil, fmt.Errorf("%w: report scope exceeds member limit", report.ErrReportLimit)
		}
		if _, err := budget.Reserve(ctx, int64(512+len(path)+len(mtime))); err != nil {
			_ = rows.Close()
			return nil, err
		}
		witness := report.CollectionWitness{
			CollectionID: id, MembershipID: identity, OriginalPath: path,
			OriginalMTime: mtime, Supersedes: supersedes,
		}
		witness.MembershipSHA256 = report.WitnessDigest(nodeID, witness)
		result[nodeID] = append(result[nodeID], witness)
	}
	return result, errors.Join(rows.Err(), rows.Close())
}

func readTermReportMembers(ctx context.Context, q metadataQuerier, request report.Request,
	witnesses map[int64][]report.CollectionWitness, budget report.Budget, frame *report.Frame,
) (map[int64]int, []termReportVersion, error) {
	index := make(map[int64]int)
	versions := make([]termReportVersion, 0)
	var scope string
	var args []any
	if !request.AllDocuments {
		nodeIDs := make([]int64, 0, len(witnesses))
		for nodeID := range witnesses {
			nodeIDs = append(nodeIDs, nodeID)
		}
		encoded, err := json.Marshal(nodeIDs)
		if err != nil {
			return nil, nil, err
		}
		scope = ` AND n.id IN (SELECT value FROM json_each(?))`
		args = append(args, string(encoded))
	}
	rows, err := q.QueryContext(ctx, `SELECT n.id,cv.version_id,cv.blob_hash,cv.size,COALESCE(cv.mime_type,''),
		n.revision,n.created_at,cv.recorded_at,
		(SELECT MIN(old.recorded_at) FROM content_versions old WHERE old.node_id=n.id),n.name,
		EXISTS(SELECT 1 FROM source_metadata_heads h WHERE h.source_sha256=cv.blob_hash)
		FROM nodes n JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
		WHERE n.kind='file' AND n.trashed_at IS NULL`+scope+` ORDER BY n.id`, args...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // Close the cursor before returning early.
			return nil, nil, err
		}
		var version termReportVersion
		if err := rows.Scan(&version.identity.NodeID, &version.identity.VersionID, &version.identity.SHA256,
			&version.size, &version.mimeType, &version.revision, &version.createdAt,
			&version.recordedAt, &version.earliestRecordedAt, &version.name, &version.hasSourceMetadata); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		if len(frame.Members) == 50000 {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("%w: report scope exceeds member limit", report.ErrReportLimit)
		}
		if _, err := budget.Reserve(ctx, int64(2048+len(version.name)+len(request.Terms))); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		member := report.Member{Identity: version.identity,
			Kind:                string(DocumentEventKindForMIMEType(version.mimeType)),
			CollectionWitnesses: witnesses[version.identity.NodeID],
			RawMatches:          make([]bool, len(request.Terms)),
			Coverage:            report.MemberCoverage{SearchState: "missing", DateEvidenceState: report.StateComplete, FamilyState: report.StateComplete},
		}
		date := version.createdAt
		basis := "node_created_at"
		role, sourceClass := "imported", "vault_addition"
		if date == "" {
			date = version.earliestRecordedAt
			basis = "earliest_retained_observation"
			role, sourceClass = "vault_recorded", "vault_observation"
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", version.identity.NodeID, basis, date)))
		member.Candidates = []report.DateCandidate{{
			ID: hex.EncodeToString(digest[:]), Document: version.identity,
			Role: role, SourceClass: sourceClass, Raw: date, Value: date,
			Precision: "second", Confidence: "source_asserted", ClaimBasis: basis,
			Locator: report.Locator{EvidenceID: basis, EvidenceSHA256: hex.EncodeToString(digest[:])},
		}}
		index[version.identity.NodeID] = len(frame.Members)
		versions = append(versions, version)
		frame.Members = append(frame.Members, member)
	}
	return index, versions, errors.Join(rows.Err(), rows.Close())
}

func (s *Store) readTermReportDateAuthority(ctx context.Context, q metadataQuerier,
	info termReportVersion, budget report.Budget, frame *report.Frame,
) error {
	target := DocumentEventTarget{ContentVersionID: info.identity.VersionID,
		BlobHash: info.identity.SHA256, MIMEType: info.mimeType, RecordedAt: info.recordedAt,
		NodeID: info.identity.NodeID, Size: info.size}
	snapshot, err := s.loadDocumentEventEvidenceTx(ctx, q, target, true)
	if err != nil {
		return err
	}
	for _, field := range snapshot.Metadata.Fields {
		if field.Sensitive {
			continue
		}
		var raw, normalized, precision, timezone string
		if stamp := field.Value.Timestamp; stamp != nil {
			raw, normalized = stamp.Raw, stamp.Normalized
			precision, timezone = string(stamp.Precision), string(stamp.Timezone)
		} else if field.Value.String != nil && strings.HasSuffix(field.Key, ".raw") {
			raw = *field.Value.String
		} else {
			continue
		}
		if _, err := budget.Reserve(ctx, int64(1024+len(raw)+len(normalized))); err != nil {
			return err
		}
		frame.RawDateFields = append(frame.RawDateFields, report.RawDateField{
			Document: info.identity, Namespace: field.Namespace, SourceField: field.SourceField,
			Key: field.Key, Raw: raw, Normalized: normalized,
			Precision: precision, Timezone: timezone,
			GenerationID:     snapshot.MetadataGeneration.GenerationID,
			GenerationSHA256: snapshot.MetadataGeneration.Checksum,
			InputFingerprint: snapshot.InputsSHA256, ProjectionState: "raw",
		})
	}
	return nil
}

func readTermReportNativeText(ctx context.Context, q metadataQuerier, info termReportVersion,
	index int, budget, textBudget report.Budget, frame *report.Frame,
) error {
	var extractionID, textBytes int64
	var extractor, status string
	err := q.QueryRowContext(ctx, `SELECT e.id,e.extractor,e.status,COALESCE(length(CAST(e.text AS BLOB)),0)
		FROM text_searchable_versions sv JOIN extracted_text e
		ON e.blob_hash=? WHERE sv.version_id=?
		ORDER BY e.status='ok' DESC,e.extracted_at DESC,e.extractor DESC LIMIT 1`,
		info.identity.SHA256, info.identity.VersionID).Scan(&extractionID, &extractor, &status, &textBytes)
	if errors.Is(err, sql.ErrNoRows) {
		frame.Members[index].Coverage.SearchState = "missing"
		return nil
	}
	if err != nil {
		return err
	}
	if status != "ok" || textBytes == 0 {
		frame.Members[index].Coverage.SearchState = "missing"
		return nil
	}
	if textBytes > 16<<20 {
		return fmt.Errorf("%w: native text exceeds per-document report limit", report.ErrReportLimit)
	}
	if _, err := budget.Reserve(ctx, 1024); err != nil {
		return err
	}
	if _, err := textBudget.Reserve(ctx, textBytes); err != nil {
		return err
	}
	// The enclosing read snapshot keeps this row fixed between the size check
	// and payload read. Reserve before asking the driver to allocate the text.
	var text string
	if err := q.QueryRowContext(ctx, `SELECT text FROM extracted_text WHERE id=?`, extractionID).Scan(&text); err != nil {
		return err
	}
	bytes := []byte(text)
	digest := sha256.Sum256(bytes)
	frame.Texts = append(frame.Texts, report.TextBinding{
		Kind: "native", Document: info.identity, NodeRevision: info.revision,
		Size: int64(len(bytes)), Native: &report.NativeText{
			Text: bytes, TextSHA256: hex.EncodeToString(digest[:]),
			ExtractorID: extractor, Status: status, SearchableVersionID: info.identity.VersionID,
		},
	})
	frame.Members[index].Coverage.SearchState = report.StateComplete
	return nil
}

// readTermReportRendition captures the entire authority tuple while the lexical
// generation and metadata snapshot are held. Physical reads later use only this
// immutable artifact hash, never a live rendition head.
func readTermReportRendition(ctx context.Context, q metadataQuerier, info termReportVersion,
	index int, coverage CoverageSelection, generationID string, budget report.Budget, frame *report.Frame,
) error {
	if coverage.Configuration != "configured" {
		frame.Members[index].Coverage.SearchState = "missing"
		return nil
	}
	binding := report.TextBinding{Kind: "rendition", Document: info.identity,
		NodeRevision: info.revision, ProfileFingerprint: coverage.ProfileFingerprint,
		GenerationID: generationID, ArtifactRole: catalogArtifactSanitizedMarkdown}
	var sourceHash, markdownHash, artifactChecksum string
	err := q.QueryRowContext(ctx, `SELECT h.attachment_id,a.build_id,b.source_sha256,b.markdown_checksum,
		x.artifact_id,x.blob_hash,x.size,x.checksum
		FROM rendition_heads h
		JOIN rendition_attachments a ON a.attachment_id=h.attachment_id
			AND a.content_version_id=h.content_version_id AND a.profile_fingerprint=h.profile_fingerprint
		JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
		JOIN rendition_lexical_generation_builds gb ON gb.build_id=b.build_id AND gb.generation_id=?
		JOIN rendition_artifacts x ON x.build_id=b.build_id AND x.role=?
		WHERE h.content_version_id=? AND h.profile_fingerprint=?
		ORDER BY x.artifact_id LIMIT 1`, generationID, catalogArtifactSanitizedMarkdown,
		info.identity.VersionID, coverage.ProfileFingerprint).Scan(
		&binding.AttachmentID, &binding.BuildID, &sourceHash, &markdownHash,
		&binding.ArtifactID, &binding.ArtifactSHA256, &binding.Size, &artifactChecksum)
	if errors.Is(err, sql.ErrNoRows) {
		frame.Members[index].Coverage.SearchState = "missing"
		return nil
	}
	if err != nil {
		return err
	}
	if sourceHash != info.identity.SHA256 || binding.ArtifactSHA256 != markdownHash ||
		binding.ArtifactSHA256 != artifactChecksum || binding.Size < 0 {
		return ErrRenditionTextUnavailable
	}
	if binding.Size > 16<<20 {
		return fmt.Errorf("%w: rendition text exceeds per-document report limit", report.ErrReportLimit)
	}
	if _, err := budget.Reserve(ctx, 1024); err != nil {
		return err
	}
	binding.RenditionID = binding.ArtifactID
	frame.Texts = append(frame.Texts, binding)
	frame.Members[index].Coverage.SearchState = report.StateComplete
	return nil
}
