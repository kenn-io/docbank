package document

const (
	// SourceMetadataContractV1 identifies canonical embedded source claims.
	SourceMetadataContractV1 = "source-metadata/v1"

	MaxSourceMetadataFields       = 512
	MaxSourceMetadataWarnings     = 256
	MaxSourceMetadataListValues   = 256
	MaxSourceMetadataLabelBytes   = 256
	MaxSourceMetadataValueBytes   = 64 << 10
	MaxSourceMetadataEncodedBytes = 8 << 20
)

// SourceMetadataValueKind identifies the one typed payload carried by a field.
type SourceMetadataValueKind string

const (
	SourceMetadataString     SourceMetadataValueKind = "string"
	SourceMetadataStringList SourceMetadataValueKind = "string_list"
	SourceMetadataInteger    SourceMetadataValueKind = "integer"
	SourceMetadataNumber     SourceMetadataValueKind = "number"
	SourceMetadataBoolean    SourceMetadataValueKind = "boolean"
	SourceMetadataTimestamp  SourceMetadataValueKind = "timestamp"
)

// SourceMetadataTimestampPrecision preserves what the source actually stated.
type SourceMetadataTimestampPrecision string

const (
	SourceMetadataPrecisionDate     SourceMetadataTimestampPrecision = "date"
	SourceMetadataPrecisionHour     SourceMetadataTimestampPrecision = "hour"
	SourceMetadataPrecisionMinute   SourceMetadataTimestampPrecision = "minute"
	SourceMetadataPrecisionSecond   SourceMetadataTimestampPrecision = "second"
	SourceMetadataPrecisionFraction SourceMetadataTimestampPrecision = "fraction"
)

// SourceMetadataTimezoneKind distinguishes an explicit zone from an omitted one.
type SourceMetadataTimezoneKind string

const (
	SourceMetadataTimezoneOmitted SourceMetadataTimezoneKind = "omitted"
	SourceMetadataTimezoneUTC     SourceMetadataTimezoneKind = "utc"
	SourceMetadataTimezoneOffset  SourceMetadataTimezoneKind = "offset"
)

// SourceMetadataTimestampV1 retains the raw value and a normalization only
// when the source syntax is unambiguous. Offset is empty for UTC or omission.
type SourceMetadataTimestampV1 struct {
	Normalized string                           `json:"normalized"`
	Offset     string                           `json:"offset,omitempty"`
	Precision  SourceMetadataTimestampPrecision `json:"precision"`
	Raw        string                           `json:"raw"`
	Timezone   SourceMetadataTimezoneKind       `json:"timezone"`
}

// SourceMetadataValueV1 is a closed typed union. Exactly one payload matching
// Kind is populated.
type SourceMetadataValueV1 struct {
	Boolean   *bool                      `json:"boolean,omitempty"`
	Integer   *int64                     `json:"integer,omitempty"`
	Kind      SourceMetadataValueKind    `json:"kind"`
	Number    *float64                   `json:"number,omitempty"`
	String    string                     `json:"string,omitempty"`
	Strings   []string                   `json:"strings,omitempty"`
	Timestamp *SourceMetadataTimestampV1 `json:"timestamp,omitempty"`
}

// SourceMetadataFieldV1 is one canonical claim with its exact source label.
// Sensitive fields remain local unless a future explicit disclosure policy
// selects them.
type SourceMetadataFieldV1 struct {
	Key         string                `json:"key"`
	Namespace   string                `json:"namespace"`
	Sensitive   bool                  `json:"sensitive"`
	SourceField string                `json:"source_field"`
	Value       SourceMetadataValueV1 `json:"value"`
}

// SourceMetadataWarningV1 preserves extraction uncertainty without guessing.
type SourceMetadataWarningV1 struct {
	Code        string `json:"code"`
	Detail      string `json:"detail"`
	Namespace   string `json:"namespace"`
	SourceField string `json:"source_field"`
}

// SourceMetadataV1 is content-scoped embedded evidence. It intentionally has
// no filename, path, ingest, filesystem, or user-authored metadata fields.
type SourceMetadataV1 struct {
	ContractVersion string                    `json:"contract_version"`
	Fields          []SourceMetadataFieldV1   `json:"fields"`
	Warnings        []SourceMetadataWarningV1 `json:"warnings"`
}
