package store

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	metadataPersonType              = "person"
	metadataPersonIdentityType      = "person_identity"
	metadataPersonExternalType      = "person_external_identity"
	metadataPersonExternalAliasType = "person_external_uid_alias"
	metadataPersonAliasType         = "person_alias"
	metadataPersonMergeType         = "person_merge"
	metadataPersonSplitType         = "person_split"
	metadataCustodianAssignmentType = "custodian_assignment"
	metadataPersonAssertionType     = "person_document_assertion"
	metadataPersonCandidateType     = "person_match_candidate"
)

type metadataPerson struct {
	Type              string `json:"type"`
	PersonID          string `json:"person_id" db:"person_id"`
	DisplayName       string `json:"display_name" db:"display_name"`
	DisplayNameFolded string `json:"display_name_folded" db:"display_name_folded"`
	Origin            string `json:"origin" db:"origin"`
	State             string `json:"state" db:"state"`
	Revision          int64  `json:"revision" db:"revision"`
	CreatedAt         string `json:"created_at" db:"created_at"`
	UpdatedAt         string `json:"updated_at" db:"updated_at"`
}

type metadataPersonIdentity struct {
	Type            string `json:"type"`
	IdentityID      string `json:"identity_id" db:"identity_id"`
	PersonID        string `json:"person_id" db:"person_id"`
	Kind            string `json:"kind" db:"kind"`
	ValueNormalized string `json:"value_normalized" db:"value_normalized"`
	ValueDisplay    string `json:"value_display" db:"value_display"`
	ScopeKind       string `json:"scope_kind" db:"scope_kind"`
	ScopeValue      string `json:"scope_value" db:"scope_value"`
	Normalization   string `json:"normalization" db:"normalization"`
	Origin          string `json:"origin" db:"origin"`
	EvidenceKind    string `json:"evidence_kind" db:"evidence_kind"`
	EvidenceID      string `json:"evidence_id" db:"evidence_id"`
	Confidence      string `json:"confidence" db:"confidence"`
	RecordedAt      string `json:"recorded_at" db:"recorded_at"`
}

type metadataPersonExternalIdentity struct {
	Type                string `json:"type"`
	PersonID            string `json:"person_id" db:"person_id"`
	System              string `json:"system" db:"system"`
	ArchiveID           string `json:"archive_id" db:"archive_id"`
	UID                 string `json:"uid" db:"uid"`
	UIDKind             string `json:"uid_kind" db:"uid_kind"`
	UIDState            string `json:"uid_state" db:"uid_state"`
	LastSeenRevision    *int64 `json:"last_seen_revision" db:"last_seen_revision"`
	DisplayNameSnapshot string `json:"display_name_snapshot" db:"display_name_snapshot"`
	LinkedAt            string `json:"linked_at" db:"linked_at"`
	UpdatedAt           string `json:"updated_at" db:"updated_at"`
}

type metadataPersonExternalUIDAlias struct {
	Type         string `json:"type"`
	System       string `json:"system" db:"system"`
	ArchiveID    string `json:"archive_id" db:"archive_id"`
	RetiredUID   string `json:"retired_uid" db:"retired_uid"`
	SurvivingUID string `json:"surviving_uid" db:"surviving_uid"`
	ObservedAt   string `json:"observed_at" db:"observed_at"`
}

type metadataPersonAlias struct {
	Type              string  `json:"type"`
	RetiredPersonID   string  `json:"retired_person_id" db:"retired_person_id"`
	SurvivingPersonID *string `json:"surviving_person_id" db:"surviving_person_id"`
	Reason            string  `json:"reason" db:"reason"`
	RetiredAt         string  `json:"retired_at" db:"retired_at"`
}

