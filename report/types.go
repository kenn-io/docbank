// Package report defines portable search-term report evidence and calculations.
package report

import "time"

const StateComplete = "complete"

// MaxRequestSummaryJSONBytes bounds each reusable request and compact run receipt.
const MaxRequestSummaryJSONBytes = 8 << 20

type Identity struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	SHA256    string `json:"sha256"`
}

type DateRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Term struct {
	Number     int       `json:"number"`
	Expression string    `json:"expression"`
	Syntax     string    `json:"syntax"`
	Dates      DateRange `json:"dates"`
}

type DateChoice struct {
	Document         Identity `json:"document"`
	CandidateID      string   `json:"candidate_id"`
	EvidenceSHA256   string   `json:"evidence_sha256"`
	Reason           string   `json:"reason"`
	Action           string   `json:"action"`
	ReviewedDate     string   `json:"reviewed_date,omitempty"`
	ReviewedTimezone string   `json:"reviewed_timezone,omitempty"`
	ReviewedRole     string   `json:"reviewed_role,omitempty"`
}

type Request struct {
	Version          int          `json:"version"`
	Profile          string       `json:"profile,omitempty"`
	AllDocuments     bool         `json:"all_documents"`
	CollectionIDs    []string     `json:"collection_ids,omitempty"`
	Timezone         string       `json:"timezone"`
	SourceTimezone   string       `json:"source_timezone,omitempty"`
	NumericDateOrder string       `json:"numeric_date_order,omitempty"`
	CoverageMode     string       `json:"coverage_mode"`
	Terms            []Term       `json:"terms"`
	DateChoices      []DateChoice `json:"date_choices,omitempty"`
}

type Locator struct {
	EvidenceID     string `json:"evidence_id,omitempty"`
	EvidenceSHA256 string `json:"evidence_sha256,omitempty"`
	RenditionID    string `json:"rendition_id,omitempty"`
	TextSHA256     string `json:"text_sha256,omitempty"`
	Page           int    `json:"page,omitempty"`
	StartByte      int64  `json:"start_byte"`
	EndByte        int64  `json:"end_byte"`
	Quote          string `json:"quote,omitempty"`
}

type DateCandidate struct {
	ID              string   `json:"id"`
	Document        Identity `json:"document"`
	Role            string   `json:"role"`
	SourceClass     string   `json:"source_class"`
	Raw             string   `json:"raw"`
	Value           string   `json:"value,omitempty"`
	Precision       string   `json:"precision,omitempty"`
	Timezone        string   `json:"timezone,omitempty"`
	Confidence      string   `json:"confidence,omitempty"`
	SourceNamespace string   `json:"source_namespace,omitempty"`
	SourceField     string   `json:"source_field,omitempty"`
	ClaimBasis      string   `json:"claim_basis,omitempty"`
	Locator         Locator  `json:"locator"`
	Rejection       string   `json:"rejection,omitempty"`
}

type DateSelection struct {
	CandidateID string `json:"candidate_id"`
	Date        string `json:"date"`
	RuleID      string `json:"rule_id"`
	Reason      string `json:"reason"`
	Mode        string `json:"mode"`
}

type CoverageDiagnostic struct {
	Code       string `json:"code"`
	EvidenceID string `json:"evidence_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type MemberCoverage struct {
	SearchState       string               `json:"search_state"`
	DateEvidenceState string               `json:"date_evidence_state"`
	FamilyState       string               `json:"family_state"`
	Diagnostics       []CoverageDiagnostic `json:"diagnostics,omitempty"`
}

type CollectionWitness struct {
	CollectionID     string `json:"collection_id"`
	MembershipID     string `json:"membership_id"`
	MembershipSHA256 string `json:"membership_sha256"`
	OriginalPath     string `json:"original_path"`
	OriginalMTime    string `json:"original_mtime,omitempty"`
	Supersedes       string `json:"supersedes,omitempty"`
}

type Member struct {
	Coverage            MemberCoverage      `json:"coverage"`
	Identity            Identity            `json:"identity"`
	Kind                string              `json:"kind"`
	FamilyID            string              `json:"family_id"`
	CollectionWitnesses []CollectionWitness `json:"collection_witnesses,omitempty"`
	Candidates          []DateCandidate     `json:"candidates,omitempty"`
	Selection           DateSelection       `json:"selection"`
	RawMatches          []bool              `json:"raw_matches"`
	Eligible            []bool              `json:"eligible,omitempty"`
	Hits                []bool              `json:"hits,omitempty"`
}

type Relation struct {
	Parent         Identity `json:"parent"`
	Child          Identity `json:"child"`
	EvidenceID     string   `json:"evidence_id"`
	EvidenceSHA256 string   `json:"evidence_sha256"`
}

type CoverageSelection struct {
	Configuration       string `json:"configuration"`
	ProfileFingerprint  string `json:"profile_fingerprint,omitempty"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
}

