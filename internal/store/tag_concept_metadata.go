package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

const (
	metadataTagConceptType     = "tag_concept"
	metadataTagAliasType       = "tag_alias"
	metadataTagConceptEdgeType = "tag_concept_edge"
	metadataTagRedirectType    = "tag_redirect"
	metadataTagMergeAuditType  = "tag_merge_audit"
	metadataPassageTagType     = "passage_tag"
)

type metadataTagConcept struct {
	Type        string `json:"type"`
	TagID       string `json:"tag_id"`
	Description string `json:"description"`
	Revision    int64  `json:"revision"`
}

type metadataTagAlias struct {
	Type  string `json:"type"`
	Alias string `json:"alias"`
	TagID string `json:"tag_id"`
}

type metadataTagConceptEdge struct {
	Type        string `json:"type"`
	ParentTagID string `json:"parent_tag_id"`
	ChildTagID  string `json:"child_tag_id"`
	Kind        string `json:"kind"`
}

type metadataTagRedirect struct {
	Type        string `json:"type"`
	SourceTagID string `json:"source_tag_id"`
	TargetTagID string `json:"target_tag_id"`
	SourceName  string `json:"source_name"`
	MergedAt    string `json:"merged_at"`
	MergeID     string `json:"merge_id"`
}

type metadataTagMergeAudit struct {
	Type           string  `json:"type"`
	MergeID        string  `json:"merge_id"`
	SourceTagID    string  `json:"source_tag_id"`
	TargetTagID    string  `json:"target_tag_id"`
	SourceRevision int64   `json:"source_revision"`
	TargetRevision int64   `json:"target_revision"`
	PreviewJSON    []byte  `json:"preview_json" format:"byte"`
	CommittedAt    string  `json:"committed_at"`
	ReversedAt     *string `json:"reversed_at"`
}

type metadataPassageTag struct {
	Type             string `json:"type"`
	PassageID        string `json:"passage_id"`
	TagID            string `json:"tag_id"`
	RefJSON          []byte `json:"ref_json" format:"byte"`
	DocumentUID      string `json:"document_uid"`
	ContentVersionID string `json:"content_version_id"`
}

func exportTagConceptMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	if err := exportConceptRows(ctx, tx, `SELECT tag_id,description,revision FROM tag_concepts ORDER BY tag_id`,
		func(rows *sql.Rows) (any, error) {
			v := metadataTagConcept{Type: metadataTagConceptType}
			err := rows.Scan(&v.TagID, &v.Description, &v.Revision)
			return v, err
		}, write); err != nil {
		return err
	}
	if err := exportConceptRows(ctx, tx, `SELECT alias,tag_id FROM tag_aliases ORDER BY alias`,
		func(rows *sql.Rows) (any, error) {
			v := metadataTagAlias{Type: metadataTagAliasType}
			err := rows.Scan(&v.Alias, &v.TagID)
			return v, err
		}, write); err != nil {
		return err
	}
	if err := exportConceptRows(ctx, tx, `SELECT parent_tag_id,child_tag_id,kind FROM tag_concept_edges ORDER BY parent_tag_id,child_tag_id,kind`,
		func(rows *sql.Rows) (any, error) {
			v := metadataTagConceptEdge{Type: metadataTagConceptEdgeType}
			err := rows.Scan(&v.ParentTagID, &v.ChildTagID, &v.Kind)
			return v, err
		}, write); err != nil {
		return err
	}
	if err := exportConceptRows(ctx, tx, `SELECT source_tag_id,target_tag_id,source_name,merged_at,merge_id FROM tag_redirects ORDER BY source_tag_id`,
		func(rows *sql.Rows) (any, error) {
			v := metadataTagRedirect{Type: metadataTagRedirectType}
			err := rows.Scan(&v.SourceTagID, &v.TargetTagID, &v.SourceName, &v.MergedAt, &v.MergeID)
			return v, err
		}, write); err != nil {
		return err
	}
	return exportConceptRows(ctx, tx, `SELECT merge_id,source_tag_id,target_tag_id,source_revision,target_revision,preview_json,committed_at,reversed_at FROM tag_merge_audit ORDER BY merge_id`,
		func(rows *sql.Rows) (any, error) {
			v := metadataTagMergeAudit{Type: metadataTagMergeAuditType}
			var reversed sql.NullString
			err := rows.Scan(&v.MergeID, &v.SourceTagID, &v.TargetTagID, &v.SourceRevision, &v.TargetRevision, &v.PreviewJSON, &v.CommittedAt, &reversed)
			v.ReversedAt = stringPtr(reversed)
			return v, err
		}, write)
}

func exportPassageTagMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	return exportConceptRows(ctx, tx, `SELECT passage_id,tag_id,ref_json,document_uid,content_version_id FROM passage_tags ORDER BY passage_id,tag_id`,
		func(rows *sql.Rows) (any, error) {
			v := metadataPassageTag{Type: metadataPassageTagType}
			err := rows.Scan(&v.PassageID, &v.TagID, &v.RefJSON, &v.DocumentUID, &v.ContentVersionID)
			return v, err
		}, write)
}

func exportConceptRows(ctx context.Context, tx metadataQuerier, query string,
	scan func(*sql.Rows) (any, error), write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("exporting concept metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return fmt.Errorf("scanning concept metadata: %w", err)
		}
		if err := validateTagConceptMetadataRecord(v); err != nil {
			return err
		}
		if err := write(v); err != nil {
			return err
		}
	}
	return rowsError("concept metadata", rows)
}

func importTagConceptMetadata(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataTagConceptType:
		var v metadataTagConcept
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateTagConceptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tag_concepts(tag_id,description,revision) VALUES(?,?,?)`, v.TagID, v.Description, v.Revision)
		return err
	case metadataTagAliasType:
		var v metadataTagAlias
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateTagConceptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tag_aliases(alias,tag_id) VALUES(?,?)`, v.Alias, v.TagID)
		return err
	case metadataTagConceptEdgeType:
		var v metadataTagConceptEdge
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateTagConceptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tag_concept_edges(parent_tag_id,child_tag_id,kind) VALUES(?,?,?)`, v.ParentTagID, v.ChildTagID, v.Kind)
		return err
	case metadataTagRedirectType:
		var v metadataTagRedirect
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateTagConceptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tag_redirects(source_tag_id,target_tag_id,source_name,merged_at,merge_id) VALUES(?,?,?,?,?)`, v.SourceTagID, v.TargetTagID, v.SourceName, v.MergedAt, v.MergeID)
		return err
	case metadataTagMergeAuditType:
		var v metadataTagMergeAudit
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateTagConceptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tag_merge_audit(merge_id,source_tag_id,target_tag_id,source_revision,target_revision,preview_json,committed_at,reversed_at) VALUES(?,?,?,?,?,?,?,?)`, v.MergeID, v.SourceTagID, v.TargetTagID, v.SourceRevision, v.TargetRevision, v.PreviewJSON, v.CommittedAt, v.ReversedAt)
		return err
	case metadataPassageTagType:
		var v metadataPassageTag
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validateTagConceptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO passage_tags(passage_id,tag_id,ref_json,document_uid,content_version_id) VALUES(?,?,?,?,?)`, v.PassageID, v.TagID, v.RefJSON, v.DocumentUID, v.ContentVersionID)
		return err
	default:
		return fmt.Errorf("unknown concept metadata type %q", kind)
	}
}

