// Package transfer defines the SQL-free Msgvault transfer wire contract.
package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"slices"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	FormatV1           = "msgvault-transfer/1"
	ContractRevision   = 1
	PersonUIDKindVCard = "vcard_uid"
)

type RecordType string

const (
	RecordTypePerson       RecordType = "person"
	RecordTypeSource       RecordType = "source"
	RecordTypeConversation RecordType = "conversation"
	RecordTypeRecord       RecordType = "record"
	RecordTypeCoverage     RecordType = "coverage"
	RecordTypeTombstone    RecordType = "tombstone"
	RecordTypeComplete     RecordType = "complete"
)

type Kind string

const (
	KindEmail                Kind = "email"
	KindChatMessage          Kind = "chat_message"
	KindConversation         Kind = "conversation"
	KindCall                 Kind = "call"
	KindVoicemail            Kind = "voicemail"
	KindCalendarEvent        Kind = "calendar_event"
	KindMeetingNote          Kind = "meeting_note"
	KindTranscript           Kind = "transcript"
	KindAttachmentOccurrence Kind = "attachment_occurrence"
)

type SourceType string

const (
	SourceTypeGmail          SourceType = "gmail"
	SourceTypeIMAP           SourceType = "imap"
	SourceTypeMBOX           SourceType = "mbox"
	SourceTypeEMLX           SourceType = "emlx"
	SourceTypePST            SourceType = "pst"
	SourceTypeIMessage       SourceType = "imessage"
	SourceTypeWhatsApp       SourceType = "whatsapp"
	SourceTypeMessenger      SourceType = "messenger"
	SourceTypeGoogleVoice    SourceType = "google_voice"
	SourceTypeSyncTech       SourceType = "synctech"
	SourceTypeTeams          SourceType = "teams"
	SourceTypeSlack          SourceType = "slack"
	SourceTypeDiscord        SourceType = "discord"
	SourceTypeBeeper         SourceType = "beeper"
	SourceTypeGoogleCalendar SourceType = "google_calendar"
	SourceTypeGranola        SourceType = "granola"
	SourceTypeCircleback     SourceType = "circleback"
	SourceTypeOther          SourceType = "other"
)

type DateKind string

const (
	DateKindMessageSent        DateKind = "message_sent"
	DateKindMessageReceived    DateKind = "message_received"
	DateKindMessageDelivered   DateKind = "message_delivered"
	DateKindMessageRead        DateKind = "message_read"
	DateKindCallStarted        DateKind = "call_started"
	DateKindCallEnded          DateKind = "call_ended"
	DateKindVoicemailLeft      DateKind = "voicemail_left"
	DateKindEventStart         DateKind = "event_start"
	DateKindEventEnd           DateKind = "event_end"
	DateKindMeetingStart       DateKind = "meeting_start"
	DateKindMeetingEnd         DateKind = "meeting_end"
	DateKindNoteRecorded       DateKind = "note_recorded"
	DateKindTranscriptRecorded DateKind = "transcript_recorded"
	DateKindAttachmentObserved DateKind = "attachment_observed"
)

type Precision string

const (
	PrecisionDate     Precision = "date"
	PrecisionHour     Precision = "hour"
	PrecisionMinute   Precision = "minute"
	PrecisionSecond   Precision = "second"
	PrecisionFraction Precision = "fraction"
)

type TimezoneKind string

const (
	TimezoneKindOmitted TimezoneKind = "omitted"
	TimezoneKindUTC     TimezoneKind = "utc"
	TimezoneKindOffset  TimezoneKind = "offset"
	TimezoneKindNamed   TimezoneKind = "named"
	TimezoneKindInvalid TimezoneKind = "invalid"
)

type Origin string

const (
	OriginSentAt       Origin = "sent_at"
	OriginReceivedAt   Origin = "received_at"
	OriginDeliveredAt  Origin = "delivered_at"
	OriginReadAt       Origin = "read_at"
	OriginInternalDate Origin = "internal_date"
	OriginHeaderDate   Origin = "header_date"
	OriginStartTime    Origin = "start_time"
	OriginEndTime      Origin = "end_time"
	OriginCreatedAt    Origin = "created_at"
	OriginModifiedAt   Origin = "modified_at"
	OriginRecordedAt   Origin = "recorded_at"
	OriginObservedAt   Origin = "observed_at"
	OriginUnspecified  Origin = "unspecified"
)

