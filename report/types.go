// Package report defines portable search-term report evidence and calculations.
package report

import (
	"errors"
	"time"
)

// ErrVisibilityChanged means source authority was withdrawn after observation.
// Frozen counts must be withheld rather than recalculated.
var ErrVisibilityChanged = errors.New("report source visibility changed")

// StateComplete marks evidence that covers the captured document fully.
const StateComplete = "complete"

// MaxRequestSummaryJSONBytes bounds each reusable request and compact run receipt.
const MaxRequestSummaryJSONBytes = 8 << 20

// Identity binds evidence to one document node, immutable version, and content digest.
type Identity struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	SHA256    string `json:"sha256"`
}

// DateRange is an inclusive pair of literal YYYY-MM-DD dates in the report timezone.
type DateRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Term is one ordered search row. Number is a positive caller-assigned label;
// Syntax is simple or advanced, and Dates contains fixed inclusive cutoffs.
type Term struct {
	Number     int       `json:"number"`
	Expression string    `json:"expression"`
	Syntax     string    `json:"syntax"`
	Dates      DateRange `json:"dates"`
}

// DateChoice records a reviewed decision against one exact candidate and evidence
// digest. Action is select, interpret, or reclassify; Reason is required.
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

// Request is the durable search-export request shared by recent export history, run
// receipts, and offline bundles. Version 1 accepts all documents or collections;
// version 2 also accepts exact selected document identities. Timezone is an explicit IANA
// timezone; SourceTimezone and NumericDateOrder (MDY or DMY) resolve missing source
// timezone and numeric-date ambiguity. CoverageMode defaults to strict, while
// available_only permits incomplete evidence. Terms retain caller order and fixed
// date cutoffs. DateChoices bind reviewed decisions to the captured evidence.
// NormalizeRequest validates this contract without consulting a vault.
type Request struct {
	Version           int          `json:"version"`
	Profile           string       `json:"profile,omitempty"`
	AllDocuments      bool         `json:"all_documents"`
	CollectionIDs     []string     `json:"collection_ids,omitempty"`
	SelectedDocuments []Identity   `json:"selected_documents,omitempty"`
	Timezone          string       `json:"timezone"`
	SourceTimezone    string       `json:"source_timezone,omitempty"`
	NumericDateOrder  string       `json:"numeric_date_order,omitempty"`
	CoverageMode      string       `json:"coverage_mode"`
	Terms             []Term       `json:"terms"`
	DateChoices       []DateChoice `json:"date_choices,omitempty"`
}

// Locator identifies retained date evidence. Text offsets are zero-based bytes
// with an exclusive EndByte; Page, when present, is one-based.
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

// DateCandidate preserves a possible date, its semantic role, source, and evidence.
// Rejection explains why automatic selection cannot use the value as captured.
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

// DateSelection records the effective date and rule or reviewed reason that chose it.
type DateSelection struct {
	CandidateID string `json:"candidate_id"`
	Date        string `json:"date"`
	RuleID      string `json:"rule_id"`
	Reason      string `json:"reason"`
	Mode        string `json:"mode"`
}

