package embedconfig

import "time"

// DefaultBatchItems is the item cap used when Batch.Items is zero. 32 is the
// largest request Hugging Face Text Embeddings Inference accepts by default,
// and at about 512 tokens per input it stays under the smallest per-request
// token caps of hosted providers. Items cannot bound tokens; set a token
// budget in Batch when inputs are long.
const DefaultBatchItems = 32

// DefaultTimeout is the HTTP timeout used when Transport.Timeout is zero.
// A local server encoding a large batch, or on CPU, can take longer; set
// Transport.Timeout for those deployments.
const DefaultTimeout = 30 * time.Second

// DefaultMaxResponseBytes bounds an embedding response when
// Transport.MaxResponseBytes is zero.
const DefaultMaxResponseBytes = 32 << 20

// Metric is the distance contract for one vector space.
type Metric string

const (
	// MetricCosine is the distance the vector pipeline can store.
	MetricCosine Metric = "cosine"
	// MetricDotProduct names a distance this pipeline cannot store yet.
	// Validation rejects it.
	MetricDotProduct Metric = "dot_product"
	// MetricL2 names a distance this pipeline cannot store yet.
	// Validation rejects it.
	MetricL2 Metric = "l2"
)

// Normalization is applied to accepted vectors before they are returned.
type Normalization string

const (
	NormalizationNone Normalization = "none"
	NormalizationL2   Normalization = "l2"
)

// InputType selects how a request tells the provider about role.
type InputType string

const (
	// InputTypeNone sends the text without an input_type field.
	InputTypeNone InputType = "none"
	// InputTypeRetrieval sends input_type as document or query.
	InputTypeRetrieval InputType = "retrieval"
)

// Role is the encoding role for one input.
type Role string

const (
	RoleDocument Role = "document"
	RoleQuery    Role = "query"
)

// Truncation is the explicit policy when formatted input exceeds MaxTokens.
// The zero value is not a policy.
type Truncation string

const (
	// TruncationReject returns an error instead of cutting or dropping text.
	TruncationReject Truncation = "reject"
	// TruncationDropTail keeps the fitting prefix and reports the dropped tail.
	TruncationDropTail Truncation = "drop_tail"
)

// Model is the embedding model and the vector-space controls that travel
// with it. Revision is the caller's weights or deployment epoch, including
// a salt when the same model name can mean different weights.
type Model struct {
	Name       string
	Revision   string
	Dimensions int
	Metric     Metric
	// Normalization reports whether stored vectors are L2-normalized.
	Normalization Normalization
	// Pooling is a model-family label such as cls or last_token.
	// Empty means the provider defines pooling and it is not pinned.
	Pooling string
	// EncodingFormat is sent on the request when set, for example float.
	EncodingFormat string
	// RequestDimensions sends the dimensions field when the provider can
	// reduce a native width. Leaving it false keeps the provider default.
	RequestDimensions bool
}

// Roles holds the document and query controls that change encoded text.
// Formatter names are caller-owned labels. Prefixes and suffixes are literal
// affixes the shared client applies. Either one can change the vector.
type Roles struct {
	DocumentFormatter string
	QueryFormatter    string
	DocumentPrefix    string
	DocumentSuffix    string
	QueryPrefix       string
	QuerySuffix       string
	InputType         InputType
}

// Deployment is the endpoint a client calls. PinEndpoint opts that endpoint
// into the vector identity. TrustPrivateNetwork is an operational permission
// for plaintext HTTP on a private address. It is not part of either identity.
type Deployment struct {
	BaseURL             string
	PinEndpoint         bool
	TrustPrivateNetwork bool
}

// Batch limits one provider request. Items is a count. MaxTokens is an
// aggregate input-token cap, and InputTokenUpperBound is the caller's
// conservative per-input ceiling. Both token fields stay zero for count-only
// batching. These limits pack requests. They do not change a vector that was
// produced from the same text.
type Batch struct {
	Items                int
	MaxTokens            int
	InputTokenUpperBound int
}

// Transport limits the HTTP call. It does not change vectors.
type Transport struct {
	Timeout          time.Duration
	MaxResponseBytes int
}

// InputLimits identifies the indexed-input recipe and, when MaxTokens is
// positive, the token window used to fit that recipe. A zero MaxTokens means
// the caller is not using this token window.
type InputLimits struct {
	Recipe            string
	Tokenizer         string
	TokenizerRevision string
	ContentID         string
	MaxTokens         int
	OverlapTokens     int
	// MaxSpans caps source spans inside a token window.
	// Zero means unlimited, not an unset limit, and validation accepts it.
	MaxSpans   int
	Truncation Truncation
}