type EnvelopeRole string

const (
	EnvelopeRoleFrom      EnvelopeRole = "from"
	EnvelopeRoleTo        EnvelopeRole = "to"
	EnvelopeRoleCc        EnvelopeRole = "cc"
	EnvelopeRoleBcc       EnvelopeRole = "bcc"
	EnvelopeRoleMention   EnvelopeRole = "mention"
	EnvelopeRoleMember    EnvelopeRole = "member"
	EnvelopeRoleOrganizer EnvelopeRole = "organizer"
	EnvelopeRoleAttendee  EnvelopeRole = "attendee"
)

type ScopeDirection string

const (
	ScopeDirectionFromPerson ScopeDirection = "from_person"
	ScopeDirectionToPerson   ScopeDirection = "to_person"
	ScopeDirectionGroup      ScopeDirection = "group"
)

type Direction string

const (
	DirectionInbound  Direction = "inbound"
	DirectionOutbound Direction = "outbound"
	DirectionObserved Direction = "observed"
)

type RecordOrigin string

const (
	RecordOriginProviderOriginal  RecordOrigin = "provider_original"
	RecordOriginProducerCanonical RecordOrigin = "producer_canonical"
)

type Capability string

const (
	CapabilityAcquisition     Capability = "acquisition"
	CapabilityTransfer        Capability = "transfer"
	CapabilityRaw             Capability = "raw"
	CapabilityAttachmentBytes Capability = "attachment_bytes"
	CapabilityPeople          Capability = "people"
	CapabilityHistory         Capability = "history"
	CapabilityTextIndexing    Capability = "text_indexing"
	CapabilityReview          Capability = "review"
	CapabilityReport          Capability = "report"
	CapabilityExport          Capability = "export"
)

type CapabilityState string

const (
	CapabilityStateAvailable     CapabilityState = "available"
	CapabilityStatePartial       CapabilityState = "partial"
	CapabilityStateUnavailable   CapabilityState = "unavailable"
	CapabilityStateNotApplicable CapabilityState = "not_applicable"
	CapabilityStateUnsupported   CapabilityState = "unsupported"
)

type ReasonCode string

const (
	ReasonNotSelected            ReasonCode = "not_selected"
	ReasonConsentWithheld        ReasonCode = "consent_withheld"
	ReasonBytesTooLarge          ReasonCode = "bytes_too_large"
	ReasonSourceMissing          ReasonCode = "source_missing"
	ReasonSourceUnreadable       ReasonCode = "source_unreadable"
	ReasonProviderURLOnly        ReasonCode = "provider_url_only"
	ReasonDigestMismatch         ReasonCode = "digest_mismatch"
	ReasonExportFailed           ReasonCode = "export_failed"
	ReasonWindowTruncated        ReasonCode = "window_truncated"
	ReasonWatermarkExceeded      ReasonCode = "watermark_exceeded"
	ReasonUnsupportedRecordKind  ReasonCode = "unsupported_record_kind"
	ReasonUnsupportedEnumValue   ReasonCode = "unsupported_enum_value"
	ReasonUnknownField           ReasonCode = "unknown_field"
	ReasonKindNotYetSupported    ReasonCode = "kind_not_yet_supported"
	ReasonFormatV1NoAttachments  ReasonCode = "format_v1_no_attachments"
	ReasonFormatV1NoRaw          ReasonCode = "format_v1_no_raw"
	ReasonFormatV1NoPersonUID    ReasonCode = "format_v1_no_person_uid"
	ReasonFormatV1NoHistory      ReasonCode = "format_v1_no_history"
	ReasonLegacyPrecisionUnknown ReasonCode = "legacy_precision_unknown"
	ReasonNotProcessed           ReasonCode = "not_processed"
	ReasonNotQualified           ReasonCode = "not_qualified"
	ReasonTruncatedExport        ReasonCode = "truncated_export"
	ReasonAliasConflict          ReasonCode = "alias_conflict"
)

type Availability string

