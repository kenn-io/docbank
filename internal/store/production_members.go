package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"reflect"
	"slices"

	"go.kenn.io/docbank/document"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
)

// ProductionTextMapAuthority binds one canonical aligned map to the exact
// retained source, optional derived PDF rendition, and stored page frames.
// Derived rendition IDs are all empty for a native PDF and all set otherwise.
type ProductionTextMapAuthority struct {
	SourceNodeID          int64
	Source                document.PageSource
	PDFSHA256             string
	PDFSize               int64
	RenditionAttachmentID string
	RenditionBuildID      string
	RenditionArtifactID   string
	Map                   redaction.TextMap
}

// RetainProductionTextMap installs immutable map authority after resolving
// every source, rendition, page-document, and page-frame reference inside one
// writer transaction. An exact retry is idempotent.
func (s *Store) RetainProductionTextMap(ctx context.Context, authority ProductionTextMapAuthority) (string, string, error) {
	authority.Map = redaction.NormalizeTextMap(authority.Map)
	canonicalMap, mapSHA256, err := redaction.CanonicalTextMap(authority.Map)
	if err != nil {
		return "", "", errors.Join(ErrInvalidProduction, err)
	}
	authority.Map.SHA256 = mapSHA256
	pageInventorySHA256, err := productionPageInventorySHA256(authority.Map.Pages)
	if err != nil || validateProductionTextMapAuthority(authority) != nil {
		return "", "", errors.Join(ErrInvalidProduction, err)
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if existing, found, loadErr := loadProductionTextMapTx(ctx, tx, mapSHA256); loadErr != nil {
			return loadErr
		} else if found {
			if existing.SourceNodeID != authority.SourceNodeID || existing.Source != authority.Source ||
				existing.PDFSHA256 != authority.PDFSHA256 || existing.PDFSize != authority.PDFSize ||
				existing.RenditionAttachmentID != authority.RenditionAttachmentID ||
				existing.RenditionBuildID != authority.RenditionBuildID ||
				existing.RenditionArtifactID != authority.RenditionArtifactID ||
				!reflect.DeepEqual(existing.Map, authority.Map) {
				return ErrInvalidProduction
			}
			return nil
		}
		if err := validateProductionTextMapRelationsTx(ctx, tx, authority, pageInventorySHA256); err != nil {
			return err
		}
		var pageDocumentSHA256 string
		if err := tx.QueryRowContext(ctx, `SELECT checksum FROM page_documents WHERE version_id=?`, authority.Source.VersionID).Scan(&pageDocumentSHA256); err != nil {
			return errors.Join(ErrInvalidProduction, err)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO production_text_maps(
			map_sha256,source_node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
			page_document_sha256,evidence_sha256,page_inventory_sha256,rendition_attachment_id,
			rendition_build_id,rendition_artifact_id,canonical_json,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, mapSHA256, authority.SourceNodeID, authority.Source.VersionID,
			authority.Source.SHA256, authority.Source.Size, authority.PDFSHA256, authority.PDFSize,
			pageDocumentSHA256, authority.Map.EvidenceSHA256, pageInventorySHA256,
			nullableProductionText(authority.RenditionAttachmentID), nullableProductionText(authority.RenditionBuildID),
			nullableProductionText(authority.RenditionArtifactID), canonicalMap, nowRFC3339())
		if err != nil {
			return errors.Join(ErrInvalidProduction, err)
		}
		for _, page := range authority.Map.Pages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO production_text_map_frames(
				map_sha256,version_id,page,frame_sha256,width,height) VALUES(?,?,?,?,?,?)`,
				mapSHA256, authority.Source.VersionID, page.Number, page.FrameSHA256, page.Width, page.Height); err != nil {
				return errors.Join(ErrInvalidProduction, err)
			}
		}
		return nil
	})
	return mapSHA256, pageInventorySHA256, err
}

func productionPageInventorySHA256(pages []redaction.Page) (string, error) {
	raw, err := canonical.Marshal(pages)
	if err != nil {
		return "", err
	}
	return productionSHA256(raw), nil
}

func validateProductionTextMapAuthority(authority ProductionTextMapAuthority) error {
	if authority.SourceNodeID < 1 || authority.Source.Validate() != nil || redaction.ValidateMap(authority.Map) != nil ||
		authority.Map.PDFSHA256 != authority.PDFSHA256 || authority.PDFSize < 1 {
		return ErrInvalidProduction
	}
	hasAttachment := authority.RenditionAttachmentID != ""
	if hasAttachment != (authority.RenditionBuildID != "") || hasAttachment != (authority.RenditionArtifactID != "") {
		return ErrInvalidProduction
	}
	if !hasAttachment && (authority.PDFSHA256 != authority.Source.SHA256 || authority.PDFSize != authority.Source.Size) {
		return ErrInvalidProduction
	}
	return nil
}

func validateProductionTextMapRelationsTx(
	ctx context.Context, tx *sql.Tx, authority ProductionTextMapAuthority, pageInventorySHA256 string,
) error {
	var sourceNodeID, sourceSize int64
	var sourceSHA256 string
	if err := tx.QueryRowContext(ctx, `SELECT node_id,blob_hash,size FROM content_versions WHERE version_id=?`, authority.Source.VersionID).
		Scan(&sourceNodeID, &sourceSHA256, &sourceSize); err != nil || sourceNodeID != authority.SourceNodeID ||
		sourceSHA256 != authority.Source.SHA256 || sourceSize != authority.Source.Size {
		return ErrInvalidProduction
	}
	var pdfSize int64
	if err := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, authority.PDFSHA256).Scan(&pdfSize); err != nil || pdfSize != authority.PDFSize {
		return ErrInvalidProduction
	}
	doc, err := loadPageDocument(ctx, tx, authority.Source.VersionID)
	if err != nil || doc.Source != authority.Source || len(doc.Frames) != len(authority.Map.Pages) {
		return ErrInvalidProduction
	}
	for index, frame := range doc.Frames {
		_, digest, frameErr := document.MarshalPageFrameV1(frame)
		page := authority.Map.Pages[index]
		if frameErr != nil || page.Number != index+1 || page.FrameSHA256 != digest || page.Width != frame.Width || page.Height != frame.Height {
			return ErrInvalidProduction
		}
	}
	actualInventory, err := productionPageInventorySHA256(authority.Map.Pages)
	if err != nil || actualInventory != pageInventorySHA256 {
		return ErrInvalidProduction
	}
	if authority.RenditionAttachmentID == "" {
		return nil
	}
	var versionID, buildID, role, blobHash, buildSourceSHA256, evidenceSHA256 string
	var artifactSize int64
	err = tx.QueryRowContext(ctx, `SELECT a.content_version_id,a.build_id,r.role,r.blob_hash,r.size,b.source_sha256,b.evidence_checksum
		FROM rendition_attachments a
		JOIN rendition_builds b ON b.build_id=a.build_id
		JOIN rendition_artifacts r ON r.build_id=b.build_id AND r.artifact_id=?
		WHERE a.attachment_id=?`, authority.RenditionArtifactID, authority.RenditionAttachmentID).
		Scan(&versionID, &buildID, &role, &blobHash, &artifactSize, &buildSourceSHA256, &evidenceSHA256)
	if err != nil || versionID != authority.Source.VersionID || buildID != authority.RenditionBuildID ||
		role != string(document.EvidenceArtifactPDF) || blobHash != authority.PDFSHA256 || artifactSize != authority.PDFSize ||
		buildSourceSHA256 != authority.Source.SHA256 || evidenceSHA256 != authority.Map.EvidenceSHA256 {
		return ErrInvalidProduction
	}
	return nil
}

func loadProductionTextMapTx(ctx context.Context, tx *sql.Tx, mapSHA256 string) (ProductionTextMapAuthority, bool, error) {
	var authority ProductionTextMapAuthority
	var pageDocumentSHA256, evidenceSHA256, pageInventorySHA256 string
	var attachmentID, buildID, artifactID sql.NullString
	var canonicalMap []byte
	err := tx.QueryRowContext(ctx, `SELECT source_node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
		page_document_sha256,evidence_sha256,page_inventory_sha256,rendition_attachment_id,rendition_build_id,
		rendition_artifact_id,canonical_json FROM production_text_maps WHERE map_sha256=?`, mapSHA256).Scan(
		&authority.SourceNodeID, &authority.Source.VersionID, &authority.Source.SHA256, &authority.Source.Size,
		&authority.PDFSHA256, &authority.PDFSize, &pageDocumentSHA256, &evidenceSHA256, &pageInventorySHA256,
		&attachmentID, &buildID, &artifactID, &canonicalMap)
	if errors.Is(err, sql.ErrNoRows) {
		return authority, false, nil
	}
	if err != nil {
		return authority, false, err
	}
	authority.RenditionAttachmentID, authority.RenditionBuildID, authority.RenditionArtifactID = attachmentID.String, buildID.String, artifactID.String
	textMap, actualMapSHA256, err := redaction.DecodeTextMap(canonicalMap)
	if err != nil || actualMapSHA256 != mapSHA256 || textMap.EvidenceSHA256 != evidenceSHA256 || textMap.PDFSHA256 != authority.PDFSHA256 {
		return ProductionTextMapAuthority{}, false, ErrInvalidProduction
	}
	authority.Map = textMap
	actualInventory, err := productionPageInventorySHA256(textMap.Pages)
	if err != nil || actualInventory != pageInventorySHA256 || validateProductionTextMapAuthority(authority) != nil {
		return ProductionTextMapAuthority{}, false, ErrInvalidProduction
	}
	var storedPageDocumentSHA256 string
	if err := tx.QueryRowContext(ctx, `SELECT checksum FROM page_documents WHERE version_id=?`, authority.Source.VersionID).Scan(&storedPageDocumentSHA256); err != nil || storedPageDocumentSHA256 != pageDocumentSHA256 {
		return ProductionTextMapAuthority{}, false, ErrInvalidProduction
	}
	rows, err := tx.QueryContext(ctx, `SELECT page,frame_sha256,width,height FROM production_text_map_frames
		WHERE map_sha256=? AND version_id=? ORDER BY page`, mapSHA256, authority.Source.VersionID)
	if err != nil {
		return ProductionTextMapAuthority{}, false, err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var page int
		var frameSHA256 string
		var width, height int64
		if err := rows.Scan(&page, &frameSHA256, &width, &height); err != nil || count >= len(textMap.Pages) {
			return ProductionTextMapAuthority{}, false, ErrInvalidProduction
		}
		mapped := textMap.Pages[count]
		if page != mapped.Number || frameSHA256 != mapped.FrameSHA256 || width != mapped.Width || height != mapped.Height {
			return ProductionTextMapAuthority{}, false, ErrInvalidProduction
		}
		count++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || count != len(textMap.Pages) {
		return ProductionTextMapAuthority{}, false, ErrInvalidProduction
	}
	if err := validateProductionTextMapRelationsTx(ctx, tx, authority, pageInventorySHA256); err != nil {
		return ProductionTextMapAuthority{}, false, err
	}
	encoded, _, err := redaction.CanonicalTextMap(authority.Map)
	if err != nil || !bytes.Equal(encoded, canonicalMap) {
		return ProductionTextMapAuthority{}, false, ErrInvalidProduction
	}
	return authority, true, nil
}

type productionListCursorV1 struct {
	Kind     string `json:"kind"`
	SetID    string `json:"set_id"`
	Revision int64  `json:"revision"`
	Ordinal  int64  `json:"ordinal,omitzero"`
	MemberID string `json:"member_id,omitzero"`
	ID       string `json:"id,omitzero"`
}

func (s *Store) ProductionMembers(ctx context.Context, setID string, revision int64, cursor string, limit int) ([]redaction.Member, string, error) {
	if validateUUIDv4(setID) != nil || revision < 1 || limit < 1 || limit > redaction.MaxProductionPage {
		return nil, "", ErrInvalidProduction
	}
	position, err := decodeProductionListCursor(cursor, "members", setID, revision)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision)); err != nil {
		return nil, "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
		map_sha256,page_inventory_sha256,family_context_json,canonical_json
		FROM production_members WHERE set_id=? AND revision=? AND
		(ordinal>? OR (ordinal=? AND member_id>?)) ORDER BY ordinal,member_id LIMIT ?`,
		setID, revision, position.Ordinal, position.Ordinal, position.MemberID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	items := make([]redaction.Member, 0, limit+1)
	for rows.Next() {
		member, err := scanProductionMember(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, member)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, "", err
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		last := items[limit-1]
		next, err = encodeProductionListCursor(productionListCursorV1{
			Kind: "members", SetID: setID, Revision: revision, Ordinal: last.Ordinal, MemberID: last.ID,
		})
		if err != nil {
			return nil, "", err
		}
		items = items[:limit]
	}
	if items == nil {
		items = []redaction.Member{}
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return items, next, nil
}

func scanProductionMember(row interface{ Scan(dest ...any) error }) (redaction.Member, error) {
	var memberID, vaultID, versionID, sourceSHA256, pdfSHA256, mapSHA256, inventorySHA256 string
	var ordinal, nodeID, sourceSize, pdfSize int64
	var familyJSON, memberJSON []byte
	if err := row.Scan(&memberID, &ordinal, &vaultID, &nodeID, &versionID, &sourceSHA256, &sourceSize, &pdfSHA256, &pdfSize,
		&mapSHA256, &inventorySHA256, &familyJSON, &memberJSON); err != nil {
		return redaction.Member{}, err
	}
	member, err := canonical.Decode[redaction.Member](memberJSON)
	if err != nil || redaction.ValidateMember(member) != nil {
		return redaction.Member{}, ErrInvalidProduction
	}
	family, err := canonical.Decode[redaction.FamilyContext](familyJSON)
	if err != nil || family != member.Family || member.ID != memberID || member.Ordinal != ordinal || member.VaultID != vaultID || member.NodeID != nodeID ||
		member.SourceVersionID != versionID || member.SourceSHA256 != sourceSHA256 || member.SourceSize != sourceSize ||
		member.PDFSHA256 != pdfSHA256 || member.PDFSize != pdfSize ||
		member.MapSHA256 != mapSHA256 || member.PageInventorySHA256 != inventorySHA256 {
		return redaction.Member{}, ErrInvalidProduction
	}
	return member, nil
}

func (s *Store) validateProductionMemberAuthorityTx(ctx context.Context, tx *sql.Tx, member redaction.Member) error {
	if redaction.ValidateMember(member) != nil || member.VaultID != s.vaultID {
		return ErrInvalidProduction
	}
	var nodeID, sourceSize int64
	var sourceSHA256 string
	if err := tx.QueryRowContext(ctx, `SELECT node_id,blob_hash,size FROM content_versions WHERE version_id=?`, member.SourceVersionID).
		Scan(&nodeID, &sourceSHA256, &sourceSize); err != nil || nodeID != member.NodeID || sourceSHA256 != member.SourceSHA256 || sourceSize != member.SourceSize {
		return ErrInvalidProduction
	}
	var pdfSize int64
	if err := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, member.PDFSHA256).Scan(&pdfSize); err != nil || pdfSize != member.PDFSize {
		return ErrInvalidProduction
	}
	var versionID, mapSourceSHA256, pdfSHA256, inventorySHA256 string
	var mapNodeID, mapSourceSize, mapPDFSize int64
	if err := tx.QueryRowContext(ctx, `SELECT source_node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,page_inventory_sha256
		FROM production_text_maps WHERE map_sha256=?`, member.MapSHA256).
		Scan(&mapNodeID, &versionID, &mapSourceSHA256, &mapSourceSize, &pdfSHA256, &mapPDFSize, &inventorySHA256); err != nil ||
		mapNodeID != member.NodeID || versionID != member.SourceVersionID || mapSourceSHA256 != member.SourceSHA256 ||
		mapSourceSize != member.SourceSize || pdfSHA256 != member.PDFSHA256 || mapPDFSize != member.PDFSize ||
		inventorySHA256 != member.PageInventorySHA256 {
		return ErrInvalidProduction
	}
	if member.Family.Kind == "email_attachment" {
		var parentVersionID string
		var childVersionID sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT p.parent_version_id,r.child_version_id
			FROM email_document_publications p JOIN email_document_relations r ON r.operation_id=p.operation_id
			WHERE r.operation_id=? AND r.occurrence_order=?`, member.Family.RelationOperationID, member.Family.RelationOrder).
			Scan(&parentVersionID, &childVersionID); err != nil || parentVersionID != member.Family.RootVersionID ||
			!childVersionID.Valid || childVersionID.String != member.SourceVersionID {
			return ErrInvalidProduction
		}
	}
	return nil
}

func (s *Store) upsertProductionMemberTx(ctx context.Context, tx *sql.Tx, setID string, revision int64, member redaction.Member) error {
	if err := s.validateProductionMemberAuthorityTx(ctx, tx, member); err != nil {
		return err
	}
	memberJSON, err := canonical.Marshal(member)
	if err != nil {
		return err
	}
	familyJSON, err := canonical.Marshal(member.Family)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO production_members(
		set_id,revision,member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,map_sha256,page_inventory_sha256,family_context_json,canonical_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(set_id,revision,member_id) DO UPDATE SET
		ordinal=excluded.ordinal,vault_id=excluded.vault_id,node_id=excluded.node_id,version_id=excluded.version_id,source_sha256=excluded.source_sha256,
		source_size=excluded.source_size,pdf_sha256=excluded.pdf_sha256,pdf_size=excluded.pdf_size,map_sha256=excluded.map_sha256,page_inventory_sha256=excluded.page_inventory_sha256,
		family_context_json=excluded.family_context_json,canonical_json=excluded.canonical_json`,
		setID, revision, member.ID, member.Ordinal, member.VaultID, member.NodeID, member.SourceVersionID, member.SourceSHA256,
		member.SourceSize, member.PDFSHA256, member.PDFSize, member.MapSHA256, member.PageInventorySHA256, familyJSON, memberJSON)
	if err != nil {
		return errors.Join(ErrInvalidProduction, err)
	}
	return nil
}

func productionMemberHash(members []redaction.Member) (string, error) {
	members = slices.Clone(members)
	if members == nil {
		members = []redaction.Member{}
	}
	slices.SortFunc(members, func(left, right redaction.Member) int {
		if left.Ordinal < right.Ordinal {
			return -1
		}
		if left.Ordinal > right.Ordinal {
			return 1
		}
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	seenIDs := make(map[string]struct{}, len(members))
	prepared := make([]documentproduction.PreparedMember, len(members))
	for index, member := range members {
		if redaction.ValidateMember(member) != nil || member.Ordinal != int64(index+1) {
			return "", ErrInvalidProduction
		}
		if _, exists := seenIDs[member.ID]; exists {
			return "", ErrInvalidProduction
		}
		seenIDs[member.ID] = struct{}{}
		prepared[index].Member = member
	}
	_, digest, err := documentproduction.PreparedMemberHash(prepared)
	return digest, err
}

func productionMemberHashTx(ctx context.Context, tx *sql.Tx, setID string, revision int64) (string, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT member_id,ordinal,vault_id,node_id,version_id,source_sha256,source_size,pdf_sha256,pdf_size,
		map_sha256,page_inventory_sha256,family_context_json,canonical_json FROM production_members
		WHERE set_id=? AND revision=? ORDER BY ordinal,member_id`, setID, revision)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rows.Close() }()
	members := make([]redaction.Member, 0)
	count := 0
	for rows.Next() {
		member, err := scanProductionMember(rows)
		if err != nil || member.Ordinal != int64(count+1) || count >= redaction.MaxProductionMembers {
			return "", 0, ErrInvalidProduction
		}
		members = append(members, member)
		count++
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	digest, err := productionMemberHash(members)
	return digest, count, err
}

func encodeProductionListCursor(value productionListCursorV1) (string, error) {
	raw, err := canonical.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeProductionListCursor(raw, kind, setID string, revision int64) (productionListCursorV1, error) {
	if raw == "" {
		return productionListCursorV1{Kind: kind, SetID: setID, Revision: revision}, nil
	}
	if len(raw) > 2048 {
		return productionListCursorV1{}, ErrInvalidProduction
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return productionListCursorV1{}, ErrInvalidProduction
	}
	value, err := canonical.Decode[productionListCursorV1](decoded)
	if err != nil || value.Kind != kind || value.SetID != setID || value.Revision != revision {
		return productionListCursorV1{}, ErrInvalidProduction
	}
	if kind == "members" && (value.Ordinal < 1 || validateUUIDv4(value.MemberID) != nil || value.ID != "") ||
		kind == "decisions" && (validateUUIDv4(value.MemberID) != nil || validateUUIDv4(value.ID) != nil || value.Ordinal != 0) ||
		kind == "sets" && (validateUUIDv4(value.ID) != nil || value.Ordinal != 0 || value.MemberID != "") {
		return productionListCursorV1{}, ErrInvalidProduction
	}
	return value, nil
}
