package api

type PackagePreflightRequest struct {
	Profile    string `json:"profile"`
	Encoding   string `json:"encoding"`
	SourceKind string `json:"source_kind"`
	SourceRef  string `json:"source_ref"`
	Mapping    []byte `json:"mapping,omitzero"`
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
	PreflightID     string              `json:"preflight_id"`
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
	CreatedAt       string              `json:"created_at"`
	ExpiresAt       string              `json:"expires_at"`
}

type PackageDiagnosticPage struct {
	Diagnostics []PackageDiagnostic `json:"diagnostics"`
	Total       int                 `json:"total"`
	NextCursor  string              `json:"next_cursor,omitzero"`
}