type metadataPersonMerge struct {
	Type                   string `json:"type"`
	MergeID                string `json:"merge_id" db:"merge_id"`
	OperationID            string `json:"operation_id" db:"operation_id"`
	RequestSHA256          string `json:"request_sha256" db:"request_sha256"`
	SurvivorPersonID       string `json:"survivor_person_id" db:"survivor_person_id"`
	AbsorbedPersonID       string `json:"absorbed_person_id" db:"absorbed_person_id"`
	AbsorbedDisplayName    string `json:"absorbed_display_name" db:"absorbed_display_name"`
	MovedJSON              []byte `json:"moved_json" db:"moved_json"`
	SurvivorRevisionBefore int64  `json:"survivor_revision_before" db:"survivor_revision_before"`
	SurvivorRevisionAfter  int64  `json:"survivor_revision_after" db:"survivor_revision_after"`
	CreatedAt              string `json:"created_at" db:"created_at"`
}

type metadataPersonSplit struct {
	Type          string `json:"type"`
	OperationID   string `json:"operation_id" db:"operation_id"`
	RequestSHA256 string `json:"request_sha256" db:"request_sha256"`
	ReceiptJSON   []byte `json:"receipt_json" db:"receipt_json"`
	CreatedAt     string `json:"created_at" db:"created_at"`
}

type metadataCustodianAssignment struct {
	Type             string  `json:"type"`
	AssignmentID     string  `json:"assignment_id" db:"assignment_id"`
	ScopeKind        string  `json:"scope_kind" db:"scope_kind"`
	IngestID         *string `json:"ingest_id" db:"ingest_id"`
	PackageID        *string `json:"package_id" db:"package_id"`
	PackageRecordID  *string `json:"package_record_id" db:"package_record_id"`
	NodeID           *int64  `json:"node_id" db:"node_id"`
	ContentVersionID *string `json:"content_version_id" db:"content_version_id"`
	PersonID         *string `json:"person_id" db:"person_id"`
	RawLabel         string  `json:"raw_label" db:"raw_label"`
	RawLabelFolded   string  `json:"raw_label_folded" db:"raw_label_folded"`
	Rank             string  `json:"rank" db:"rank"`
	Basis            string  `json:"basis" db:"basis"`
	SourceRef        string  `json:"source_ref" db:"source_ref"`
	Revision         int64   `json:"revision" db:"revision"`
	RecordedAt       string  `json:"recorded_at" db:"recorded_at"`
	RetiredAt        *string `json:"retired_at" db:"retired_at"`
}

type metadataPersonDocumentAssertion struct {
	Type             string `json:"type"`
	AssertionID      string `json:"assertion_id" db:"assertion_id"`
	ContentVersionID string `json:"content_version_id" db:"content_version_id"`
	PersonID         string `json:"person_id" db:"person_id"`
	Role             string `json:"role" db:"role"`
	Action           string `json:"action" db:"action"`
	Note             string `json:"note" db:"note"`
	RecordedAt       string `json:"recorded_at" db:"recorded_at"`
	Revision         int64  `json:"revision" db:"revision"`
}

type metadataPersonMatchCandidate struct {
	Type              string  `json:"type"`
	CandidateID       string  `json:"candidate_id" db:"candidate_id"`
	ActorKey          string  `json:"actor_key" db:"actor_key"`
	DisplayName       string  `json:"display_name" db:"display_name"`
	SuggestedPersonID *string `json:"suggested_person_id" db:"suggested_person_id"`
	Reason            string  `json:"reason" db:"reason"`
	EvidenceJSON      []byte  `json:"evidence_json" db:"evidence_json"`
	EvidenceSHA256    string  `json:"evidence_sha256" db:"evidence_sha256"`
	OccurrenceCount   int64   `json:"occurrence_count" db:"occurrence_count"`
	Revision          int64   `json:"revision" db:"revision"`
	State             string  `json:"state" db:"state"`
	DecidedPersonID   *string `json:"decided_person_id" db:"decided_person_id"`
	CreatedAt         string  `json:"created_at" db:"created_at"`
	DecidedAt         *string `json:"decided_at" db:"decided_at"`
}

