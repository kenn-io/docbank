package document

const (
	EmailContractV1       = "docbank-email/v1"
	EmailRecipeContractV1 = "docbank-email-decoder/v1"
)

type EmailOutcome string
type EmailVerification string
type EmailInventoryState string
type EmailDecodeState string
type EmailProtection string
type EmailArtifactRole string
type EmailHeaderState string
type EmailInterpretationState string
type EmailDateState string
type EmailTimezoneState string
type EmailBodyKind string
type EmailDisplayState string
type EmailResourceState string
type EmailOperation string
type EmailDiagnosticCode string

const (
	EmailOutcomeDecoded             EmailOutcome             = "decoded"
	EmailOutcomeUnavailable         EmailOutcome             = "unavailable"
	EmailVerificationVerified       EmailVerification        = "verified"
	EmailVerificationCatalogOnly    EmailVerification        = "catalog_only"
	EmailInventoryComplete          EmailInventoryState      = "complete"
	EmailInventoryPartial           EmailInventoryState      = "partial"
	EmailDecodeDecoded              EmailDecodeState         = "decoded"
	EmailDecodeUnsupported          EmailDecodeState         = "unsupported"
	EmailDecodeFailed               EmailDecodeState         = "failed"
	EmailProtectionNone             EmailProtection          = "none"
	EmailProtectionSignedUnverified EmailProtection          = "signed_unverified"
	EmailProtectionEncrypted        EmailProtection          = "encrypted"
	EmailArtifactDecodedPayload     EmailArtifactRole        = "decoded_payload"
	EmailArtifactBodyUTF8           EmailArtifactRole        = "body_utf8"
	EmailArtifactRawHeaders         EmailArtifactRole        = "raw_headers"
	EmailHeaderValid                EmailHeaderState         = "valid"
	EmailHeaderMalformed            EmailHeaderState         = "malformed"
	EmailInterpretationMissing      EmailInterpretationState = "missing"
	EmailInterpretationDecoded      EmailInterpretationState = "decoded"
	EmailInterpretationInvalid      EmailInterpretationState = "invalid"
	EmailInterpretationUnsupported  EmailInterpretationState = "unsupported"
	EmailDateMissing                EmailDateState           = "missing"
	EmailDateInvalid                EmailDateState           = "invalid"
	EmailDateParsed                 EmailDateState           = "parsed"
	EmailTimezoneMissing            EmailTimezoneState       = "missing"
	EmailTimezoneNumeric            EmailTimezoneState       = "numeric"
	EmailTimezoneKnownNamed         EmailTimezoneState       = "known_named"
	EmailTimezoneUnknownNamed       EmailTimezoneState       = "unknown_named"
	EmailTimezoneInvalid            EmailTimezoneState       = "invalid"
	EmailBodyHTML                   EmailBodyKind            = "html"
	EmailBodyPlain                  EmailBodyKind            = "plain"
	EmailDisplayAvailable           EmailDisplayState        = "available"
	EmailDisplayUnsupported         EmailDisplayState        = "unsupported"
	EmailDisplayFailed              EmailDisplayState        = "failed"
	EmailDisplayTooLarge            EmailDisplayState        = "too_large"
	EmailResourceUnique             EmailResourceState       = "unique"
	EmailResourceMissing            EmailResourceState       = "missing"
	EmailResourceAmbiguous          EmailResourceState       = "ambiguous"

	EmailOperationSource        EmailOperation = "source"
	EmailOperationHeaders       EmailOperation = "headers"
	EmailOperationStructure     EmailOperation = "structure"
	EmailOperationTransfer      EmailOperation = "transfer"
	EmailOperationCharset       EmailOperation = "charset"
	EmailOperationHeaderDisplay EmailOperation = "header_display"
	EmailOperationFilename      EmailOperation = "filename"
	EmailOperationDate          EmailOperation = "date"
	EmailOperationBodySelection EmailOperation = "body_selection"
	EmailOperationCID           EmailOperation = "cid"
	EmailOperationInventory     EmailOperation = "inventory"

	EmailDiagnosticSourceSizeLimit              EmailDiagnosticCode = "source_size_limit"
	EmailDiagnosticSourceUnsupported            EmailDiagnosticCode = "source_unsupported"
	EmailDiagnosticHeaderBytesLimit             EmailDiagnosticCode = "header_bytes_limit"
	EmailDiagnosticHeaderTotalBytesLimit        EmailDiagnosticCode = "header_total_bytes_limit"
	EmailDiagnosticHeaderFieldsLimit            EmailDiagnosticCode = "header_fields_limit"
	EmailDiagnosticHeaderTotalFieldsLimit       EmailDiagnosticCode = "header_total_fields_limit"
	EmailDiagnosticMalformedHeader              EmailDiagnosticCode = "malformed_header"
	EmailDiagnosticMissingHeader                EmailDiagnosticCode = "missing_header"
	EmailDiagnosticInvalidHeader                EmailDiagnosticCode = "invalid_header"
	EmailDiagnosticDuplicateHeader              EmailDiagnosticCode = "duplicate_header"
	EmailDiagnosticHeaderDisplayLimit           EmailDiagnosticCode = "header_display_limit"
	EmailDiagnosticEncodedWordInvalid           EmailDiagnosticCode = "encoded_word_invalid"
	EmailDiagnosticCharsetMissing               EmailDiagnosticCode = "charset_missing"
	EmailDiagnosticCharsetUnsupported           EmailDiagnosticCode = "charset_unsupported"
	EmailDiagnosticCharsetInvalid               EmailDiagnosticCode = "charset_invalid"
	EmailDiagnosticCharsetReplacement           EmailDiagnosticCode = "charset_replacement"
	EmailDiagnosticFilenameInvalid              EmailDiagnosticCode = "filename_invalid"
	EmailDiagnosticFilenameUnsupported          EmailDiagnosticCode = "filename_unsupported"
	EmailDiagnosticContentIDInvalid             EmailDiagnosticCode = "content_id_invalid"
	EmailDiagnosticContentIDAmbiguous           EmailDiagnosticCode = "content_id_ambiguous"
	EmailDiagnosticBoundaryMissing              EmailDiagnosticCode = "boundary_missing"
	EmailDiagnosticBoundaryInvalid              EmailDiagnosticCode = "boundary_invalid"
	EmailDiagnosticBoundaryUnclosed             EmailDiagnosticCode = "boundary_unclosed"
	EmailDiagnosticTransferUnsupported          EmailDiagnosticCode = "transfer_unsupported"
	EmailDiagnosticTransferInvalid              EmailDiagnosticCode = "transfer_invalid"
	EmailDiagnosticPartBytesLimit               EmailDiagnosticCode = "part_bytes_limit"
	EmailDiagnosticDecodedBytesLimit            EmailDiagnosticCode = "decoded_bytes_limit"
	EmailDiagnosticPartCountLimit               EmailDiagnosticCode = "part_count_limit"
	EmailDiagnosticDepthLimit                   EmailDiagnosticCode = "depth_limit"
	EmailDiagnosticBodyUTF8Limit                EmailDiagnosticCode = "body_utf8_limit"
	EmailDiagnosticBodyUTF8TotalLimit           EmailDiagnosticCode = "body_utf8_total_limit"
	EmailDiagnosticHTMLDisplayLimit             EmailDiagnosticCode = "html_display_limit"
	EmailDiagnosticInventoryMetadataLimit       EmailDiagnosticCode = "inventory_metadata_limit"
	EmailDiagnosticLimit                        EmailDiagnosticCode = "diagnostic_limit"
	EmailDiagnosticDateMissing                  EmailDiagnosticCode = "date_missing"
	EmailDiagnosticDateInvalid                  EmailDiagnosticCode = "date_invalid"
	EmailDiagnosticDateAmbiguous                EmailDiagnosticCode = "date_ambiguous"
	EmailDiagnosticTimezoneUnknown              EmailDiagnosticCode = "timezone_unknown"
	EmailDiagnosticTimezoneOriginUnknown        EmailDiagnosticCode = "timezone_origin_unknown"
	EmailDiagnosticLeapSecondInstantUnavailable EmailDiagnosticCode = "leap_second_instant_unavailable"
	EmailDiagnosticBodyUnavailable              EmailDiagnosticCode = "body_unavailable"
	EmailDiagnosticCIDMissing                   EmailDiagnosticCode = "cid_missing"
	EmailDiagnosticCIDAmbiguous                 EmailDiagnosticCode = "cid_ambiguous"
	EmailDiagnosticSignatureUnverified          EmailDiagnosticCode = "signature_unverified"
	EmailDiagnosticEncryptedUnavailable         EmailDiagnosticCode = "encrypted_unavailable"
)