const (
	AvailabilityBytesIncluded Availability = "bytes_included"
	AvailabilityBytesOmitted  Availability = "bytes_omitted"
	AvailabilityMetadataOnly  Availability = "metadata_only"
	AvailabilityURLOnly       Availability = "url_only"
	AvailabilityMissingBlob   Availability = "missing_blob"
	AvailabilitySkipped       Availability = "skipped"
	AvailabilityFailed        Availability = "failed"
)

type AttachmentRole string

const (
	AttachmentRoleStandalone AttachmentRole = "standalone"
	AttachmentRoleInline     AttachmentRole = "inline"
	AttachmentRoleAvatar     AttachmentRole = "avatar"
	AttachmentRoleThumbnail  AttachmentRole = "thumbnail"
	AttachmentRolePreview    AttachmentRole = "preview"
	AttachmentRoleSticker    AttachmentRole = "sticker"
	AttachmentRoleUIAsset    AttachmentRole = "ui_asset"
	AttachmentRoleUnknown    AttachmentRole = "unknown"
)

type ContactPointKind string

const (
	ContactPointKindEmail    ContactPointKind = "email"
	ContactPointKindPhone    ContactPointKind = "phone"
	ContactPointKindAppleID  ContactPointKind = "apple_id"
	ContactPointKindWhatsApp ContactPointKind = "whatsapp"
	ContactPointKindHandle   ContactPointKind = "handle"
	ContactPointKindURI      ContactPointKind = "uri"
)

type CustodianRank string

const (
	CustodianRankPrimary    CustodianRank = "primary"
	CustodianRankAdditional CustodianRank = "additional"
)

type HistoryState string

const (
	HistoryStateObserved    HistoryState = "observed"
	HistoryStateAbsent      HistoryState = "absent"
	HistoryStateUnsupported HistoryState = "unsupported"
)