// personMetadataTables exports the person records in dependency order.
var personMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataPerson]{record: metadataPerson{Type: metadataPersonType}, table: "persons",
		suffix: "ORDER BY person_id", validate: validateMetadataPerson, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonIdentity]{record: metadataPersonIdentity{Type: metadataPersonIdentityType}, table: "person_identities",
		suffix: "ORDER BY identity_id", validate: validateMetadataPersonIdentity, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonExternalIdentity]{record: metadataPersonExternalIdentity{Type: metadataPersonExternalType}, table: "person_external_identities",
		suffix: "ORDER BY system,archive_id,uid", validate: validateMetadataPersonExternalIdentity, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonExternalUIDAlias]{record: metadataPersonExternalUIDAlias{Type: metadataPersonExternalAliasType}, table: "person_external_uid_aliases",
		suffix: "ORDER BY system,archive_id,retired_uid", validate: validateMetadataPersonExternalUIDAlias, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonAlias]{record: metadataPersonAlias{Type: metadataPersonAliasType}, table: "person_aliases",
		suffix: "ORDER BY retired_person_id", validate: validateMetadataPersonAlias, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonMerge]{record: metadataPersonMerge{Type: metadataPersonMergeType}, table: "person_merges",
		suffix: "ORDER BY merge_id", validate: validateMetadataPersonMerge, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonSplit]{record: metadataPersonSplit{Type: metadataPersonSplitType}, table: "person_splits",
		suffix: "ORDER BY operation_id", validate: validateMetadataPersonSplit, checkExport: true}),
	newMetadataTable(metadataTable[metadataCustodianAssignment]{record: metadataCustodianAssignment{Type: metadataCustodianAssignmentType}, table: "custodian_assignments",
		suffix: "ORDER BY assignment_id", validate: validateMetadataCustodianAssignment, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonDocumentAssertion]{record: metadataPersonDocumentAssertion{Type: metadataPersonAssertionType}, table: "person_document_assertions",
		suffix: "ORDER BY assertion_id", validate: validateMetadataPersonAssertion, checkExport: true}),
	newMetadataTable(metadataTable[metadataPersonMatchCandidate]{record: metadataPersonMatchCandidate{Type: metadataPersonCandidateType}, table: "person_match_candidates",
		suffix: "ORDER BY candidate_id", validate: validateMetadataPersonCandidate, checkExport: true}),
}

func validateMetadataPerson(r metadataPerson) error {
	if r.Type != metadataPersonType || validateUUIDv4(r.PersonID) != nil || r.Revision < 1 ||
		!validPersonName(r.DisplayName) || r.DisplayNameFolded != document.FoldPersonName(r.DisplayName) ||
		!validPersonOrigin(r.Origin) ||
		!slices.Contains([]string{"provisional", "curated", "retired"}, r.State) {
		return errors.New("invalid person metadata")
	}
	if err := validateMetadataTime("person created_at", r.CreatedAt); err != nil {
		return err
	}
	return validateMetadataTime("person updated_at", r.UpdatedAt)
}

func validateMetadataPersonIdentity(r metadataPersonIdentity) error {
	if r.Type != metadataPersonIdentityType || validateUUIDv4(r.IdentityID) != nil || validateUUIDv4(r.PersonID) != nil ||
		!validPersonOrigin(r.Origin) ||
		!slices.Contains(document.PersonEvidenceKinds(), document.PersonEvidenceKind(r.EvidenceKind)) ||
		r.EvidenceID == "" || len(r.EvidenceID) > document.MaxPersonEvidenceIDBytes ||
		!validPersonConfidence(r.Confidence) {
		return errors.New("invalid person identity metadata")
	}
	normalized, err := document.NormalizeScopedPersonIdentity(document.PersonIdentityKind(r.Kind), r.ValueDisplay, r.ScopeKind, r.ScopeValue)
	if err != nil || normalized.ValueNormalized != r.ValueNormalized || normalized.Normalization != r.Normalization {
		return errors.New("invalid person identity normalization")
	}
	return validateMetadataTime("person identity recorded_at", r.RecordedAt)
}

