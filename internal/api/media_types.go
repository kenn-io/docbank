package api

type MediaAcquisitionPlan struct {
	PlanToken       string   `json:"plan_token"`
	PlanFingerprint string   `json:"plan_fingerprint"`
	OriginID        string   `json:"origin_id"`
	Provider        string   `json:"provider,omitempty"`
	InputClasses    []string `json:"input_classes"`
	RetainedClasses []string `json:"retained_classes"`
	GrantState      string   `json:"grant_state"`
}

type MediaAcquisitionGrantBody struct {
	OperationID string `json:"operation_id"`
	PlanToken   string `json:"plan_token" maxLength:"16384"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

type MediaAcquisitionRevokeBody struct {
	OperationID string `json:"operation_id"`
	OriginID    string `json:"origin_id"`
}

type MediaReferenceBody struct {
	OperationID       string               `json:"operation_id"`
	ReferenceURL      string               `json:"reference_url" maxLength:"8192" writeOnly:"true"`
	ProviderHint      string               `json:"provider_hint,omitempty"`
	CredentialBinding string               `json:"credential_binding,omitempty"`
	Acquire           bool                 `json:"acquire"`
	Occurrence        MediaOccurrenceBody  `json:"occurrence"`
	Processing        *MediaProcessingBody `json:"processing,omitempty"`
}

type MediaSuppliedMetadata struct {
	OperationID              string               `json:"operation_id"`
	Filename                 string               `json:"filename"`
	MediaType                string               `json:"media_type"`
	SHA256                   string               `json:"sha256"`
	ByteLength               int64                `json:"byte_length"`
	ExistingContentVersionID string               `json:"existing_content_version_id,omitempty"`
	Occurrence               MediaOccurrenceBody  `json:"occurrence"`
	Processing               *MediaProcessingBody `json:"processing,omitempty"`
}

type MediaTimestamp struct {
	Normalized     string `json:"normalized"`
	Raw            string `json:"raw"`
	Precision      string `json:"precision"`
	Timezone       string `json:"timezone"`
	ZoneText       string `json:"zone_text,omitempty"`
	OffsetSeconds  *int   `json:"offset_seconds,omitempty"`
	FractionDigits int    `json:"fraction_digits"`
}

type MediaOccurrenceBody struct {
	Ref          string         `json:"ref"`
	Revision     string         `json:"revision"`
	Filename     string         `json:"filename"`
	PersonRef    string         `json:"person_ref,omitempty"`
	SpeakerLabel string         `json:"speaker_label,omitempty"`
	Message      MediaTimestamp `json:"message"`
}

type MediaProcessingBody struct {
	Profile         string `json:"profile"`
	SuppliedInputID string `json:"supplied_input_id,omitempty"`
}

type MediaReceipt struct {
	VaultUID         string `json:"vault_uid"`
	SourceID         string `json:"source_id"`
	SourceVersionID  string `json:"source_version_id,omitempty"`
	ContentVersionID string `json:"content_version_id,omitempty"`
	OccurrenceID     string `json:"occurrence_id,omitempty"`
	OperationID      string `json:"operation_id"`
	JobID            string `json:"job_id,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	OperationState   string `json:"operation_state"`
	CoverageState    string `json:"coverage_state"`
	SuppliedInputID  string `json:"supplied_input_id,omitempty"`
}

type MediaSourceRow struct {
	SourceID         string `json:"source_id"`
	SourceVersionID  string `json:"source_version_id,omitempty"`
	ContentVersionID string `json:"content_version_id,omitempty"`
	Filename         string `json:"filename"`
	CaptureLabel     string `json:"capture_label"`
	Outcome          string `json:"outcome"`
	CoverageState    string `json:"coverage_state"`
	Excerpt          string `json:"excerpt,omitempty"`
}

type MediaSourcePage struct {
	Items      []MediaSourceRow `json:"items"`
	Total      int              `json:"total"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type MediaOccurrenceRow struct {
	OccurrenceID    string         `json:"occurrence_id"`
	SourceID        string         `json:"source_id"`
	SourceVersionID string         `json:"source_version_id,omitempty"`
	Ref             string         `json:"ref"`
	Revision        string         `json:"revision"`
	Filename        string         `json:"filename"`
	PersonRef       string         `json:"person_ref,omitempty"`
	SpeakerLabel    string         `json:"speaker_label,omitempty"`
	Message         MediaTimestamp `json:"message"`
}

type MediaOccurrencePage struct {
	Items      []MediaOccurrenceRow `json:"items"`
	Total      int                  `json:"total"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type MediaOrigin struct {
	OriginID             string `json:"origin_id"`
	Provider             string `json:"provider"`
	AcquisitionAvailable bool   `json:"acquisition_available"`
}

type MediaOriginPage struct {
	Items []MediaOrigin `json:"items"`
}

type MediaRetryBody struct {
	OperationID string               `json:"operation_id"`
	Processing  *MediaProcessingBody `json:"processing,omitempty"`
}

type MediaOccurrenceMutationBody struct {
	OperationID string              `json:"operation_id"`
	SourceID    string              `json:"source_id"`
	Occurrence  MediaOccurrenceBody `json:"occurrence"`
}

type MediaOccurrenceRevokeBody struct {
	OperationID string `json:"operation_id"`
	Revision    string `json:"revision"`
}

type MediaArtifactMetadata struct {
	OperationID  string               `json:"operation_id"`
	OccurrenceID string               `json:"occurrence_id"`
	Kind         string               `json:"kind"`
	Origin       string               `json:"origin,omitempty"`
	Provider     string               `json:"provider,omitempty"`
	Language     string               `json:"language,omitempty"`
	Filename     string               `json:"filename"`
	MediaType    string               `json:"media_type"`
	SHA256       string               `json:"sha256"`
	ByteLength   int64                `json:"byte_length"`
	Processing   *MediaProcessingBody `json:"processing,omitempty"`
}

type MediaConsentReceipt struct {
	OperationID string `json:"operation_id"`
	OriginID    string `json:"origin_id"`
	GrantID     string `json:"grant_id,omitempty"`
	Fence       int64  `json:"fence"`
	RevokedAt   string `json:"revoked_at,omitempty"`
}