var (
	recordTypes       = []RecordType{RecordTypePerson, RecordTypeSource, RecordTypeConversation, RecordTypeRecord, RecordTypeCoverage, RecordTypeTombstone, RecordTypeComplete}
	kinds             = []Kind{KindEmail, KindChatMessage, KindConversation, KindCall, KindVoicemail, KindCalendarEvent, KindMeetingNote, KindTranscript, KindAttachmentOccurrence}
	sourceTypes       = []SourceType{SourceTypeGmail, SourceTypeIMAP, SourceTypeMBOX, SourceTypeEMLX, SourceTypePST, SourceTypeIMessage, SourceTypeWhatsApp, SourceTypeMessenger, SourceTypeGoogleVoice, SourceTypeSyncTech, SourceTypeTeams, SourceTypeSlack, SourceTypeDiscord, SourceTypeBeeper, SourceTypeGoogleCalendar, SourceTypeGranola, SourceTypeCircleback, SourceTypeOther}
	dateKinds         = []DateKind{DateKindMessageSent, DateKindMessageReceived, DateKindMessageDelivered, DateKindMessageRead, DateKindCallStarted, DateKindCallEnded, DateKindVoicemailLeft, DateKindEventStart, DateKindEventEnd, DateKindMeetingStart, DateKindMeetingEnd, DateKindNoteRecorded, DateKindTranscriptRecorded, DateKindAttachmentObserved}
	precisions        = []Precision{PrecisionDate, PrecisionHour, PrecisionMinute, PrecisionSecond, PrecisionFraction}
	timezoneKinds     = []TimezoneKind{TimezoneKindOmitted, TimezoneKindUTC, TimezoneKindOffset, TimezoneKindNamed, TimezoneKindInvalid}
	origins           = []Origin{OriginSentAt, OriginReceivedAt, OriginDeliveredAt, OriginReadAt, OriginInternalDate, OriginHeaderDate, OriginStartTime, OriginEndTime, OriginCreatedAt, OriginModifiedAt, OriginRecordedAt, OriginObservedAt, OriginUnspecified}
	envelopeRoles     = []EnvelopeRole{EnvelopeRoleFrom, EnvelopeRoleTo, EnvelopeRoleCc, EnvelopeRoleBcc, EnvelopeRoleMention, EnvelopeRoleMember, EnvelopeRoleOrganizer, EnvelopeRoleAttendee}
	scopeDirections   = []ScopeDirection{ScopeDirectionFromPerson, ScopeDirectionToPerson, ScopeDirectionGroup}
	directions        = []Direction{DirectionInbound, DirectionOutbound, DirectionObserved}
	recordOrigins     = []RecordOrigin{RecordOriginProviderOriginal, RecordOriginProducerCanonical}
	capabilities      = []Capability{CapabilityAcquisition, CapabilityTransfer, CapabilityRaw, CapabilityAttachmentBytes, CapabilityPeople, CapabilityHistory, CapabilityTextIndexing, CapabilityReview, CapabilityReport, CapabilityExport}
	capabilityStates  = []CapabilityState{CapabilityStateAvailable, CapabilityStatePartial, CapabilityStateUnavailable, CapabilityStateNotApplicable, CapabilityStateUnsupported}
	reasonCodes       = []ReasonCode{ReasonNotSelected, ReasonConsentWithheld, ReasonBytesTooLarge, ReasonSourceMissing, ReasonSourceUnreadable, ReasonProviderURLOnly, ReasonDigestMismatch, ReasonExportFailed, ReasonWindowTruncated, ReasonWatermarkExceeded, ReasonUnsupportedRecordKind, ReasonUnsupportedEnumValue, ReasonUnknownField, ReasonKindNotYetSupported, ReasonFormatV1NoAttachments, ReasonFormatV1NoRaw, ReasonFormatV1NoPersonUID, ReasonFormatV1NoHistory, ReasonLegacyPrecisionUnknown, ReasonNotProcessed, ReasonNotQualified, ReasonTruncatedExport, ReasonAliasConflict}
	availabilities    = []Availability{AvailabilityBytesIncluded, AvailabilityBytesOmitted, AvailabilityMetadataOnly, AvailabilityURLOnly, AvailabilityMissingBlob, AvailabilitySkipped, AvailabilityFailed}
	attachmentRoles   = []AttachmentRole{AttachmentRoleStandalone, AttachmentRoleInline, AttachmentRoleAvatar, AttachmentRoleThumbnail, AttachmentRolePreview, AttachmentRoleSticker, AttachmentRoleUIAsset, AttachmentRoleUnknown}
	contactPointKinds = []ContactPointKind{ContactPointKindEmail, ContactPointKindPhone, ContactPointKindAppleID, ContactPointKindWhatsApp, ContactPointKindHandle, ContactPointKindURI}
	custodianRanks    = []CustodianRank{CustodianRankPrimary, CustodianRankAdditional}
	historyStates     = []HistoryState{HistoryStateObserved, HistoryStateAbsent, HistoryStateUnsupported}
)

func AllRecordTypes() []RecordType             { return slices.Clone(recordTypes) }
func AllKinds() []Kind                         { return slices.Clone(kinds) }
func AllSourceTypes() []SourceType             { return slices.Clone(sourceTypes) }
func AllDateKinds() []DateKind                 { return slices.Clone(dateKinds) }
func AllPrecisions() []Precision               { return slices.Clone(precisions) }
func AllTimezoneKinds() []TimezoneKind         { return slices.Clone(timezoneKinds) }
func AllOrigins() []Origin                     { return slices.Clone(origins) }
func AllEnvelopeRoles() []EnvelopeRole         { return slices.Clone(envelopeRoles) }
func AllScopeDirections() []ScopeDirection     { return slices.Clone(scopeDirections) }
func AllDirections() []Direction               { return slices.Clone(directions) }
func AllRecordOrigins() []RecordOrigin         { return slices.Clone(recordOrigins) }
func AllCapabilities() []Capability            { return slices.Clone(capabilities) }
func AllCapabilityStates() []CapabilityState   { return slices.Clone(capabilityStates) }
func AllReasonCodes() []ReasonCode             { return slices.Clone(reasonCodes) }
func AllAvailabilities() []Availability        { return slices.Clone(availabilities) }
func AllAttachmentRoles() []AttachmentRole     { return slices.Clone(attachmentRoles) }
func AllContactPointKinds() []ContactPointKind { return slices.Clone(contactPointKinds) }
func AllCustodianRanks() []CustodianRank       { return slices.Clone(custodianRanks) }
func AllHistoryStates() []HistoryState         { return slices.Clone(historyStates) }