func validateMetadataPersonExternalIdentity(r metadataPersonExternalIdentity) error {
	if r.Type != metadataPersonExternalType || validateUUIDv4(r.PersonID) != nil ||
		!validExternalTuple(r.System, r.ArchiveID, r.UID) || !validExternalIdentityClassification(r.UIDKind, r.UIDState) ||
		!validExternalIdentityDetails(r.DisplayNameSnapshot, r.LastSeenRevision) {
		return errors.New("invalid person external identity metadata")
	}
	if err := validateMetadataTime("person external identity linked_at", r.LinkedAt); err != nil {
		return err
	}
	return validateMetadataTime("person external identity updated_at", r.UpdatedAt)
}

func validateMetadataPersonExternalUIDAlias(r metadataPersonExternalUIDAlias) error {
	if r.Type != metadataPersonExternalAliasType || r.RetiredUID == r.SurvivingUID ||
		!validExternalTuple(r.System, r.ArchiveID, r.RetiredUID) ||
		!validExternalTuple(r.System, r.ArchiveID, r.SurvivingUID) {
		return errors.New("invalid person external UID alias metadata")
	}
	return validateMetadataTime("person external UID alias observed_at", r.ObservedAt)
}

func validateMetadataPersonAlias(r metadataPersonAlias) error {
	if r.Type != metadataPersonAliasType || validateUUIDv4(r.RetiredPersonID) != nil {
		return errors.New("invalid person alias metadata")
	}
	if r.SurvivingPersonID == nil {
		if r.Reason != "deleted" {
			return errors.New("invalid deleted person alias metadata")
		}
	} else if validateUUIDv4(*r.SurvivingPersonID) != nil || *r.SurvivingPersonID == r.RetiredPersonID || r.Reason != "merged" {
		return errors.New("invalid merged person alias metadata")
	}
	return validateMetadataTime("person alias retired_at", r.RetiredAt)
}

func validateMetadataPersonMerge(r metadataPersonMerge) error {
	if r.Type != metadataPersonMergeType || validateUUIDv4(r.MergeID) != nil || validateUUIDv4(r.OperationID) != nil ||
		validateCatalogSHA256(r.RequestSHA256, "person merge request digest") != nil ||
		validateUUIDv4(r.SurvivorPersonID) != nil || validateUUIDv4(r.AbsorbedPersonID) != nil ||
		r.SurvivorPersonID == r.AbsorbedPersonID || !validPersonName(r.AbsorbedDisplayName) ||
		r.SurvivorRevisionBefore < 1 || r.SurvivorRevisionAfter != r.SurvivorRevisionBefore+1 ||
		len(r.MovedJSON) == 0 || len(r.MovedJSON) > document.MaxPersonMergeMovedBytes {
		return errors.New("invalid person merge metadata")
	}
	var moved PersonMergeMoved
	if err := json.Unmarshal(r.MovedJSON, &moved, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid person merge moved authority: %w", err)
	}
	canonicalMoved, err := canonical.Marshal(moved)
	if err != nil || !bytes.Equal(canonicalMoved, r.MovedJSON) {
		return errors.New("person merge moved authority is not canonical")
	}
	for _, ids := range [][]string{moved.IdentityIDs, moved.AssignmentIDs, moved.AssertionIDs, moved.SupersededCandidates} {
		if err := validateUniqueMetadataUUIDs(ids); err != nil {
			return err
		}
	}
	for _, external := range moved.ExternalUIDs {
		if !validExternalTuple(external.System, external.ArchiveID, external.UID) {
			return errors.New("invalid person merge external UID")
		}
	}
	for _, identity := range moved.DeduplicatedIdentities {
		if validateUUIDv4(identity.RetainedIdentityID) != nil || identity.RetainedIdentityID == identity.IdentityID {
			return errors.New("invalid person merge retained identity")
		}
		if err := validateMetadataPersonIdentity(metadataPersonIdentity{
			Type: metadataPersonIdentityType, IdentityID: identity.IdentityID, PersonID: r.SurvivorPersonID,
			Kind: identity.Kind, ValueNormalized: identity.ValueNormalized, ValueDisplay: identity.ValueDisplay,
			ScopeKind: identity.ScopeKind, ScopeValue: identity.ScopeValue, Normalization: identity.Normalization,
			Origin: identity.Origin, EvidenceKind: identity.EvidenceKind, EvidenceID: identity.EvidenceID,
			Confidence: identity.Confidence, RecordedAt: identity.RecordedAt,
		}); err != nil {
			return fmt.Errorf("invalid person merge deduplicated identity: %w", err)
		}
	}
	for _, movedAssertion := range moved.DeduplicatedAssertions {
		assertion := movedAssertion.Assertion
		if validateUUIDv4(movedAssertion.RetainedAssertionID) != nil || movedAssertion.RetainedAssertionID == assertion.AssertionID {
			return errors.New("invalid person merge retained assertion")
		}
		if err := validateMetadataPersonAssertion(metadataPersonDocumentAssertion{
			Type: metadataPersonAssertionType, AssertionID: assertion.AssertionID, ContentVersionID: assertion.ContentVersionID,
			PersonID: assertion.PersonID, Role: assertion.Role, Action: assertion.Action, Note: assertion.Note,
			RecordedAt: assertion.RecordedAt, Revision: assertion.Revision,
		}); err != nil {
			return fmt.Errorf("invalid person merge deduplicated assertion: %w", err)
		}
	}
	return validateMetadataTime("person merge created_at", r.CreatedAt)
}

