// Package bridge implements the fixed docbank-rendition/v1 HTTP bridge.
package bridge

import (
	"encoding/json/jsontext"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const (
	ContractVersion = "docbank-rendition/v1"

	jobsPath              = "/docbank-rendition/v1/jobs"
	authorizationPartName = "authorization"
	sourcePartName        = "source"
	jobMediaType          = "application/vnd.docbank.rendition-job+json;version=1"
	evidenceMediaType     = "application/vnd.docbank.source-evidence+json;version=1"
)

// JobStatus is one protocol state returned by a bridge.
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
)

// SecretResolver resolves only a configured named binding. Secret values are
// used for the fixed Authorization header and never enter manifests or receipts.
type SecretResolver = providerutil.SecretResolver

// Profile fixes one bridge origin, provider identity, and execution bounds.
type Profile struct {
	Origin           string
	Descriptor       document.RenditionDescriptor
	SecretBinding    string
	RequestTimeout   time.Duration
	TotalTimeout     time.Duration
	PollInterval     time.Duration
	MaxPollAttempts  int
	MaxResponseBytes int64
}

// Client implements document.RenditionProvider through docbank-rendition/v1.
type Client struct {
	executor        providerutil.Executor
	descriptor      document.RenditionDescriptor
	totalTimeout    time.Duration
	pollInterval    time.Duration
	maxPollAttempts int
}

// AuthorizationManifest is the canonical multipart policy part sent beside
// the exact source bytes.
type AuthorizationManifest struct {
	ContractVersion string                            `json:"contract_version"`
	Source          document.AuthorizedUploadMetadata `json:"source"`
	Authorization   document.RenditionAuthorization   `json:"authorization"`
}

type jobEnvelope struct {
	ContractVersion       string         `json:"contract_version"`
	Status                JobStatus      `json:"status"`
	JobID                 string         `json:"job_id"`
	SourceSHA256          string         `json:"source_sha256"`
	AdapterID             string         `json:"adapter_id"`
	DescriptorFingerprint string         `json:"descriptor_fingerprint"`
	PolicyFingerprint     string         `json:"policy_fingerprint"`
	RetryAfterMillis      int64          `json:"retry_after_millis,omitempty"`
	Result                jsontext.Value `json:"result,omitempty"`
	Error                 jsontext.Value `json:"error,omitempty"`
}

type bridgeError struct {
	Code             document.RenditionErrorCode `json:"code"`
	Message          string                      `json:"message"`
	RetryAfterMillis int64                       `json:"retry_after_millis,omitempty"`
}

type completedResult struct {
	Evidence         evidencePayload           `json:"evidence"`
	ProviderMarkdown *binaryPayloadRecord      `json:"provider_markdown,omitempty"`
	Artifacts        []artifactPayload         `json:"artifacts,omitempty"`
	Receipt          document.RenditionReceipt `json:"receipt"`
}

type evidencePayload struct {
	MediaType  string         `json:"media_type"`
	ByteLength int64          `json:"byte_length"`
	SHA256     string         `json:"sha256"`
	Inline     jsontext.Value `json:"inline"`
}

type binaryPayloadRecord struct {
	MediaType    string `json:"media_type"`
	ByteLength   int64  `json:"byte_length"`
	SHA256       string `json:"sha256"`
	InlineBase64 string `json:"inline_base64"`
}

type artifactPayload struct {
	Role         document.EvidenceArtifactRole `json:"role"`
	MediaType    string                        `json:"media_type"`
	ByteLength   int64                         `json:"byte_length"`
	SHA256       string                        `json:"sha256"`
	Location     string                        `json:"location"`
	InlineBase64 string                        `json:"inline_base64,omitempty"`
	ArtifactID   string                        `json:"artifact_id,omitempty"`
}