func ValidRecordType(v RecordType) bool             { return slices.Contains(recordTypes, v) }
func ValidKind(v Kind) bool                         { return slices.Contains(kinds, v) }
func ValidSourceType(v SourceType) bool             { return slices.Contains(sourceTypes, v) }
func ValidDateKind(v DateKind) bool                 { return slices.Contains(dateKinds, v) }
func ValidPrecision(v Precision) bool               { return slices.Contains(precisions, v) }
func ValidTimezoneKind(v TimezoneKind) bool         { return slices.Contains(timezoneKinds, v) }
func ValidOrigin(v Origin) bool                     { return slices.Contains(origins, v) }
func ValidEnvelopeRole(v EnvelopeRole) bool         { return slices.Contains(envelopeRoles, v) }
func ValidScopeDirection(v ScopeDirection) bool     { return slices.Contains(scopeDirections, v) }
func ValidDirection(v Direction) bool               { return slices.Contains(directions, v) }
func ValidRecordOrigin(v RecordOrigin) bool         { return slices.Contains(recordOrigins, v) }
func ValidCapability(v Capability) bool             { return slices.Contains(capabilities, v) }
func ValidCapabilityState(v CapabilityState) bool   { return slices.Contains(capabilityStates, v) }
func ValidReasonCode(v ReasonCode) bool             { return slices.Contains(reasonCodes, v) }
func ValidAvailability(v Availability) bool         { return slices.Contains(availabilities, v) }
func ValidAttachmentRole(v AttachmentRole) bool     { return slices.Contains(attachmentRoles, v) }
func ValidContactPointKind(v ContactPointKind) bool { return slices.Contains(contactPointKinds, v) }
func ValidCustodianRank(v CustodianRank) bool       { return slices.Contains(custodianRanks, v) }
func ValidHistoryState(v HistoryState) bool         { return slices.Contains(historyStates, v) }

type ManifestV1 struct {
	Format         string      `json:"format"`
	PackageID      string      `json:"package_id"`
	ExportSequence string      `json:"export_sequence"`
	Producer       ProducerV1  `json:"producer"`
	Archive        ArchiveV1   `json:"archive"`
	CreatedAt      string      `json:"created_at"`
	Selection      SelectionV1 `json:"selection"`
	Snapshot       SnapshotV1  `json:"snapshot"`
	RecordsSHA256  string      `json:"records_sha256"`
	BlobCount      int64       `json:"blob_count"`
	BlobBytes      int64       `json:"blob_bytes"`
	Counts         CountsV1    `json:"counts"`
}

type ProducerV1 struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	ContractRevision int    `json:"contract_revision"`
}

type ArchiveV1 struct {
	ArchiveID   string `json:"archive_id"`
	System      string `json:"system"`
	DisplayName string `json:"display_name"`
}

type SelectionV1 struct {
	Sources                []string              `json:"sources"`
	Kinds                  []Kind                `json:"kinds"`
	Window                 SelectionWindowV1     `json:"window"`
	IncludeRawSource       bool                  `json:"include_raw_source"`
	IncludeAttachmentBytes bool                  `json:"include_attachment_bytes"`
	IncludePersonRecords   bool                  `json:"include_person_records"`
	PersonFieldPolicy      string                `json:"person_field_policy"`
	Excluded               []SelectionExcludedV1 `json:"excluded"`
}

type SelectionWindowV1 struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type SelectionExcludedV1 struct {
	SourceRef string     `json:"source_ref,omitzero"`
	Kind      Kind       `json:"kind,omitzero"`
	Reason    ReasonCode `json:"reason"`
}

type SnapshotV1 struct {
	Consistency     string `json:"consistency"`
	SpineTimezone   string `json:"spine_timezone"`
	SpineGeneration int64  `json:"spine_generation"`
	HighWatermark   string `json:"high_watermark"`
	ReadStartedAt   string `json:"read_started_at"`
	ReadCompletedAt string `json:"read_completed_at"`
}

type CountsV1 struct {
	Person               int64 `json:"person"`
	Source               int64 `json:"source"`
	Conversation         int64 `json:"conversation"`
	Record               int64 `json:"record"`
	AttachmentOccurrence int64 `json:"attachment_occurrence"`
	Coverage             int64 `json:"coverage"`
	Tombstone            int64 `json:"tombstone"`
}