// EmailArtifactRoles returns the closed canonical role set in stable order.
func EmailArtifactRoles() [3]EmailArtifactRole {
	return [3]EmailArtifactRole{
		EmailArtifactRawHeaders,
		EmailArtifactDecodedPayload,
		EmailArtifactBodyUTF8,
	}
}

// IsEmailArtifactRole reports whether role belongs to the canonical role set.
func IsEmailArtifactRole(role string) bool {
	for _, candidate := range EmailArtifactRoles() {
		if role == string(candidate) {
			return true
		}
	}
	return false
}

type EmailV1 struct {
	ContractVersion string            `json:"contract_version"`
	Source          EmailSourceV1     `json:"source"`
	Recipe          EmailRecipeV1     `json:"recipe"`
	Outcome         EmailOutcome      `json:"outcome"`
	Inventory       *EmailInventoryV1 `json:"inventory"`
	Failure         *EmailFailureV1   `json:"failure"`
}
type EmailSourceV1 struct {
	SHA256       string            `json:"sha256"`
	Size         int64             `json:"size"`
	Verification EmailVerification `json:"verification"`
}
type EmailRecipeV1 struct {
	ContractVersion        string        `json:"contract_version"`
	ImplementationRevision int           `json:"implementation_revision"`
	GoVersion              string        `json:"go_version"`
	CharsetProfile         string        `json:"charset_profile"`
	HeaderProfile          string        `json:"header_profile"`
	FilenameProfile        string        `json:"filename_profile"`
	BodyProfile            string        `json:"body_profile"`
	Limits                 EmailLimitsV1 `json:"limits"`
}
type EmailLimitsV1 struct {
	SourceBytes            int64 `json:"source_bytes"`
	PartBytes              int64 `json:"part_bytes"`
	DecodedBytes           int64 `json:"decoded_bytes"`
	Parts                  int   `json:"parts"`
	Depth                  int   `json:"depth"`
	HeaderBytes            int64 `json:"header_bytes"`
	AggregateHeaderBytes   int64 `json:"aggregate_header_bytes"`
	HeaderFields           int   `json:"header_fields"`
	AggregateHeaderFields  int   `json:"aggregate_header_fields"`
	BodyUTF8Bytes          int64 `json:"body_utf8_bytes"`
	AggregateBodyUTF8Bytes int64 `json:"aggregate_body_utf8_bytes"`
	HTMLDisplayBytes       int64 `json:"html_display_bytes"`
	InventoryBytes         int64 `json:"inventory_bytes"`
	Diagnostics            int   `json:"diagnostics"`
	DiagnosticDetailBytes  int   `json:"diagnostic_detail_bytes"`
	HeaderDisplayBytes     int64 `json:"header_display_bytes"`
}
type EmailFailureV1 struct {
	Code      EmailDiagnosticCode `json:"code"`
	Operation EmailOperation      `json:"operation"`
	Path      *string             `json:"path"`
	Limit     *int64              `json:"limit"`
	Observed  *int64              `json:"observed"`
	Detail    string              `json:"detail"`
}
type EmailTerminationV1 struct {
	Code      EmailDiagnosticCode `json:"code"`
	Operation EmailOperation      `json:"operation"`
	Path      *string             `json:"path"`
	Limit     *int64              `json:"limit"`
	Observed  *int64              `json:"observed"`
}
type EmailInventoryV1 struct {
	State       EmailInventoryState `json:"state"`
	RootPath    *string             `json:"root_path"`
	Parts       []EmailPartV1       `json:"parts"`
	Messages    []EmailMessageV1    `json:"messages"`
	Diagnostics []EmailDiagnosticV1 `json:"diagnostics"`
	Termination *EmailTerminationV1 `json:"termination"`
}
type EmailPartV1 struct {
	Path             string              `json:"path"`
	ParentPath       *string             `json:"parent_path"`
	SiblingOrder     int                 `json:"sibling_order"`
	Depth            int                 `json:"depth"`
	MessagePath      string              `json:"message_path"`
	Media            EmailMediaV1        `json:"media"`
	HeaderBlock      *EmailArtifactRefV1 `json:"header_block"`
	Headers          []EmailHeaderV1     `json:"headers"`
	Disposition      *string             `json:"disposition"`
	Filename         EmailFilenameV1     `json:"filename"`
	ContentID        EmailContentIDV1    `json:"content_id"`
	TransferEncoding *string             `json:"transfer_encoding"`
	DecodeState      EmailDecodeState    `json:"decode_state"`
	Protection       EmailProtection     `json:"protection"`
	Payload          *EmailArtifactRefV1 `json:"payload"`
	BodyUTF8         *EmailArtifactRefV1 `json:"body_utf8"`
	Diagnostics      []EmailDiagnosticV1 `json:"diagnostics"`
}