func validateMetadataPersonSplit(r metadataPersonSplit) error {
	if r.Type != metadataPersonSplitType || validateUUIDv4(r.OperationID) != nil ||
		validateCatalogSHA256(r.RequestSHA256, "person split request digest") != nil || len(r.ReceiptJSON) == 0 {
		return errors.New("invalid person split metadata")
	}
	var receipt PersonSplitReceipt
	if err := json.Unmarshal(r.ReceiptJSON, &receipt, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid person split receipt: %w", err)
	}
	canonicalReceipt, err := canonical.Marshal(receipt)
	if err != nil || !bytes.Equal(canonicalReceipt, r.ReceiptJSON) || receipt.OperationID != r.OperationID ||
		validateUUIDv4(receipt.SourcePersonID) != nil || validateUUIDv4(receipt.NewPersonID) != nil ||
		receipt.SourcePersonID == receipt.NewPersonID || validateUniqueMetadataUUIDs(receipt.MovedIdentityIDs) != nil ||
		receipt.CreatedAt != r.CreatedAt {
		return errors.New("invalid person split receipt authority")
	}
	return validateMetadataTime("person split created_at", r.CreatedAt)
}

func validateMetadataCustodianAssignment(r metadataCustodianAssignment) error {
	if r.Type != metadataCustodianAssignmentType || validateUUIDv4(r.AssignmentID) != nil || r.Revision < 1 ||
		!validPersonName(r.RawLabel) || r.RawLabelFolded != document.FoldPersonName(r.RawLabel) ||
		!validCustodianClassification(r.Rank, r.Basis) ||
		!validCustodianSourceRef(r.SourceRef) {
		return errors.New("invalid custodian assignment metadata")
	}
	if r.PersonID != nil && validateUUIDv4(*r.PersonID) != nil {
		return errors.New("invalid custodian assignment person")
	}
	switch r.ScopeKind {
	case "collection":
		if r.IngestID == nil || r.PackageID != nil || r.PackageRecordID != nil || r.NodeID != nil || r.ContentVersionID != nil {
			return errors.New("invalid custodian scope coordinates")
		}
	case "package":
		if r.IngestID != nil || r.PackageID == nil || r.PackageRecordID == nil || r.NodeID != nil || r.ContentVersionID != nil {
			return errors.New("invalid custodian scope coordinates")
		}
	case "document":
		if r.IngestID != nil || r.PackageID != nil || r.PackageRecordID != nil || r.NodeID == nil || r.ContentVersionID == nil {
			return errors.New("invalid custodian scope coordinates")
		}
	default:
		return errors.New("invalid custodian assignment scope")
	}
	scope := CustodianScope{Kind: r.ScopeKind}
	if r.IngestID != nil {
		scope.IngestID = *r.IngestID
	}
	if r.PackageID != nil {
		scope.PackageID = *r.PackageID
	}
	if r.PackageRecordID != nil {
		scope.PackageRecordID = *r.PackageRecordID
		scope.HasPackageRecordID = true
	}
	if r.NodeID != nil {
		scope.NodeID = *r.NodeID
	}
	if r.ContentVersionID != nil {
		scope.ContentVersionID = *r.ContentVersionID
	}
	if err := validateCustodianScope(scope); err != nil {
		return err
	}
	if scope.Kind == "collection" && validateUUIDv4(scope.IngestID) != nil ||
		scope.Kind == "package" && validateUUIDv4(scope.PackageID) != nil ||
		scope.Kind == "document" && validateUUIDv4(scope.ContentVersionID) != nil {
		return errors.New("invalid custodian assignment scope identity")
	}
	if err := validateMetadataTime("custodian assignment recorded_at", r.RecordedAt); err != nil {
		return err
	}
	if r.RetiredAt != nil {
		return validateMetadataTime("custodian assignment retired_at", *r.RetiredAt)
	}
	return nil
}

