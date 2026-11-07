package store

import (
	"context"
	"fmt"

	"go.kenn.io/kit/packstore"
)

type metadataNotePublication struct {
	Type              string `json:"type"`
	Name              string `json:"name" db:"name"`
	Destination       string `json:"destination" db:"destination"`
	PolicyFingerprint string `json:"policy_fingerprint" db:"policy_fingerprint"`
	NamespaceRevision int64  `json:"namespace_revision" db:"namespace_revision"`
	Active            int64  `json:"active" db:"active"`
	LastCompleteAt    string `json:"last_complete_at" db:"last_complete_at"`
	LastAcceptedAt    string `json:"last_accepted_at" db:"last_accepted_at"`
}

type metadataPublishedNoteSource struct {
	Type             string `json:"type"`
	Publication      string `json:"publication" db:"publication_name"`
	SourceRef        string `json:"source_ref" db:"source_ref"`
	NodeID           int64  `json:"node_id" db:"node_id"`
	ContentVersionID string `json:"content_version_id" db:"content_version_id"`
	Hash             string `json:"blob_hash" db:"blob_hash"`
	Size             int64  `json:"size" db:"size"`
	SourceRevision   int64  `json:"source_revision" db:"source_revision"`
	Presence         string `json:"presence" db:"presence"`
	Availability     string `json:"availability" db:"availability"`
	AcceptedAt       string `json:"accepted_at" db:"accepted_at"`
}

var notePublicationMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataNotePublication]{record: metadataNotePublication{Type: "note_publication"},
		table: "note_publications", suffix: "ORDER BY name", validate: validateNotePublicationRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPublishedNoteSource]{record: metadataPublishedNoteSource{Type: "published_note_source"},
		table: "published_note_sources", suffix: "ORDER BY publication_name,source_ref", validate: validatePublishedNoteSourceRecord, checkExport: true}),
}

func validateNotePublicationRecord(v metadataNotePublication) error {
	if err := ValidatePushName(v.Name); err != nil {
		return err
	}
	if err := ValidatePushDestination(v.Destination); err != nil {
		return err
	}
	if err := validateNotePolicyFingerprint(v.PolicyFingerprint); err != nil {
		return err
	}
	if v.NamespaceRevision <= 0 || (v.Active != 0 && v.Active != 1) {
		return fmt.Errorf("invalid note publication state")
	}
	for _, at := range []string{v.LastCompleteAt, v.LastAcceptedAt} {
		if at != "" {
			if err := validateMetadataTime("note publication time", at); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateNotePolicyFingerprint(value string) error {
	// Policy is a stable SHA256 digest of the fixed recipe and explicit scope.
	_, err := packstore.ParseHash(value)
	return err
}

func validatePublishedNoteSourceRecord(v metadataPublishedNoteSource) error {
	// Vault/revision belong to the surrounding namespace, checked relationally.
	if err := ValidatePublishedNoteSource(NotePublicationSource{Identity: PublishedNoteIdentity{
		VaultID: "00000000-0000-4000-8000-000000000000", Publication: v.Publication, SourceRef: v.SourceRef,
		NodeID: v.NodeID, ContentVersionID: v.ContentVersionID, Hash: v.Hash, Size: v.Size, SourceRevision: v.SourceRevision, NamespaceRevision: 1,
	}, Presence: v.Presence, Availability: v.Availability}); err != nil {
		return err
	}
	return validateMetadataTime("published note accepted_at", v.AcceptedAt)
}

func validateNotePublicationMetadata(ctx context.Context, q metadataQuerier) error {
	for _, codec := range notePublicationMetadataTables {
		if err := codec.validateRows(ctx, q); err != nil {
			return err
		}
	}
	var invalid int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM published_note_sources s
		LEFT JOIN note_publications p ON p.name=s.publication_name
		LEFT JOIN content_versions v ON v.version_id=s.content_version_id
		LEFT JOIN nodes n ON n.id=s.node_id
		WHERE p.name IS NULL
        OR EXISTS(SELECT 1 FROM push_sources ps WHERE ps.push_name=s.publication_name AND ps.source_ref=s.source_ref AND ps.node_id<>s.node_id)
        OR EXISTS(SELECT 1 FROM push_sources ps WHERE ps.node_id=s.node_id AND (ps.push_name<>s.publication_name OR ps.source_ref<>s.source_ref))
        OR (s.availability='retained' AND
		(n.id IS NULL OR v.version_id IS NULL OR v.node_id<>s.node_id OR v.blob_hash<>s.blob_hash OR v.size<>s.size))`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("published note authority has inconsistent retained bindings")
	}
	return nil
}