type RecordV1 struct {
	RecordType        RecordType              `json:"record_type"`
	RecordRef         string                  `json:"record_ref"`
	LegacyRef         string                  `json:"legacy_ref,omitzero"`
	Kind              Kind                    `json:"kind"`
	SourceRef         string                  `json:"source_ref"`
	ConversationRef   string                  `json:"conversation_ref,omitzero"`
	ParentRecordRef   string                  `json:"parent_record_ref,omitzero"`
	Revision          string                  `json:"revision,omitzero"`
	Dates             []DateV1                `json:"dates,omitzero"`
	Direction         Direction               `json:"direction,omitzero"`
	Participants      []ParticipantV1         `json:"participants,omitzero"`
	Custodian         *CustodianV1            `json:"custodian,omitzero"`
	Subject           string                  `json:"subject,omitzero"`
	BodyText          string                  `json:"body_text,omitzero"`
	BodyMediaType     string                  `json:"body_media_type,omitzero"`
	BodyTruncated     bool                    `json:"body_truncated,omitzero"`
	Raw               *BlobRefV1              `json:"raw,omitzero"`
	NormalizedSHA256  string                  `json:"normalized_sha256,omitzero"`
	RecordOrigin      RecordOrigin            `json:"record_origin"`
	Attachments       []string                `json:"attachments,omitzero"`
	Attachment        *AttachmentOccurrenceV1 `json:"attachment,omitzero"`
	History           *HistoryV1              `json:"history,omitzero"`
	DeletedFromSource bool                    `json:"deleted_from_source,omitzero"`
	SourceFields      jsontext.Value          `json:"source_fields,omitzero"`
}

type PersonV1 struct {
	RecordType       RecordType       `json:"record_type"`
	PersonRef        string           `json:"person_ref"`
	UIDKind          string           `json:"uid_kind"`
	RetiredUIDs      []string         `json:"retired_uids,omitzero"`
	DisplayName      string           `json:"display_name"`
	PersonLocalID    string           `json:"person_local_id,omitzero"`
	Revision         string           `json:"revision,omitzero"`
	IdentityRevision string           `json:"identity_revision,omitzero"`
	ContactPoints    []ContactPointV1 `json:"contact_points,omitzero"`
	ParticipantRefs  []string         `json:"participant_refs,omitzero"`
	SelectedFields   []string         `json:"selected_fields"`
	WithheldFields   []string         `json:"withheld_fields"`
}

type ContactPointV1 struct {
	Kind            ContactPointKind `json:"kind"`
	ValueNormalized string           `json:"value_normalized"`
	ValueDisplay    string           `json:"value_display"`
	ServiceID       string           `json:"service_id,omitzero"`
	ScopeKind       string           `json:"scope_kind,omitzero"`
	ScopeValue      string           `json:"scope_value,omitzero"`
	Pref            *int             `json:"pref,omitzero"`
	IsPrimary       bool             `json:"is_primary"`
}

type SourceLineV1 struct {
	RecordType    RecordType   `json:"record_type"`
	SourceRef     string       `json:"source_ref"`
	SourceType    SourceType   `json:"source_type"`
	Route         string       `json:"route"`
	Identifier    string       `json:"identifier"`
	SourceTypeRaw string       `json:"source_type_raw,omitzero"`
	DisplayName   string       `json:"display_name,omitzero"`
	Custodian     *CustodianV1 `json:"custodian,omitzero"`
}

type ConversationV1 struct {
	RecordType            RecordType `json:"record_type"`
	SourceRef             string     `json:"source_ref"`
	ConversationRef       string     `json:"conversation_ref"`
	Title                 string     `json:"title"`
	ConversationType      string     `json:"conversation_type"`
	ParentConversationRef string     `json:"parent_conversation_ref,omitzero"`
	ThreadRootRecordRef   string     `json:"thread_root_record_ref,omitzero"`
}

type CustodianV1 struct {
	RawLabel  string        `json:"raw_label"`
	PersonRef string        `json:"person_ref,omitzero"`
	Rank      CustodianRank `json:"rank"`
}