func validateMetadataPersonAssertion(r metadataPersonDocumentAssertion) error {
	assertion := PersonDocumentAssertion{AssertionID: r.AssertionID, ContentVersionID: r.ContentVersionID,
		PersonID: r.PersonID, Role: r.Role, Action: r.Action, Note: r.Note, Revision: r.Revision}
	if r.Type != metadataPersonAssertionType || validatePersonAssertion(assertion) != nil {
		return errors.New("invalid person document assertion metadata")
	}
	return validateMetadataTime("person document assertion recorded_at", r.RecordedAt)
}

func validateMetadataPersonCandidate(r metadataPersonMatchCandidate) error {
	if r.Type != metadataPersonCandidateType || validateUUIDv4(r.CandidateID) != nil || r.Revision < 1 ||
		document.ValidateActorKeyV1(r.ActorKey) != nil || !document.ValidPersonIdentityText(r.ActorKey) || !validPersonName(r.DisplayName) ||
		!slices.Contains([]string{"open", "linked", "rejected", "superseded"}, r.State) ||
		len(r.EvidenceJSON) == 0 || len(r.EvidenceJSON) > maxPersonCandidateEvidence || r.OccurrenceCount < 1 {
		return errors.New("invalid person candidate metadata")
	}
	if r.SuggestedPersonID != nil && validateUUIDv4(*r.SuggestedPersonID) != nil ||
		r.DecidedPersonID != nil && validateUUIDv4(*r.DecidedPersonID) != nil {
		return errors.New("invalid person candidate person reference")
	}
	normalized, err := normalizePersonCandidate(PersonMatchCandidate{
		ActorKey: r.ActorKey, DisplayName: r.DisplayName, Reason: r.Reason,
		Evidence: r.EvidenceJSON, EvidenceSHA256: r.EvidenceSHA256,
	})
	if err != nil || normalized.OccurrenceCount != r.OccurrenceCount ||
		normalized.EvidenceSHA256 != r.EvidenceSHA256 || !bytes.Equal(normalized.Evidence, r.EvidenceJSON) {
		return errors.New("invalid person candidate evidence")
	}
	if r.State == "open" && (r.DecidedPersonID != nil || r.DecidedAt != nil) ||
		r.State == "linked" && (r.DecidedPersonID == nil || r.DecidedAt == nil) ||
		r.State == "rejected" && (r.DecidedPersonID != nil || r.DecidedAt == nil) ||
		r.State == "superseded" && (r.DecidedPersonID != nil || r.DecidedAt != nil) {
		return errors.New("invalid person candidate decision state")
	}
	if err := validateMetadataTime("person candidate created_at", r.CreatedAt); err != nil {
		return err
	}
	if r.DecidedAt != nil {
		return validateMetadataTime("person candidate decided_at", *r.DecidedAt)
	}
	return nil
}