// IsAttachmentLike reports metadata that excludes a part from body selection.
// A filename need not decode successfully to be present;
// only an absent disposition or one valid inline disposition permits a body.
func (part EmailPartV1) IsAttachmentLike() bool {
	if part.Filename.State != EmailInterpretationMissing || part.Disposition != nil && *part.Disposition != "inline" {
		return true
	}
	dispositions := 0
	for _, header := range part.Headers {
		if header.Name != nil && *header.Name == "content-disposition" {
			dispositions++
		}
	}
	return dispositions > 1 || dispositions > 0 && part.Disposition == nil
}

type EmailMediaV1 struct {
	Declared    *string             `json:"declared"`
	Detected    *string             `json:"detected"`
	Diagnostics []EmailDiagnosticV1 `json:"diagnostics"`
}
type EmailArtifactRefV1 struct {
	Role   EmailArtifactRole `json:"role"`
	SHA256 string            `json:"sha256"`
	Size   int64             `json:"size"`
}
type EmailHeaderV1 struct {
	Index  int              `json:"index"`
	Offset int64            `json:"offset"`
	Length int64            `json:"length"`
	Name   *string          `json:"name"`
	State  EmailHeaderState `json:"state"`
}
type EmailFilenameV1 struct {
	Fields   []int                    `json:"fields"`
	Decoded  *string                  `json:"decoded"`
	State    EmailInterpretationState `json:"state"`
	SafeName string                   `json:"safe_name"`
}
type EmailContentIDV1 struct {
	Fields []int                    `json:"fields"`
	Value  *string                  `json:"value"`
	State  EmailInterpretationState `json:"state"`
}
type EmailMessageV1 struct {
	Path             string                `json:"path"`
	Fields           EmailFieldsV1         `json:"fields"`
	Date             EmailDateV1           `json:"date"`
	Alternatives     []EmailAlternativeV1  `json:"alternatives"`
	SelectedBodyPath *string               `json:"selected_body_path"`
	RelatedGroups    []EmailRelatedGroupV1 `json:"related_groups"`
	Diagnostics      []EmailDiagnosticV1   `json:"diagnostics"`
}
type EmailFieldsV1 struct {
	Subject    []EmailDecodedFieldV1 `json:"subject"`
	From       []EmailDecodedFieldV1 `json:"from"`
	To         []EmailDecodedFieldV1 `json:"to"`
	Cc         []EmailDecodedFieldV1 `json:"cc"`
	Bcc        []EmailDecodedFieldV1 `json:"bcc"`
	ReplyTo    []EmailDecodedFieldV1 `json:"reply_to"`
	MessageID  []EmailDecodedFieldV1 `json:"message_id"`
	InReplyTo  []EmailDecodedFieldV1 `json:"in_reply_to"`
	References []EmailDecodedFieldV1 `json:"references"`
}
type EmailDecodedFieldV1 struct {
	HeaderIndex int                      `json:"header_index"`
	State       EmailInterpretationState `json:"state"`
	Text        *string                  `json:"text"`
	Addresses   *[]EmailAddressV1        `json:"addresses"`
}
type EmailAddressV1 struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}
type EmailDateV1 struct {
	State         EmailDateState      `json:"state"`
	Fields        []int               `json:"fields"`
	Civil         *string             `json:"civil"`
	UTC           *string             `json:"utc"`
	TimezoneText  *string             `json:"timezone_text"`
	TimezoneState EmailTimezoneState  `json:"timezone_state"`
	Diagnostics   []EmailDiagnosticV1 `json:"diagnostics"`
}
type EmailAlternativeV1 struct {
	PartPath     string              `json:"part_path"`
	Kind         EmailBodyKind       `json:"kind"`
	DisplayState EmailDisplayState   `json:"display_state"`
	Display      *EmailArtifactRefV1 `json:"display"`
	Diagnostics  []EmailDiagnosticV1 `json:"diagnostics"`
}
type EmailRelatedGroupV1 struct {
	RootPath  string            `json:"root_path"`
	BodyPath  *string           `json:"body_path"`
	Resources []EmailResourceV1 `json:"resources"`
}
type EmailResourceV1 struct {
	CID        *string            `json:"cid"`
	Candidates []string           `json:"candidates"`
	State      EmailResourceState `json:"state"`
}
type EmailDiagnosticV1 struct {
	Code        EmailDiagnosticCode `json:"code"`
	Operation   EmailOperation      `json:"operation"`
	Path        *string             `json:"path"`
	HeaderIndex *int                `json:"header_index"`
	Detail      string              `json:"detail"`
}
