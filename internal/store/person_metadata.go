package store

import (
	"bytes"
	"context"
	"database/sql"
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
	PersonID          string `json:"person_id"`
	DisplayName       string `json:"display_name"`
	DisplayNameFolded string `json:"display_name_folded"`
	Origin            string `json:"origin"`
	State             string `json:"state"`
	Revision          int64  `json:"revision"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

type metadataPersonIdentity struct {
	Type            string `json:"type"`
	IdentityID      string `json:"identity_id"`
	PersonID        string `json:"person_id"`
	Kind            string `json:"kind"`
	ValueNormalized string `json:"value_normalized"`
	ValueDisplay    string `json:"value_display"`
	ScopeKind       string `json:"scope_kind"`
	ScopeValue      string `json:"scope_value"`
	Normalization   string `json:"normalization"`
	Origin          string `json:"origin"`
	EvidenceKind    string `json:"evidence_kind"`
	EvidenceID      string `json:"evidence_id"`
	Confidence      string `json:"confidence"`
	RecordedAt      string `json:"recorded_at"`
}

type metadataPersonExternalIdentity struct {
	Type                string `json:"type"`
	PersonID            string `json:"person_id"`
	System              string `json:"system"`
	ArchiveID           string `json:"archive_id"`
	UID                 string `json:"uid"`
	UIDKind             string `json:"uid_kind"`
	UIDState            string `json:"uid_state"`
	LastSeenRevision    *int64 `json:"last_seen_revision"`
	DisplayNameSnapshot string `json:"display_name_snapshot"`
	LinkedAt            string `json:"linked_at"`
	UpdatedAt           string `json:"updated_at"`
}

type metadataPersonExternalUIDAlias struct {
	Type         string `json:"type"`
	System       string `json:"system"`
	ArchiveID    string `json:"archive_id"`
	RetiredUID   string `json:"retired_uid"`
	SurvivingUID string `json:"surviving_uid"`
	ObservedAt   string `json:"observed_at"`
}

type metadataPersonAlias struct {
	Type              string  `json:"type"`
	RetiredPersonID   string  `json:"retired_person_id"`
	SurvivingPersonID *string `json:"surviving_person_id"`
	Reason            string  `json:"reason"`
	RetiredAt         string  `json:"retired_at"`
}

type metadataPersonMerge struct {
	Type                   string `json:"type"`
	MergeID                string `json:"merge_id"`
	OperationID            string `json:"operation_id"`
	RequestSHA256          string `json:"request_sha256"`
	SurvivorPersonID       string `json:"survivor_person_id"`
	AbsorbedPersonID       string `json:"absorbed_person_id"`
	AbsorbedDisplayName    string `json:"absorbed_display_name"`
	MovedJSON              []byte `json:"moved_json"`
	SurvivorRevisionBefore int64  `json:"survivor_revision_before"`
	SurvivorRevisionAfter  int64  `json:"survivor_revision_after"`
	CreatedAt              string `json:"created_at"`
}

type metadataPersonSplit struct {
	Type          string `json:"type"`
	OperationID   string `json:"operation_id"`
	RequestSHA256 string `json:"request_sha256"`
	ReceiptJSON   []byte `json:"receipt_json"`
	CreatedAt     string `json:"created_at"`
}

type metadataCustodianAssignment struct {
	Type             string  `json:"type"`
	AssignmentID     string  `json:"assignment_id"`
	ScopeKind        string  `json:"scope_kind"`
	IngestID         *string `json:"ingest_id"`
	NodeID           *int64  `json:"node_id"`
	ContentVersionID *string `json:"content_version_id"`
	PersonID         *string `json:"person_id"`
	RawLabel         string  `json:"raw_label"`
	RawLabelFolded   string  `json:"raw_label_folded"`
	Rank             string  `json:"rank"`
	Basis            string  `json:"basis"`
	SourceRef        string  `json:"source_ref"`
	Revision         int64   `json:"revision"`
	RecordedAt       string  `json:"recorded_at"`
	RetiredAt        *string `json:"retired_at"`
}

type metadataPersonDocumentAssertion struct {
	Type             string `json:"type"`
	AssertionID      string `json:"assertion_id"`
	ContentVersionID string `json:"content_version_id"`
	PersonID         string `json:"person_id"`
	Role             string `json:"role"`
	Action           string `json:"action"`
	Note             string `json:"note"`
	RecordedAt       string `json:"recorded_at"`
	Revision         int64  `json:"revision"`
}

type metadataPersonMatchCandidate struct {
	Type              string  `json:"type"`
	CandidateID       string  `json:"candidate_id"`
	ActorKey          string  `json:"actor_key"`
	DisplayName       string  `json:"display_name"`
	SuggestedPersonID *string `json:"suggested_person_id"`
	Reason            string  `json:"reason"`
	EvidenceJSON      []byte  `json:"evidence_json"`
	EvidenceSHA256    string  `json:"evidence_sha256"`
	OccurrenceCount   int64   `json:"occurrence_count"`
	Revision          int64   `json:"revision"`
	State             string  `json:"state"`
	DecidedPersonID   *string `json:"decided_person_id"`
	CreatedAt         string  `json:"created_at"`
	DecidedAt         *string `json:"decided_at"`
}

var personMetadataRequiredFields = map[string][]string{
	metadataPersonType:              {metadataTypeField, "person_id", "display_name", "display_name_folded", "origin", "state", metadataRevisionField, metadataCreatedAtField, "updated_at"},
	metadataPersonIdentityType:      {metadataTypeField, "identity_id", "person_id", "kind", "value_normalized", "value_display", "scope_kind", "scope_value", "normalization", "origin", "evidence_kind", "evidence_id", "confidence", "recorded_at"},
	metadataPersonExternalType:      {metadataTypeField, "person_id", "system", "archive_id", "uid", "uid_kind", "uid_state", "last_seen_revision", "display_name_snapshot", "linked_at", "updated_at"},
	metadataPersonExternalAliasType: {metadataTypeField, "system", "archive_id", "retired_uid", "surviving_uid", "observed_at"},
	metadataPersonAliasType:         {metadataTypeField, "retired_person_id", "surviving_person_id", "reason", "retired_at"},
	metadataPersonMergeType:         {metadataTypeField, "merge_id", "operation_id", "request_sha256", "survivor_person_id", "absorbed_person_id", "absorbed_display_name", "moved_json", "survivor_revision_before", "survivor_revision_after", metadataCreatedAtField},
	metadataPersonSplitType:         {metadataTypeField, "operation_id", "request_sha256", "receipt_json", metadataCreatedAtField},
	metadataCustodianAssignmentType: {metadataTypeField, "assignment_id", "scope_kind", metadataIngestIDField, "node_id", metadataContentVersionIDField, "person_id", "raw_label", "raw_label_folded", "rank", "basis", "source_ref", metadataRevisionField, "recorded_at", "retired_at"},
	metadataPersonAssertionType:     {metadataTypeField, "assertion_id", metadataContentVersionIDField, "person_id", "role", "action", "note", "recorded_at", metadataRevisionField},
	metadataPersonCandidateType:     {metadataTypeField, "candidate_id", "actor_key", "display_name", "suggested_person_id", "reason", "evidence_json", "evidence_sha256", "occurrence_count", metadataRevisionField, "state", "decided_person_id", metadataCreatedAtField, "decided_at"},
}

var personMetadataNullableFields = map[string]map[string]bool{
	metadataPersonExternalType: {"last_seen_revision": true},
	metadataPersonAliasType:    {"surviving_person_id": true},
	metadataCustodianAssignmentType: {
		metadataIngestIDField: true,
		"node_id":             true, metadataContentVersionIDField: true, "person_id": true, "retired_at": true,
	},
	metadataPersonCandidateType: {
		"suggested_person_id": true, "decided_person_id": true, "decided_at": true,
	},
}

func isPersonMetadataType(kind string) bool {
	_, ok := personMetadataRequiredFields[kind]
	return ok
}

func exportPersonMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	exporters := []func(context.Context, metadataQuerier, metadataWrite) error{
		exportPersons, exportPersonIdentities, exportPersonExternalIdentities,
		exportPersonExternalUIDAliases, exportPersonAliases, exportPersonMerges,
		exportPersonSplits, exportCustodianAssignments, exportPersonAssertions,
		exportPersonCandidates,
	}
	for _, export := range exporters {
		if err := export(ctx, q, write); err != nil {
			return err
		}
	}
	return nil
}

func exportPersons(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at FROM persons ORDER BY person_id`)
	if err != nil {
		return fmt.Errorf("exporting persons: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPerson{Type: metadataPersonType}
		if err := rows.Scan(&r.PersonID, &r.DisplayName, &r.DisplayNameFolded, &r.Origin, &r.State, &r.Revision, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return err
		}
		if err := validateMetadataPerson(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonIdentities(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT identity_id,person_id,kind,value_normalized,value_display,scope_kind,scope_value,normalization,origin,evidence_kind,evidence_id,confidence,recorded_at FROM person_identities ORDER BY identity_id`)
	if err != nil {
		return fmt.Errorf("exporting person identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonIdentity{Type: metadataPersonIdentityType}
		if err := rows.Scan(&r.IdentityID, &r.PersonID, &r.Kind, &r.ValueNormalized, &r.ValueDisplay, &r.ScopeKind, &r.ScopeValue, &r.Normalization, &r.Origin, &r.EvidenceKind, &r.EvidenceID, &r.Confidence, &r.RecordedAt); err != nil {
			return err
		}
		if err := validateMetadataPersonIdentity(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonExternalIdentities(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT person_id,system,archive_id,uid,uid_kind,uid_state,last_seen_revision,display_name_snapshot,linked_at,updated_at FROM person_external_identities ORDER BY system,archive_id,uid`)
	if err != nil {
		return fmt.Errorf("exporting person external identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonExternalIdentity{Type: metadataPersonExternalType}
		var revision sql.NullInt64
		if err := rows.Scan(&r.PersonID, &r.System, &r.ArchiveID, &r.UID, &r.UIDKind, &r.UIDState, &revision, &r.DisplayNameSnapshot, &r.LinkedAt, &r.UpdatedAt); err != nil {
			return err
		}
		r.LastSeenRevision = int64Ptr(revision)
		if err := validateMetadataPersonExternalIdentity(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonExternalUIDAliases(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT system,archive_id,retired_uid,surviving_uid,observed_at FROM person_external_uid_aliases ORDER BY system,archive_id,retired_uid`)
	if err != nil {
		return fmt.Errorf("exporting person external UID aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonExternalUIDAlias{Type: metadataPersonExternalAliasType}
		if err := rows.Scan(&r.System, &r.ArchiveID, &r.RetiredUID, &r.SurvivingUID, &r.ObservedAt); err != nil {
			return err
		}
		if err := validateMetadataPersonExternalUIDAlias(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonAliases(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT retired_person_id,surviving_person_id,reason,retired_at FROM person_aliases ORDER BY retired_person_id`)
	if err != nil {
		return fmt.Errorf("exporting person aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonAlias{Type: metadataPersonAliasType}
		var survivor sql.NullString
		if err := rows.Scan(&r.RetiredPersonID, &survivor, &r.Reason, &r.RetiredAt); err != nil {
			return err
		}
		r.SurvivingPersonID = stringPtr(survivor)
		if err := validateMetadataPersonAlias(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonMerges(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT merge_id,operation_id,request_sha256,survivor_person_id,absorbed_person_id,absorbed_display_name,moved_json,survivor_revision_before,survivor_revision_after,created_at FROM person_merges ORDER BY merge_id`)
	if err != nil {
		return fmt.Errorf("exporting person merges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonMerge{Type: metadataPersonMergeType}
		if err := rows.Scan(&r.MergeID, &r.OperationID, &r.RequestSHA256, &r.SurvivorPersonID, &r.AbsorbedPersonID, &r.AbsorbedDisplayName, &r.MovedJSON, &r.SurvivorRevisionBefore, &r.SurvivorRevisionAfter, &r.CreatedAt); err != nil {
			return err
		}
		if err := validateMetadataPersonMerge(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonSplits(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT operation_id,request_sha256,receipt_json,created_at FROM person_splits ORDER BY operation_id`)
	if err != nil {
		return fmt.Errorf("exporting person splits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonSplit{Type: metadataPersonSplitType}
		if err := rows.Scan(&r.OperationID, &r.RequestSHA256, &r.ReceiptJSON, &r.CreatedAt); err != nil {
			return err
		}
		if err := validateMetadataPersonSplit(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportCustodianAssignments(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT assignment_id,scope_kind,ingest_id,node_id,content_version_id,person_id,raw_label,raw_label_folded,rank,basis,source_ref,revision,recorded_at,retired_at FROM custodian_assignments ORDER BY assignment_id`)
	if err != nil {
		return fmt.Errorf("exporting custodian assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataCustodianAssignment{Type: metadataCustodianAssignmentType}
		var ingest, version, person, retired sql.NullString
		var node sql.NullInt64
		if err := rows.Scan(&r.AssignmentID, &r.ScopeKind, &ingest, &node, &version, &person, &r.RawLabel, &r.RawLabelFolded, &r.Rank, &r.Basis, &r.SourceRef, &r.Revision, &r.RecordedAt, &retired); err != nil {
			return err
		}
		r.IngestID = stringPtr(ingest)
		r.NodeID, r.ContentVersionID, r.PersonID = int64Ptr(node), stringPtr(version), stringPtr(person)
		r.RetiredAt = stringPtr(retired)
		if err := validateMetadataCustodianAssignment(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonAssertions(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision FROM person_document_assertions ORDER BY assertion_id`)
	if err != nil {
		return fmt.Errorf("exporting person assertions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonDocumentAssertion{Type: metadataPersonAssertionType}
		if err := rows.Scan(&r.AssertionID, &r.ContentVersionID, &r.PersonID, &r.Role, &r.Action, &r.Note, &r.RecordedAt, &r.Revision); err != nil {
			return err
		}
		if err := validateMetadataPersonAssertion(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportPersonCandidates(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,decided_person_id,created_at,decided_at FROM person_match_candidates ORDER BY candidate_id`)
	if err != nil {
		return fmt.Errorf("exporting person candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := metadataPersonMatchCandidate{Type: metadataPersonCandidateType}
		var suggested, decided, decidedAt sql.NullString
		if err := rows.Scan(&r.CandidateID, &r.ActorKey, &r.DisplayName, &suggested, &r.Reason, &r.EvidenceJSON, &r.EvidenceSHA256, &r.OccurrenceCount, &r.Revision, &r.State, &decided, &r.CreatedAt, &decidedAt); err != nil {
			return err
		}
		r.SuggestedPersonID, r.DecidedPersonID, r.DecidedAt = stringPtr(suggested), stringPtr(decided), stringPtr(decidedAt)
		if err := validateMetadataPersonCandidate(r); err != nil {
			return err
		}
		if err := write(r); err != nil {
			return err
		}
	}
	return rows.Err()
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
	if r.ScopeKind == "collection" && (r.NodeID != nil || r.ContentVersionID != nil) ||
		r.ScopeKind == "document" && r.IngestID != nil {
		return errors.New("invalid custodian scope coordinates")
	}
	scope := CustodianScope{Kind: r.ScopeKind}
	if r.IngestID != nil {
		scope.IngestID = *r.IngestID
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

func importPersonMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw []byte) error {
	switch kind {
	case metadataPersonType:
		var r metadataPerson
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPerson(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO persons(person_id,display_name,display_name_folded,origin,state,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, r.PersonID, r.DisplayName, r.DisplayNameFolded, r.Origin, r.State, r.Revision, r.CreatedAt, r.UpdatedAt)
		return err
	case metadataPersonIdentityType:
		var r metadataPersonIdentity
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonIdentity(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_identities(identity_id,person_id,kind,value_normalized,value_display,scope_kind,scope_value,normalization,origin,evidence_kind,evidence_id,confidence,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.IdentityID, r.PersonID, r.Kind, r.ValueNormalized, r.ValueDisplay, r.ScopeKind, r.ScopeValue, r.Normalization, r.Origin, r.EvidenceKind, r.EvidenceID, r.Confidence, r.RecordedAt)
		return err
	case metadataPersonExternalType:
		var r metadataPersonExternalIdentity
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonExternalIdentity(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_external_identities(person_id,system,archive_id,uid,uid_kind,uid_state,last_seen_revision,display_name_snapshot,linked_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, r.PersonID, r.System, r.ArchiveID, r.UID, r.UIDKind, r.UIDState, r.LastSeenRevision, r.DisplayNameSnapshot, r.LinkedAt, r.UpdatedAt)
		return err
	case metadataPersonExternalAliasType:
		var r metadataPersonExternalUIDAlias
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonExternalUIDAlias(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_external_uid_aliases(system,archive_id,retired_uid,surviving_uid,observed_at) VALUES(?,?,?,?,?)`, r.System, r.ArchiveID, r.RetiredUID, r.SurvivingUID, r.ObservedAt)
		return err
	case metadataPersonAliasType:
		var r metadataPersonAlias
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonAlias(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_aliases(retired_person_id,surviving_person_id,reason,retired_at) VALUES(?,?,?,?)`, r.RetiredPersonID, r.SurvivingPersonID, r.Reason, r.RetiredAt)
		return err
	default:
		return importPersonDecisionMetadataRecord(ctx, tx, kind, raw)
	}
}

func importPersonDecisionMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw []byte) error {
	switch kind {
	case metadataPersonMergeType:
		var r metadataPersonMerge
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonMerge(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_merges(merge_id,operation_id,request_sha256,survivor_person_id,absorbed_person_id,absorbed_display_name,moved_json,survivor_revision_before,survivor_revision_after,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, r.MergeID, r.OperationID, r.RequestSHA256, r.SurvivorPersonID, r.AbsorbedPersonID, r.AbsorbedDisplayName, r.MovedJSON, r.SurvivorRevisionBefore, r.SurvivorRevisionAfter, r.CreatedAt)
		return err
	case metadataPersonSplitType:
		var r metadataPersonSplit
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonSplit(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_splits(operation_id,request_sha256,receipt_json,created_at) VALUES(?,?,?,?)`, r.OperationID, r.RequestSHA256, r.ReceiptJSON, r.CreatedAt)
		return err
	case metadataCustodianAssignmentType:
		var r metadataCustodianAssignment
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataCustodianAssignment(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO custodian_assignments(assignment_id,scope_kind,ingest_id,node_id,content_version_id,person_id,raw_label,raw_label_folded,rank,basis,source_ref,revision,recorded_at,retired_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.AssignmentID, r.ScopeKind, r.IngestID, r.NodeID, r.ContentVersionID, r.PersonID, r.RawLabel, r.RawLabelFolded, r.Rank, r.Basis, r.SourceRef, r.Revision, r.RecordedAt, r.RetiredAt)
		return err
	case metadataPersonAssertionType:
		var r metadataPersonDocumentAssertion
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonAssertion(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_document_assertions(assertion_id,content_version_id,person_id,role,action,note,recorded_at,revision) VALUES(?,?,?,?,?,?,?,?)`, r.AssertionID, r.ContentVersionID, r.PersonID, r.Role, r.Action, r.Note, r.RecordedAt, r.Revision)
		return err
	case metadataPersonCandidateType:
		var r metadataPersonMatchCandidate
		if err := decodeMetadataRecord(raw, &r); err != nil {
			return err
		}
		if err := validateMetadataPersonCandidate(r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_match_candidates(candidate_id,actor_key,display_name,suggested_person_id,reason,evidence_json,evidence_sha256,occurrence_count,revision,state,decided_person_id,created_at,decided_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.CandidateID, r.ActorKey, r.DisplayName, r.SuggestedPersonID, r.Reason, r.EvidenceJSON, r.EvidenceSHA256, r.OccurrenceCount, r.Revision, r.State, r.DecidedPersonID, r.CreatedAt, r.DecidedAt)
		return err
	default:
		return fmt.Errorf("unknown person metadata type %q", kind)
	}
}

func validatePersonMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := exportPersonMetadata(ctx, q, func(any) error { return nil }); err != nil {
		return fmt.Errorf("validating person metadata records: %w", err)
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
			LEFT JOIN content_versions v ON v.version_id=c.content_version_id
			WHERE (c.scope_kind='collection' AND i.id IS NULL)
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