func validateTagConceptMetadataRecord(value any) error {
	switch v := value.(type) {
	case metadataTagConcept:
		if v.Type != metadataTagConceptType || v.Revision < 1 || !validConceptDescription(v.Description) {
			return errors.New("invalid tag concept metadata")
		}
		return validateUUIDv4(v.TagID)
	case metadataTagAlias:
		if v.Type != metadataTagAliasType {
			return errors.New("invalid tag alias metadata")
		}
		if err := validateCanonicalConceptName(v.Alias); err != nil {
			return err
		}
		return validateUUIDv4(v.TagID)
	case metadataTagConceptEdge:
		if v.Type != metadataTagConceptEdgeType || v.ParentTagID == v.ChildTagID ||
			(v.Kind != "broader" && v.Kind != "related") || (v.Kind == "related" && v.ChildTagID < v.ParentTagID) {
			return errors.New("invalid concept edge metadata")
		}
		if err := validateUUIDv4(v.ParentTagID); err != nil {
			return err
		}
		return validateUUIDv4(v.ChildTagID)
	case metadataTagRedirect:
		if v.Type != metadataTagRedirectType || v.SourceTagID == v.TargetTagID {
			return errors.New("invalid tag redirect metadata")
		}
		for _, id := range []string{v.SourceTagID, v.TargetTagID, v.MergeID} {
			if err := validateUUIDv4(id); err != nil {
				return err
			}
		}
		if err := validateCanonicalConceptName(v.SourceName); err != nil {
			return err
		}
		return validateMetadataTime("tag redirect merged_at", v.MergedAt)
	case metadataTagMergeAudit:
		if v.Type != metadataTagMergeAuditType || v.SourceTagID == v.TargetTagID || v.SourceRevision < 1 || v.TargetRevision < 1 {
			return errors.New("invalid tag merge audit metadata")
		}
		for _, id := range []string{v.MergeID, v.SourceTagID, v.TargetTagID} {
			if err := validateUUIDv4(id); err != nil {
				return err
			}
		}
		if err := validateMetadataTime("tag merge committed_at", v.CommittedAt); err != nil {
			return err
		}
		if v.ReversedAt != nil {
			if err := validateMetadataTime("tag merge reversed_at", *v.ReversedAt); err != nil {
				return err
			}
			if *v.ReversedAt < v.CommittedAt {
				return errors.New("tag merge reversal precedes commit")
			}
		}
		return validateTagMergePreviewMetadata(v)
	case metadataPassageTag:
		if v.Type != metadataPassageTagType {
			return errors.New("invalid passage tag metadata")
		}
		for _, id := range []string{v.TagID, v.DocumentUID, v.ContentVersionID} {
			if err := validateUUIDv4(id); err != nil {
				return err
			}
		}
		var ref document.PassageRefV1
		if err := json.Unmarshal(v.RefJSON, &ref, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("decoding passage tag reference: %w", err)
		}
		passageID, err := document.PassageIdentityV1(ref)
		if err != nil {
			return err
		}
		if v.PassageID != passageID {
			return errors.New("passage tag identity mismatch")
		}
		canonical, err := json.Marshal(ref)
		if err != nil {
			return err
		}
		if !bytes.Equal(canonical, v.RefJSON) {
			return errors.New("passage tag reference is not canonical JSON")
		}
		return nil
	default:
		return errors.New("unknown concept metadata record")
	}
}

func validateCanonicalConceptName(name string) error {
	normalized, err := NormalizeTagName(name)
	if err != nil {
		return err
	}
	if normalized != name {
		return errors.New("concept name is not canonical NFC")
	}
	return nil
}

func validateTagMergePreviewMetadata(v metadataTagMergeAudit) error {
	var preview TagMergePreview
	if err := json.Unmarshal(v.PreviewJSON, &preview, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid tag merge preview JSON: %w", err)
	}
	if preview.SourceTagID != v.SourceTagID || preview.TargetTagID != v.TargetTagID ||
		preview.SourceRevision != v.SourceRevision || preview.TargetRevision != v.TargetRevision ||
		preview.SourceConceptRev < 1 || preview.TargetConceptRev < 1 ||
		preview.DocumentAssignments < 0 || preview.PassageAssignments < 0 ||
		preview.TargetDocumentAssignments < 0 || preview.TargetPassageAssignments < 0 ||
		len(preview.DocumentNodeIDs) != preview.DocumentAssignments ||
		len(preview.PassageIDs) != preview.PassageAssignments ||
		len(preview.SavedQueryIDs) != len(preview.SavedQueryRevisions) ||
		len(preview.ContentMapIDs) != len(preview.ContentMapRevisions) ||
		len(preview.ContentMapIDs) > maxTagMergeRefs ||
		len(preview.DocumentNodeIDs) > maxTagMergeRefs || len(preview.PassageIDs) > maxTagMergeRefs {
		return errors.New("tag merge preview does not match audited authority")
	}
	if err := validateCanonicalConceptName(preview.SourceName); err != nil {
		return fmt.Errorf("invalid tag merge preview source name: %w", err)
	}
	if err := validateCanonicalConceptName(preview.TargetName); err != nil {
		return fmt.Errorf("invalid tag merge preview target name: %w", err)
	}
	if !validConceptDescription(preview.SourceDescription) {
		return errors.New("invalid tag merge preview source description")
	}
	canonical, err := json.Marshal(preview)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, v.PreviewJSON) {
		return errors.New("tag merge preview is not canonical JSON")
	}
	return nil
}

