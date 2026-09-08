package retrieval

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/embedding"
	"go.kenn.io/docbank/internal/store"
)

const (
	DefaultCandidateLimit = embedding.DefaultCandidateLimit
	MaxCandidateLimit     = embedding.MaxCandidateLimit
	ReciprocalRankK       = embedding.DefaultReciprocalRankConstant
)

type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeLexical  Mode = "lexical"
	ModeSemantic Mode = "semantic"
	ModeHybrid   Mode = "hybrid"
)

type Lane string

const (
	LaneLexical  Lane = "lexical"
	LaneSemantic Lane = "semantic"
)

type DocumentIdentity struct {
	VaultID          string
	NodeID           int64
	ContentVersionID string
}

type EvidenceReference struct {
	Kind                   string
	VaultID                string
	NodeID                 int64
	NodeRevision           int64
	ContentVersionID       string
	VectorSpaceID          string
	EmbeddingSetID         string
	InputGenerationID      string
	InputID                string
	InputKind              document.EmbeddingInputKind
	BuildID                string
	SegmentID              string
	BlobHash               string
	SourceManifestChecksum string
}

type Candidate struct {
	Document      DocumentIdentity
	Lane          Lane
	Rank          int
	Score         float64
	VectorSpaceID string
	Path          string
	Excerpt       string
	Evidence      []EvidenceReference
}

type Contribution struct {
	Lane         Lane
	Rank         int
	Contribution float64
}

type Result struct {
	Document     DocumentIdentity
	Rank         int
	Score        float64
	Path         string
	Excerpt      string
	LexicalRank  int
	SemanticRank int
	Evidence     []EvidenceReference
	Explanation  []Contribution
}

type CoverageState string

const (
	CoverageUnknown    CoverageState = "unknown"
	CoverageComplete   CoverageState = "complete"
	CoverageIncomplete CoverageState = "incomplete"
)

type Coverage struct {
	BindingRequired   bool
	ScopedDocuments   int
	CompleteDocuments int
	State             CoverageState
}

type Degradation string

const (
	DegradationNone              Degradation = ""
	DegradationExpansionDegraded Degradation = "query_expansion_degraded"
	DegradationRerankingDegraded Degradation = "reranking_degraded"
)

type TraceCode string

const (
	TraceLexicalCandidates  TraceCode = "lexical_candidates"
	TraceSemanticCandidates TraceCode = "semantic_candidates"
	TraceFusedCandidates    TraceCode = "fused_candidates"
	TraceRerankedCandidates TraceCode = "reranked_candidates"
)

type TraceEvent struct {
	Code  TraceCode
	Count int
}

type Report struct {
	RequestedMode Mode
	ActualMode    Mode
	Coverage      Coverage
	Results       []Result
	Truncated     bool
	Trace         []TraceEvent
	Degradations  []Degradation
	Receipts      []ProviderReceipt
}

type Query struct {
	Text                         string
	Mode                         Mode
	Limit                        int
	Scope                        store.SearchOptions
	ProcessingProfileFingerprint string
	BindingID                    string
	Authorization                document.EmbeddingAuthorization
}