type ParticipantV1 struct {
	PersonRef      string           `json:"person_ref,omitzero"`
	DisplayName    string           `json:"display_name"`
	Address        string           `json:"address,omitzero"`
	AddressKind    ContactPointKind `json:"address_kind,omitzero"`
	EnvelopeRole   EnvelopeRole     `json:"envelope_role"`
	ScopeDirection ScopeDirection   `json:"scope_direction"`
}

type DateV1 struct {
	Kind             DateKind     `json:"kind"`
	Instant          string       `json:"instant,omitzero"`
	Civil            string       `json:"civil,omitzero"`
	Precision        Precision    `json:"precision"`
	FractionDigits   int          `json:"fraction_digits"`
	Timezone         TimezoneKind `json:"timezone"`
	TimezoneName     string       `json:"timezone_name,omitzero"`
	UTCOffsetMinutes *int         `json:"utc_offset_minutes,omitzero"`
	Origin           Origin       `json:"origin"`
	Raw              string       `json:"raw,omitzero"`
	Diagnostics      []string     `json:"diagnostics,omitzero"`
}

type BlobRefV1 struct {
	BlobSHA256 string `json:"blob_sha256"`
	Size       int64  `json:"size"`
	MediaType  string `json:"media_type"`
}

type AttachmentOccurrenceV1 struct {
	PartKey            string         `json:"part_key"`
	Filename           string         `json:"filename"`
	MediaType          string         `json:"media_type"`
	Size               int64          `json:"size"`
	ContentSHA256      string         `json:"content_sha256"`
	Role               AttachmentRole `json:"role"`
	Blob               *BlobRefV1     `json:"blob"`
	Availability       Availability   `json:"availability"`
	AvailabilityReason ReasonCode     `json:"availability_reason,omitzero"`
}

type HistoryV1 struct {
	Edits     HistoryState `json:"edits"`
	Reactions HistoryState `json:"reactions"`
	Deletions HistoryState `json:"deletions"`
}

type CoverageV1 struct {
	RecordType               RecordType          `json:"record_type"`
	SourceRef                string              `json:"source_ref"`
	Route                    string              `json:"route"`
	Kind                     Kind                `json:"kind"`
	SelectionDigest          string              `json:"selection_digest"`
	Selected                 int64               `json:"selected"`
	Emitted                  int64               `json:"emitted"`
	Skipped                  int64               `json:"skipped"`
	RawIncluded              int64               `json:"raw_included"`
	RawAbsent                int64               `json:"raw_absent"`
	AttachmentsSelected      int64               `json:"attachments_selected"`
	AttachmentsBytesIncluded int64               `json:"attachments_bytes_included"`
	AttachmentsOmitted       int64               `json:"attachments_omitted"`
	Reasons                  []ReasonCountV1     `json:"reasons"`
	Capabilities             []CapabilityEntryV1 `json:"capabilities"`
}

type CapabilityEntryV1 struct {
	Capability Capability      `json:"capability"`
	State      CapabilityState `json:"state"`
	Reason     ReasonCode      `json:"reason,omitzero"`
}

type ReasonCountV1 struct {
	Code  ReasonCode `json:"code"`
	Count int64      `json:"count"`
}

type TombstoneV1 struct {
	RecordType RecordType `json:"record_type"`
	SourceRef  string     `json:"source_ref"`
	Kind       Kind       `json:"kind"`
	RecordRef  string     `json:"record_ref"`
	Reason     string     `json:"reason"`
	ObservedAt string     `json:"observed_at"`
}

type CompleteV1 struct {
	RecordType    RecordType     `json:"record_type"`
	Counts        CountsV1       `json:"counts"`
	RecordsSHA256 string         `json:"records_sha256"`
	Truncated     bool           `json:"truncated"`
	Continuation  ContinuationV1 `json:"continuation,omitzero"`
}

type ContinuationV1 string

func RecordKey(sourceRef string, kind Kind, recordRef string) (string, error) {
	if sourceRef == "" || recordRef == "" || !ValidKind(kind) {
		return "", errors.New("transfer: invalid record identity")
	}
	if len(sourceRef) > MaxSourceRefBytes || len(recordRef) > MaxRecordRefBytes {
		return "", errors.New("transfer: identity exceeds 512 bytes")
	}
	raw, err := canonical.Marshal([]string{sourceRef, string(kind), recordRef})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