// CoverageDiagnostic explains a gap in one member's captured evidence.
type CoverageDiagnostic struct {
	Code       string `json:"code"`
	EvidenceID string `json:"evidence_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// MemberCoverage records search-text, date-evidence, and family coverage separately.
// SearchState is complete or missing; FamilyState is complete or incomplete.
// DateEvidenceState is complete: candidate-limit failures abort the observation.
type MemberCoverage struct {
	SearchState       string               `json:"search_state"`
	DateEvidenceState string               `json:"date_evidence_state"`
	FamilyState       string               `json:"family_state"`
	Diagnostics       []CoverageDiagnostic `json:"diagnostics,omitempty"`
}

// CollectionWitness retains the membership authority that places a member in scope.
type CollectionWitness struct {
	CollectionID     string `json:"collection_id"`
	MembershipID     string `json:"membership_id"`
	MembershipSHA256 string `json:"membership_sha256"`
	OriginalPath     string `json:"original_path"`
	OriginalMTime    string `json:"original_mtime,omitempty"`
	Supersedes       string `json:"supersedes,omitempty"`
}

// Member is one frozen document with retained evidence and one bit per term in
// RawMatches, Eligible, and Hits. Calculate derives the latter two slices.
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

// Relation retains the authority for a parent-child document family edge.
type Relation struct {
	Parent         Identity `json:"parent"`
	Child          Identity `json:"child"`
	EvidenceID     string   `json:"evidence_id"`
	EvidenceSHA256 string   `json:"evidence_sha256"`
}

// CoverageSelection identifies the processing configuration used for the observation.
type CoverageSelection struct {
	Configuration       string `json:"configuration"`
	ProfileFingerprint  string `json:"profile_fingerprint,omitempty"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
}

// NativeText binds searchable native text to a version. Text is transient and
// must be discarded after candidate extraction; it is never serialized.
type NativeText struct {
	Text                []byte `json:"-"`
	TextSHA256          string `json:"text_sha256"`
	ExtractorID         string `json:"extractor_id,omitempty"`
	Status              string `json:"status"`
	SearchableVersionID string `json:"searchable_version_id"`
}

// TextBinding identifies the native text or rendition bytes inspected for dates.
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

// RawDateField retains a captured native or metadata field before date adaptation.
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

// Coverage counts date-eligible documents, once per row or once across all rows.
// Warnings retain observation-level notices independently of the derived counts.
type Coverage struct {
	Scoped             int64    `json:"scoped"`
	Searchable         int64    `json:"searchable"`
	MissingText        int64    `json:"missing_text"`
	IncompleteFamilies int64    `json:"incomplete_families"`
	FallbackDates      int64    `json:"fallback_dates"`
	Warnings           []string `json:"warnings,omitempty"`
}

// Dependency identifies source authority and its revision at observation time.
type Dependency struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// Frame is one frozen observation. Calculations and bundle verification use its
// retained evidence without reading live vault state.
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

// Counts holds the five distinct-document counts for one term's eligible population.
type Counts struct {
	Hits                 int64 `json:"hits"`
	HitsPlusFamily       int64 `json:"hits_plus_family"`
	UniqueHits           int64 `json:"unique_hits"`
	UniqueFamilies       int64 `json:"unique_families"`
	UniqueHitsPlusFamily int64 `json:"unique_hits_plus_family"`
}

// Result combines a frozen frame with calculated term counts and coverage.
type Result struct {
	Frame  Frame    `json:"frame"`
	Counts []Counts `json:"counts"`
}

// Artifact owns the downloadable CSV and bundle for a retained run.
type Artifact struct {
	ID        string    `json:"id"`
	Result    Result    `json:"-"`
	CSV       []byte    `json:"-"`
	Bundle    []byte    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Summary is the compact run receipt; it carries no source text or candidate quotes.
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

// DatePageRequest requests a bounded page of date candidates. A document's
// candidates may span pages; DateReviewMember.CandidatesComplete marks completion.
type DatePageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// DateReviewMember exposes one document's candidates and effective reviewed decision.
type DateReviewMember struct {
	CandidatesComplete bool            `json:"candidates_complete"`
	Document           Identity        `json:"document"`
	Candidates         []DateCandidate `json:"candidates"`
	Selection          DateSelection   `json:"selection"`
	Choice             *DateChoice     `json:"choice,omitempty"`
}

// DatePage contains reviewable documents and an opaque cursor for the next page.
type DatePage struct {
	Members    []DateReviewMember `json:"members"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// Verification reports internal bundle consistency. Offline verification cannot
// establish that the producer searched all source documents.
type Verification struct {
	InternallyConsistent bool `json:"internally_consistent"`
	SourceVerified       bool `json:"source_verified"`
}