func validateUniqueMetadataUUIDs(ids []string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if validateUUIDv4(id) != nil || seen[id] {
			return errors.New("invalid or repeated person metadata identity")
		}
		seen[id] = true
	}
	return nil
}

func validatePersonMetadataState(ctx context.Context, q metadataQuerier) error {
	for _, table := range personMetadataTables {
		if err := table.validateRows(ctx, q); err != nil {
			return fmt.Errorf("validating person metadata records: %w", err)
		}
	}
	// Receipts and candidate evidence describe past edits. Later merges, retirement,
	// identity removal, or version deletion do not invalidate retained history.
	checks := []struct {
		name  string
		query string
		args  []any
	}{
		{"person alias authority", `SELECT EXISTS(
			SELECT 1 FROM person_aliases a
			LEFT JOIN persons retired ON retired.person_id=a.retired_person_id
			LEFT JOIN persons survivor ON survivor.person_id=a.surviving_person_id
			WHERE (a.reason='merged' AND (retired.person_id IS NOT NULL OR survivor.person_id IS NULL OR
				EXISTS(SELECT 1 FROM person_aliases next WHERE next.retired_person_id=a.surviving_person_id)))
			   OR (a.reason='deleted' AND (retired.state<>'retired' OR a.surviving_person_id IS NOT NULL))
		)`, nil},
		{"person merge authority", `SELECT EXISTS(
			SELECT 1 FROM person_merges m
			LEFT JOIN persons survivor ON survivor.person_id=m.survivor_person_id
			LEFT JOIN person_aliases survivor_alias ON survivor_alias.retired_person_id=m.survivor_person_id
			LEFT JOIN person_aliases a ON a.retired_person_id=m.absorbed_person_id
			WHERE (survivor.person_id IS NULL AND survivor_alias.retired_person_id IS NULL) OR a.retired_person_id IS NULL
		)`, nil},
		{"custodian scope authority", `SELECT EXISTS(
			SELECT 1 FROM custodian_assignments c
			LEFT JOIN ingests i ON i.id=c.ingest_id
			LEFT JOIN packages p ON p.package_id=c.package_id
			LEFT JOIN content_versions v ON v.version_id=c.content_version_id
			WHERE (c.scope_kind='collection' AND i.id IS NULL)
			   OR (c.scope_kind='package' AND (p.package_id IS NULL OR
			       (c.package_record_id<>'' AND NOT EXISTS(SELECT 1 FROM package_records r
			         WHERE r.package_id=c.package_id AND r.row_id=c.package_record_id))))
			   OR (c.scope_kind='document' AND (v.version_id IS NULL OR v.node_id<>c.node_id))
		)`, nil},
		{"person identity bounds", `SELECT EXISTS(
			SELECT 1 FROM person_identities GROUP BY person_id HAVING COUNT(*)>?
		)`, []any{document.MaxPersonIdentitiesPerPerson}},
		{"person external identity bounds", `SELECT EXISTS(
			SELECT 1 FROM person_external_identities GROUP BY person_id HAVING COUNT(*)>?
		)`, []any{document.MaxPersonExternalIdentities}},
		{"person candidate queue bounds", `SELECT COUNT(*)>? FROM person_match_candidates WHERE state='open'`, []any{maxOpenPersonCandidates}},
		{"person candidate references", `SELECT EXISTS(
			SELECT 1 FROM person_match_candidates c
			WHERE (c.suggested_person_id IS NOT NULL
			  AND NOT EXISTS(SELECT 1 FROM persons p WHERE p.person_id=c.suggested_person_id)
			  AND NOT EXISTS(SELECT 1 FROM person_aliases a WHERE a.retired_person_id=c.suggested_person_id))
			   OR (c.decided_person_id IS NOT NULL
			  AND NOT EXISTS(SELECT 1 FROM persons p WHERE p.person_id=c.decided_person_id)
			  AND NOT EXISTS(SELECT 1 FROM person_aliases a WHERE a.retired_person_id=c.decided_person_id))
		)`, nil},
	}
	for _, check := range checks {
		var invalid bool
		if err := q.QueryRowContext(ctx, check.query, check.args...).Scan(&invalid); err != nil {
			return fmt.Errorf("validating %s: %w", check.name, err)
		}
		if invalid {
			return fmt.Errorf("invalid %s", check.name)
		}
	}
	if err := validatePersonExternalAliasMetadataState(ctx, q); err != nil {
		return err
	}
	return validatePersonSplitMetadataState(ctx, q)
}

