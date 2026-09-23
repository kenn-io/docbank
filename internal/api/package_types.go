package api

type PackageContainer struct {
	ContainerID string `json:"container_id"`
	Format      string `json:"format"`
	State       string `json:"state"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"created_at"`
}

type PackagePreflightRequest struct {
	Profile        string `json:"profile" minLength:"1"`
	PageMapProfile string `json:"page_map_profile,omitzero"`
	Encoding       string `json:"encoding" minLength:"1"`
	SourceKind     string `json:"source_kind" enum:"root,container"`
	SourceRef      string `json:"source_ref" minLength:"1"`
	Mapping        []byte `json:"mapping,omitzero"`
}

type PackageDiagnostic struct {
	Code       string `json:"code"`
	Severity   string `json:"severity"`
	LoadFile   string `json:"load_file,omitzero"`
	RowID      string `json:"row_id,omitzero"`
	RowOrdinal int    `json:"row_ordinal,omitzero"`
	Column     string `json:"column,omitzero"`
	Detail     string `json:"detail,omitzero"`
}

type PackageVolume struct {
	Ordinal      int    `json:"ordinal"`
	VolumeName   string `json:"volume_name"`
	DeclaredRoot string `json:"declared_root"`
}

type PackagePreflight struct {
	PreflightID     string              `json:"preflight_id" format:"uuid"`
	SourceKind      string              `json:"source_kind"`
	SourceRef       string              `json:"source_ref"`
	ProfileSHA256   string              `json:"profile_sha256"`
	MappingSHA256   string              `json:"mapping_sha256"`
	ManifestSHA256  string              `json:"manifest_sha256"`
	Volumes         []PackageVolume     `json:"volumes"`
	Records         int                 `json:"records"`
	Pages           int                 `json:"pages"`
	DiagnosticCount int                 `json:"diagnostic_count"`
	Diagnostics     []PackageDiagnostic `json:"diagnostics"`
	Blocking        bool                `json:"blocking"`
	CreatedAt       string              `json:"created_at" format:"date-time"`
	ExpiresAt       string              `json:"expires_at" format:"date-time"`
}

type PackageDiagnosticPage struct {
	Diagnostics []PackageDiagnostic `json:"diagnostics"`
	Total       int                 `json:"total"`
	NextCursor  string              `json:"next_cursor,omitzero"`
}

type PackageImportRequest struct {
	PreflightID       string `json:"preflight_id"`
	Into              string `json:"into"`
	Name              string `json:"name"`
	Party             string `json:"party"`
	OperationID       string `json:"operation_id"`
	AcceptPartial     bool   `json:"accept_partial,omitzero"`
	IndexSuppliedText bool   `json:"index_supplied_text,omitzero"`
}

type PackageImportJob struct {
	OperationID string   `json:"operation_id"`
	JobID       string   `json:"job_id"`
	PackageID   string   `json:"package_id"`
	PreflightID string   `json:"preflight_id"`
	State       string   `json:"state"`
	Committed   int      `json:"committed"`
	Total       int      `json:"total"`
	GapCount    int      `json:"gap_count"`
	Gaps        []string `json:"gaps,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

type PersonSummary struct {
	PersonID    string `json:"person_id"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
	Revision    int64  `json:"revision"`
}

type PersonPage struct {
	Items      []PersonSummary `json:"items"`
	NextCursor string          `json:"next_cursor,omitzero"`
}

type CustodianAssignment struct {
	AssignmentID    string `json:"assignment_id"`
	ScopeKind       string `json:"scope_kind"`
	PackageID       string `json:"package_id,omitzero"`
	PackageRecordID string `json:"package_record_id,omitzero"`
	PersonID        string `json:"person_id,omitzero"`
	RawLabel        string `json:"raw_label"`
	Rank            string `json:"rank"`
	Basis           string `json:"basis"`
	SourceRef       string `json:"source_ref"`
	Revision        int64  `json:"revision"`
	RecordedAt      string `json:"recorded_at"`
}

type CustodianPage struct {
	Items      []CustodianAssignment `json:"items"`
	Total      int64                 `json:"total"`
	NextCursor string                `json:"next_cursor,omitzero"`
}

type CustodianResolveRequest struct {
	PersonID        string `json:"person_id"`
	IfMatchRevision int64  `json:"if_match_revision"`
}

type PackageCustodianRequest struct {
	RowID           *string `json:"row_id,omitzero"`
	RawLabel        string  `json:"raw_label"`
	PersonID        string  `json:"person_id,omitzero"`
	IfMatchRevision int64   `json:"if_match_revision"`
}