type NativeText struct {
	Text                []byte `json:"-"`
	TextSHA256          string `json:"text_sha256"`
	ExtractorID         string `json:"extractor_id,omitempty"`
	Status              string `json:"status"`
	SearchableVersionID string `json:"searchable_version_id"`
}

type TextBinding struct {
	Kind               string      `json:"kind"`
	Document           Identity    `json:"document"`
	NodeRevision       int64       `json:"node_revision"`
	ProfileFingerprint string      `json:"profile_fingerprint,omitempty"`
	GenerationID       string      `json:"generation_id,omitempty"`
	AttachmentID       string      `json:"attachment_id,omitempty"`
	BuildID            string      `json:"build_id,omitempty"`
	ArtifactID         string      `json:"artifact_id,omitempty"`
	ArtifactRole       string      `json:"artifact_role,omitempty"`
	ArtifactSHA256     string      `json:"artifact_sha256,omitempty"`
	RenditionID        string      `json:"rendition_id,omitempty"`
	Size               int64       `json:"size"`
	Native             *NativeText `json:"native,omitempty"`
}

type RawDateField struct {
	Document         Identity `json:"document"`
	Namespace        string   `json:"namespace"`
	SourceField      string   `json:"source_field"`
	Key              string   `json:"key"`
	Raw              string   `json:"raw"`
	Normalized       string   `json:"normalized,omitempty"`
	Precision        string   `json:"precision,omitempty"`
	Timezone         string   `json:"timezone,omitempty"`
	ClaimBasis       string   `json:"claim_basis,omitempty"`
	GenerationID     string   `json:"generation_id,omitempty"`
	GenerationSHA256 string   `json:"generation_sha256,omitempty"`
	InputFingerprint string   `json:"input_fingerprint,omitempty"`
	ProjectionState  string   `json:"projection_state,omitempty"`
	Sensitive        bool     `json:"sensitive,omitempty"`
}

type Coverage struct {
	Scoped             int64    `json:"scoped"`
	Searchable         int64    `json:"searchable"`
	MissingText        int64    `json:"missing_text"`
	IncompleteFamilies int64    `json:"incomplete_families"`
	FallbackDates      int64    `json:"fallback_dates"`
	Warnings           []string `json:"warnings,omitempty"`
}

type Dependency struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

type Frame struct {
	VaultID           string            `json:"vault_id"`
	GenerationID      string            `json:"generation_id"`
	GenerationKind    string            `json:"generation_kind"`
	ObservedAt        time.Time         `json:"observed_at"`
	Request           Request           `json:"request"`
	Members           []Member          `json:"members"`
	Relations         []Relation        `json:"relations"`
	Texts             []TextBinding     `json:"texts"`
	RawDateFields     []RawDateField    `json:"raw_date_fields"`
	CoverageSelection CoverageSelection `json:"coverage_selection"`
	Dependencies      []Dependency      `json:"dependencies"`
	Coverage          Coverage          `json:"coverage"`
	RowCoverage       []Coverage        `json:"row_coverage"`
}

type Counts struct {
	Hits                 int64 `json:"hits"`
	HitsPlusFamily       int64 `json:"hits_plus_family"`
	UniqueHits           int64 `json:"unique_hits"`
	UniqueFamilies       int64 `json:"unique_families"`
	UniqueHitsPlusFamily int64 `json:"unique_hits_plus_family"`
}

type Result struct {
	Frame  Frame    `json:"frame"`
	Counts []Counts `json:"counts"`
}

type Artifact struct {
	ID        string    `json:"id"`
	Result    Result    `json:"-"`
	CSV       []byte    `json:"-"`
	Bundle    []byte    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Summary struct {
	ID              string     `json:"id"`
	ParentID        string     `json:"parent_id,omitempty"`
	State           string     `json:"state"`
	ObservedAt      time.Time  `json:"observed_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	Terms           []Term     `json:"terms"`
	Counts          []Counts   `json:"counts,omitempty"`
	Coverage        Coverage   `json:"coverage"`
	RowCoverage     []Coverage `json:"row_coverage,omitempty"`
	UnresolvedDates int64      `json:"unresolved_dates"`
	CSVSHA256       string     `json:"csv_sha256,omitempty"`
	BundleSHA256    string     `json:"bundle_sha256,omitempty"`
	CSVBytes        int64      `json:"csv_bytes,omitempty"`
	BundleBytes     int64      `json:"bundle_bytes,omitempty"`
}

type DatePageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type DateReviewMember struct {
	CandidatesComplete bool            `json:"candidates_complete"`
	Document           Identity        `json:"document"`
	Candidates         []DateCandidate `json:"candidates"`
	Selection          DateSelection   `json:"selection"`
	Choice             *DateChoice     `json:"choice,omitempty"`
}

type DatePage struct {
	Members    []DateReviewMember `json:"members"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type Verification struct {
	InternallyConsistent bool `json:"internally_consistent"`
	SourceVerified       bool `json:"source_verified"`
}
