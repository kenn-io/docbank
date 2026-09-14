package api

// TimelineRebuildRequest starts or replays one durable timeline rebuild.
type TimelineRebuildRequest struct {
	OperationID string `json:"operation_id" format:"uuid" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
}

// TimelineBuild is the durable progress receipt for one full timeline rebuild.
type TimelineBuild struct {
	OperationID        string  `json:"operation_id" format:"uuid"`
	State              string  `json:"state" enum:"running,completed,failed"`
	DeriverFingerprint string  `json:"deriver_fingerprint" pattern:"^[0-9a-f]{64}$"`
	TargetEpoch        int64   `json:"target_epoch"`
	Scanned            int64   `json:"scanned"`
	Published          int64   `json:"published"`
	Failed             int64   `json:"failed"`
	Unavailable        int64   `json:"unavailable"`
	StartedAt          string  `json:"started_at" format:"date-time"`
	UpdatedAt          string  `json:"updated_at" format:"date-time"`
	FinishedAt         *string `json:"finished_at,omitempty" format:"date-time"`
}

// DocumentEventCoverage reports current-file timeline derivation and safe
// operator diagnostics. Rebuild receipt counters separately cover all retained
// versions.
type DocumentEventCoverage struct {
	Selected             int64  `json:"selected"`
	Indexed              int64  `json:"indexed"`
	Pending              int64  `json:"pending"`
	Failed               int64  `json:"failed"`
	Unavailable          int64  `json:"unavailable"`
	MissingMetadata      int64  `json:"missing_metadata"`
	InvalidDates         int64  `json:"invalid_dates"`
	UnboundProvenance    int64  `json:"unbound_provenance"`
	OperationalFallbacks int64  `json:"operational_fallbacks"`
	ContractVersion      string `json:"contract_version"`
	DeriverFingerprint   string `json:"deriver_fingerprint" pattern:"^[0-9a-f]{64}$"`
	InputEpoch           int64  `json:"input_epoch"`
	PublicationEpoch     int64  `json:"publication_epoch"`
}