func validatePersonExternalAliasMetadataState(ctx context.Context, q metadataQuerier) error {
	var externalCycle, externalOverlong bool
	if err := q.QueryRowContext(ctx, `WITH RECURSIVE chain(system,archive_id,start_uid,uid,depth) AS (
		SELECT system,archive_id,retired_uid,surviving_uid,1 FROM person_external_uid_aliases
		UNION ALL
		SELECT c.system,c.archive_id,c.start_uid,a.surviving_uid,c.depth+1
		FROM chain c JOIN person_external_uid_aliases a
		  ON a.system=c.system AND a.archive_id=c.archive_id AND a.retired_uid=c.uid
		WHERE c.depth<65
	) SELECT EXISTS(SELECT 1 FROM chain WHERE uid=start_uid)`).Scan(&externalCycle); err != nil {
		return fmt.Errorf("validating person external UID alias cycles: %w", err)
	}
	if externalCycle {
		return errors.New("invalid person external UID alias cycle")
	}
	if err := q.QueryRowContext(ctx, `WITH RECURSIVE chain(system,archive_id,uid,depth) AS (
		SELECT system,archive_id,surviving_uid,1 FROM person_external_uid_aliases
		UNION ALL
		SELECT c.system,c.archive_id,a.surviving_uid,c.depth+1
		FROM chain c JOIN person_external_uid_aliases a
		  ON a.system=c.system AND a.archive_id=c.archive_id AND a.retired_uid=c.uid
		WHERE c.depth<33
	) SELECT EXISTS(SELECT 1 FROM chain WHERE depth=33)`).Scan(&externalOverlong); err != nil {
		return fmt.Errorf("validating person external UID alias hop limit: %w", err)
	}
	if externalOverlong {
		return errors.New("invalid person external UID alias hop limit")
	}
	return nil
}

func validatePersonSplitMetadataState(ctx context.Context, q metadataQuerier) error {
	rows, err := q.QueryContext(ctx, `SELECT operation_id,receipt_json FROM person_splits ORDER BY operation_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var operationID string
		var raw []byte
		if err := rows.Scan(&operationID, &raw); err != nil {
			return err
		}
		var receipt PersonSplitReceipt
		if err := json.Unmarshal(raw, &receipt, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		var sourceExists, targetExists bool
		if err := q.QueryRowContext(ctx, `SELECT
			EXISTS(SELECT 1 FROM persons WHERE person_id=? UNION ALL SELECT 1 FROM person_aliases WHERE retired_person_id=?),
			EXISTS(SELECT 1 FROM persons WHERE person_id=? UNION ALL SELECT 1 FROM person_aliases WHERE retired_person_id=?)`,
			receipt.SourcePersonID, receipt.SourcePersonID, receipt.NewPersonID, receipt.NewPersonID).Scan(&sourceExists, &targetExists); err != nil {
			return err
		}
		if !sourceExists || !targetExists {
			return fmt.Errorf("person split %s references missing people", operationID)
		}
	}
	return rows.Err()
}
