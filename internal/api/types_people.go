package api

import "go.kenn.io/docbank/internal/store"

type Person struct {
	PersonID               string `json:"person_id" format:"uuid"`
	DisplayName            string `json:"display_name"`
	Origin                 string `json:"origin"`
	State                  string `json:"state"`
	Revision               int64  `json:"revision" minimum:"1"`
	CreatedAt              string `json:"created_at" format:"date-time"`
	UpdatedAt              string `json:"updated_at" format:"date-time"`
	ReachedThroughPersonID string `json:"reached_through_person_id,omitzero" format:"uuid"`
}

type PersonIdentity struct {
	IdentityID      string `json:"identity_id" format:"uuid"`
	Kind            string `json:"kind"`
	ValueDisplay    string `json:"value_display"`
	ValueNormalized string `json:"value_normalized"`
	ScopeKind       string `json:"scope_kind,omitzero"`
	ScopeValue      string `json:"scope_value,omitzero"`
	Normalization   string `json:"normalization"`
	Origin          string `json:"origin"`
	EvidenceKind    string `json:"evidence_kind"`
	EvidenceID      string `json:"evidence_id"`
	Confidence      string `json:"confidence"`
	RecordedAt      string `json:"recorded_at" format:"date-time"`
}

type PersonExternalIdentity struct {
	System              string `json:"system"`
	ArchiveID           string `json:"archive_id"`
	UID                 string `json:"uid"`
	UIDKind             string `json:"uid_kind"`
	UIDState            string `json:"uid_state"`
	LastSeenRevision    *int64 `json:"last_seen_revision,omitzero"`
	DisplayNameSnapshot string `json:"display_name_snapshot"`
	LinkedAt            string `json:"linked_at" format:"date-time"`
	UpdatedAt           string `json:"updated_at" format:"date-time"`
}

type PersonDetail struct {
	Person

	Identities         []PersonIdentity         `json:"identities" maxItems:"200"`
	ExternalIdentities []PersonExternalIdentity `json:"external_identities" maxItems:"64"`
}

type CreatePersonRequest struct {
	DisplayName string `json:"display_name" minLength:"1"`
}

type RenamePersonRequest struct {
	DisplayName string `json:"display_name" minLength:"1"`
}

type MergePersonRequest struct {
	AbsorbedPersonID string `json:"absorbed_person_id" format:"uuid"`
	AbsorbedRevision int64  `json:"absorbed_revision" minimum:"1"`
	OperationID      string `json:"operation_id" format:"uuid"`
}

type PersonExternalUID struct {
	System    string `json:"system"`
	ArchiveID string `json:"archive_id"`
	UID       string `json:"uid"`
}

type SplitPersonRequest struct {
	OperationID        string              `json:"operation_id" format:"uuid"`
	DisplayName        string              `json:"display_name" minLength:"1"`
	IdentityIDs        []string            `json:"identity_ids,omitzero" maxItems:"200"`
	AssignmentIDs      []string            `json:"assignment_ids,omitzero"`
	ExternalIdentities []PersonExternalUID `json:"external_identities,omitzero" maxItems:"64"`
}

type PersonMergeMovedCounts struct {
	Assertions             int `json:"assertions"`
	DeduplicatedAssertions int `json:"deduplicated_assertions"`
	SupersededCandidates   int `json:"superseded_candidates"`
	Identities             int `json:"identities"`
	DeduplicatedIdentities int `json:"deduplicated_identities"`
	ExternalUIDs           int `json:"external_uids"`
	CustodianAssignments   int `json:"custodian_assignments"`
}

type PersonMergeReceipt struct {
	MergeID                string                 `json:"merge_id" format:"uuid"`
	OperationID            string                 `json:"operation_id" format:"uuid"`
	SurvivorPersonID       string                 `json:"survivor_person_id" format:"uuid"`
	AbsorbedPersonID       string                 `json:"absorbed_person_id" format:"uuid"`
	AbsorbedDisplayName    string                 `json:"absorbed_display_name"`
	SurvivorRevisionBefore int64                  `json:"survivor_revision_before" minimum:"1"`
	SurvivorRevisionAfter  int64                  `json:"survivor_revision_after" minimum:"1"`
	CreatedAt              string                 `json:"created_at" format:"date-time"`
	Moved                  PersonMergeMovedCounts `json:"moved"`
}

type PersonSplitReceipt struct {
	OperationID         string   `json:"operation_id" format:"uuid"`
	SourcePersonID      string   `json:"source_person_id" format:"uuid"`
	NewPersonID         string   `json:"new_person_id" format:"uuid"`
	SourceRevisionAfter int64    `json:"source_revision_after" minimum:"1"`
	MovedIdentityIDs    []string `json:"moved_identity_ids"`
	CreatedAt           string   `json:"created_at" format:"date-time"`
}

type personOutput struct {
	ETag string `header:"ETag"`
	Body Person
}

type personDetailOutput struct {
	ETag string `header:"ETag"`
	Body PersonDetail
}

type personMergeOutput struct {
	ETag string `header:"ETag"`
	Body PersonMergeReceipt
}

type personSplitOutput struct {
	ETag string `header:"ETag"`
	Body PersonSplitReceipt
}

type personCustodianPageOutput struct {
	Body PersonCustodianPage
}