func validateTagConceptMetadataState(ctx context.Context, tx metadataQuerier) error {
	if err := exportTagConceptMetadata(ctx, tx, func(any) error { return nil }); err != nil {
		return err
	}
	if err := exportPassageTagMetadata(ctx, tx, func(any) error { return nil }); err != nil {
		return err
	}
	if err := validatePassageTagLocalAuthority(ctx, tx); err != nil {
		return err
	}
	var collision bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tag_aliases a JOIN tags t ON t.name=a.alias)`).Scan(&collision); err != nil {
		return err
	}
	if collision {
		return errors.New("tag alias collides with canonical name")
	}
	var mismatchedRedirect bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM tag_redirects r LEFT JOIN tag_merge_audit a
		ON a.merge_id=r.merge_id AND a.source_tag_id=r.source_tag_id
		WHERE a.merge_id IS NULL OR a.reversed_at IS NOT NULL
		UNION ALL
		SELECT 1 FROM tag_merge_audit a LEFT JOIN tag_redirects r
		ON r.merge_id=a.merge_id AND r.source_tag_id=a.source_tag_id
		WHERE a.reversed_at IS NULL AND r.source_tag_id IS NULL
	)`).Scan(&mismatchedRedirect); err != nil {
		return err
	}
	if mismatchedRedirect {
		return errors.New("tag redirect does not match active merge audit")
	}
	rows, err := tx.QueryContext(ctx, `SELECT parent_tag_id,child_tag_id FROM tag_concept_edges WHERE kind='broader' ORDER BY parent_tag_id,child_tag_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	graph := make(map[string][]string)
	for rows.Next() {
		var parent, child string
		if err := rows.Scan(&parent, &child); err != nil {
			return err
		}
		if document.WouldCreateConceptCycle(graph, parent, child) {
			return errors.New("concept hierarchy contains a cycle")
		}
		graph[parent] = append(graph[parent], child)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func validatePassageTagLocalAuthority(ctx context.Context, tx metadataQuerier) error {
	var vaultUID string
	if err := tx.QueryRowContext(ctx, `SELECT vault_uid FROM vault_metadata WHERE singleton=1`).Scan(&vaultUID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT ref_json,document_uid,content_version_id FROM passage_tags ORDER BY passage_id,tag_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		var localDocumentUID, localVersionID string
		if err := rows.Scan(&raw, &localDocumentUID, &localVersionID); err != nil {
			return err
		}
		var ref document.PassageRefV1
		if err := json.Unmarshal(raw, &ref); err != nil {
			return err
		}
		if ref.FederationDomainUID == "" && ref.VaultUID == vaultUID {
			if ref.DocumentUID != localDocumentUID || ref.ContentVersionID != localVersionID {
				return errors.New("passage tag local authority mismatch")
			}
			continue
		}
		var count int
		var mappedDocumentUID, mappedVersionID sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*),MIN(pa.local_document_uid),MIN(pa.local_content_version_id)
			FROM adopted_passage_authorities pa
			JOIN document_identity_aliases da ON da.domain_uid=pa.domain_uid
				AND da.source_vault_uid=pa.source_vault_uid
				AND da.source_document_uid=pa.source_document_uid
				AND da.local_document_uid=pa.local_document_uid
			JOIN content_versions v ON v.version_id=pa.local_content_version_id
			WHERE pa.source_vault_uid=? AND pa.source_document_uid=?
				AND pa.source_content_version_id=? AND pa.source_build_id=?
				AND pa.source_attachment_id=? AND v.blob_hash=?
				AND (?='' OR pa.domain_uid=?)`, ref.VaultUID, ref.DocumentUID,
			ref.ContentVersionID, ref.RenditionBuildID, ref.AttachmentID,
			ref.SourceSHA256, ref.FederationDomainUID, ref.FederationDomainUID).
			Scan(&count, &mappedDocumentUID, &mappedVersionID)
		if err != nil {
			return err
		}
		if count != 1 || mappedDocumentUID.String != localDocumentUID ||
			mappedVersionID.String != localVersionID {
			return errors.New("passage tag local authority mismatch")
		}
	}
	return rows.Err()
}
