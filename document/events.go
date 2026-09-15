package document

import "slices"

const DocumentEventsContractV1 = "document-events/v1"

type DateKind string
type EventPrecision string
type EventTimezoneKind string
type EventRole string
type EventEvidenceKind string
type DocumentKind string

var dateKinds = []DateKind{
	"sent", "received", "document_date", "authored", "created", "modified",
	"captured", "started", "ended", "printed", "accessed", "produced", "exported",
	"imported", "observed", "vault_recorded",
}

var eventPrecisions = []EventPrecision{"year", "month", "date", "hour", "minute", "second", "fraction"}

var eventTimezoneKinds = []EventTimezoneKind{"utc", "offset", "named", "omitted", "invalid"}

var eventRoles = []EventRole{
	"author", "last_saved_by", "custodian", "sender", "recipient", "copied", "blind_copy",
	"organizer", "attendee", "participant",
}

var eventEvidenceKinds = []EventEvidenceKind{
	"source_metadata", "email_generation", "provenance_binding", "content_version", "package_row",
	"output_receipt", "transfer_record", "takeout_claim", "media_source_version",
}

var documentKinds = []DocumentKind{"email", "message", "calendar", "image", "audio_video", "other"}

func AllDateKinds() []DateKind                        { return slices.Clone(dateKinds) }
func AllEventPrecisions() []EventPrecision            { return slices.Clone(eventPrecisions) }
func AllEventTimezoneKinds() []EventTimezoneKind      { return slices.Clone(eventTimezoneKinds) }
func AllEventRoles() []EventRole                      { return slices.Clone(eventRoles) }
func AllEventEvidenceKinds() []EventEvidenceKind      { return slices.Clone(eventEvidenceKinds) }
func AllDocumentKinds() []DocumentKind                { return slices.Clone(documentKinds) }
func ValidDateKind(v DateKind) bool                   { return slices.Contains(dateKinds, v) }
func ValidEventPrecision(v EventPrecision) bool       { return slices.Contains(eventPrecisions, v) }
func ValidEventTimezoneKind(v EventTimezoneKind) bool { return slices.Contains(eventTimezoneKinds, v) }
func ValidEventRole(v EventRole) bool                 { return slices.Contains(eventRoles, v) }
func ValidEventEvidenceKind(v EventEvidenceKind) bool { return slices.Contains(eventEvidenceKinds, v) }
func ValidDocumentKind(v DocumentKind) bool           { return slices.Contains(documentKinds, v) }

const (
	MaxDocumentEvents               = 1024
	MaxDocumentEventActors          = 4096
	MaxDocumentEventsEncodedBytes   = 8 << 20
	MaxDocumentEventRawValueBytes   = 64 << 10
	MaxDocumentEventLocatorBytes    = 4 << 10
	MaxDocumentEventActorClaimBytes = 4 << 10
	MaxActorKeyBytes                = 384
)

type DocumentEventActorV1 struct {
	ActorKey     string            `json:"actor_key"`
	Address      string            `json:"address"`
	Claim        string            `json:"claim"`
	DisplayName  string            `json:"display_name"`
	EvidenceID   string            `json:"evidence_id,omitzero"`
	EvidenceKind EventEvidenceKind `json:"evidence_kind,omitzero"`
	Ordinal      int               `json:"ordinal"`
	Role         EventRole         `json:"role"`
	Sensitive    bool              `json:"sensitive"`
}

type DocumentEventV1 struct {
	Actors          []DocumentEventActorV1 `json:"actors"`
	AxisKey         string                 `json:"axis_key"`
	ClaimBasis      string                 `json:"claim_basis"`
	DateKind        DateKind               `json:"date_kind"`
	DateValue       string                 `json:"date_value"`
	EventID         string                 `json:"event_id"`
	EvidenceID      string                 `json:"evidence_id"`
	EvidenceKind    EventEvidenceKind      `json:"evidence_kind"`
	EvidenceLocator string                 `json:"evidence_locator"`
	EvidenceSHA256  string                 `json:"evidence_sha256"`
	FractionDigits  int                    `json:"fraction_digits"`
	OffsetSeconds   *int                   `json:"offset_seconds,omitzero"`
	ParseConfidence string                 `json:"parse_confidence"`
	Precision       EventPrecision         `json:"precision"`
	RawValue        string                 `json:"raw_value"`
	Sensitive       bool                   `json:"sensitive"`
	SourceKey       string                 `json:"source_key"`
	SourceKindRaw   string                 `json:"source_kind_raw"`
	TimezoneKind    EventTimezoneKind      `json:"timezone_kind"`
	UTCKey          string                 `json:"utc_key,omitzero"`
	ZoneText        string                 `json:"zone_text"`
}

type DocumentEventPrimaryV1 struct {
	EventID    string `json:"event_id"`
	Reason     string `json:"reason"`
	RuleID     string `json:"rule_id"`
	ScopeClass string `json:"scope_class"`
	Disclosure string `json:"disclosure"`
}

type DocumentEventSourceV1 struct {
	EvidenceID     string            `json:"evidence_id"`
	EvidenceKind   EventEvidenceKind `json:"evidence_kind"`
	EvidenceSHA256 string            `json:"evidence_sha256"`
}

type DocumentEventDiagnosticV1 struct {
	Code      string `json:"code"`
	Detail    string `json:"detail"`
	SourceKey string `json:"source_key"`
}

type DocumentEventsV1 struct {
	VaultUID         string                      `json:"vault_uid"`
	ContentVersionID string                      `json:"content_version_id"`
	ContractVersion  string                      `json:"contract_version"`
	Diagnostics      []DocumentEventDiagnosticV1 `json:"diagnostics"`
	DocumentKind     DocumentKind                `json:"document_kind"`
	Events           []DocumentEventV1           `json:"events"`
	Primaries        []DocumentEventPrimaryV1    `json:"primaries"`
	Sources          []DocumentEventSourceV1     `json:"sources"`
}