type PersonCustodianAssignment struct {
	AssignmentID     string `json:"assignment_id"`
	ScopeKind        string `json:"scope_kind"`
	IngestID         string `json:"ingest_id,omitzero"`
	PackageID        string `json:"package_id,omitzero"`
	PackageRecordID  string `json:"package_record_id,omitzero"`
	NodeID           int64  `json:"node_id,omitzero" minimum:"1"`
	ContentVersionID string `json:"content_version_id,omitzero"`
	PersonID         string `json:"person_id,omitzero"`
	RawLabel         string `json:"raw_label"`
	Rank             string `json:"rank"`
	Basis            string `json:"basis"`
	SourceRef        string `json:"source_ref"`
	Revision         int64  `json:"revision"`
	RecordedAt       string `json:"recorded_at"`
}

type PersonCustodianPage struct {
	Items      []PersonCustodianAssignment `json:"items"`
	Total      int64                       `json:"total"`
	NextCursor string                      `json:"next_cursor,omitzero"`
}

func fromStorePerson(value store.Person, reachedThrough string) Person {
	return Person{PersonID: value.PersonID, DisplayName: value.DisplayName, Origin: value.Origin, State: value.State,
		Revision: value.Revision, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, ReachedThroughPersonID: reachedThrough}
}

func fromStorePersonIdentity(value store.PersonIdentity) PersonIdentity {
	return PersonIdentity{IdentityID: value.IdentityID, Kind: value.Kind, ValueDisplay: value.ValueDisplay,
		ValueNormalized: value.ValueNormalized, ScopeKind: value.ScopeKind, ScopeValue: value.ScopeValue,
		Normalization: value.Normalization, Origin: value.Origin, EvidenceKind: value.EvidenceKind,
		EvidenceID: value.EvidenceID, Confidence: value.Confidence, RecordedAt: value.RecordedAt}
}

func fromStorePersonExternalIdentity(value store.PersonExternalIdentity) PersonExternalIdentity {
	return PersonExternalIdentity{System: value.System, ArchiveID: value.ArchiveID, UID: value.UID, UIDKind: value.UIDKind,
		UIDState: value.UIDState, LastSeenRevision: value.LastSeenRevision, DisplayNameSnapshot: value.DisplayNameSnapshot,
		LinkedAt: value.LinkedAt, UpdatedAt: value.UpdatedAt}
}

func fromStorePersonDetail(value store.PersonDetail) PersonDetail {
	out := PersonDetail{Person: fromStorePerson(value.Person, value.ReachedThrough), Identities: make([]PersonIdentity, 0, len(value.Identities)), ExternalIdentities: make([]PersonExternalIdentity, 0, len(value.External))}
	for _, identity := range value.Identities {
		out.Identities = append(out.Identities, fromStorePersonIdentity(identity))
	}
	for _, identity := range value.External {
		out.ExternalIdentities = append(out.ExternalIdentities, fromStorePersonExternalIdentity(identity))
	}
	return out
}

func fromStorePersonMergeReceipt(value store.PersonMergeReceipt) PersonMergeReceipt {
	return PersonMergeReceipt{MergeID: value.MergeID, OperationID: value.OperationID, SurvivorPersonID: value.SurvivorPersonID,
		AbsorbedPersonID: value.AbsorbedPersonID, AbsorbedDisplayName: value.AbsorbedDisplayName,
		SurvivorRevisionBefore: value.SurvivorRevisionBefore, SurvivorRevisionAfter: value.SurvivorRevisionAfter,
		CreatedAt: value.CreatedAt, Moved: PersonMergeMovedCounts{Assertions: len(value.Moved.AssertionIDs),
			DeduplicatedAssertions: len(value.Moved.DeduplicatedAssertions), SupersededCandidates: len(value.Moved.SupersededCandidates),
			Identities: len(value.Moved.IdentityIDs), DeduplicatedIdentities: len(value.Moved.DeduplicatedIdentities),
			ExternalUIDs: len(value.Moved.ExternalUIDs), CustodianAssignments: len(value.Moved.AssignmentIDs)}}
}

func fromStorePersonSplitReceipt(value store.PersonSplitReceipt) PersonSplitReceipt {
	return PersonSplitReceipt{OperationID: value.OperationID, SourcePersonID: value.SourcePersonID, NewPersonID: value.NewPersonID,
		MovedIdentityIDs: append([]string{}, value.MovedIdentityIDs...), CreatedAt: value.CreatedAt}
}

func fromStorePersonCustodian(value store.CustodianAssignment) PersonCustodianAssignment {
	out := PersonCustodianAssignment{AssignmentID: value.AssignmentID, ScopeKind: value.ScopeKind,
		RawLabel: value.RawLabel, Rank: value.Rank, Basis: value.Basis, SourceRef: value.SourceRef,
		Revision: value.Revision, RecordedAt: value.RecordedAt}
	if value.IngestID != nil {
		out.IngestID = *value.IngestID
	}
	if value.PackageID != nil {
		out.PackageID = *value.PackageID
	}
	if value.PackageRecordID != nil {
		out.PackageRecordID = *value.PackageRecordID
	}
	if value.NodeID != nil {
		out.NodeID = *value.NodeID
	}
	if value.ContentVersionID != nil {
		out.ContentVersionID = *value.ContentVersionID
	}
	if value.PersonID != nil {
		out.PersonID = *value.PersonID
	}
	return out
}
