// Package embeddingbridge implements the fixed synchronous docbank-embedding/v1
// provider-neutral embedding protocol.
package embeddingbridge

import (
	"context"
	"net/http"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	// ContractVersion is the only protocol version this package accepts.
	ContractVersion = "docbank-embedding/v1"

	embeddingsPath    = "/docbank-embedding/v1/embeddings"
	manifestPartName  = "manifest"
	filePartName      = "file"
	manifestMediaType = "application/vnd.docbank.embedding-manifest+json;version=1"
	responseMediaType = "application/vnd.docbank.embedding-result+json;version=1"
	adapterContract   = "docbank-standard-embedding-bridge/v1"
)

// SecretResolver resolves only the configured named credential binding.
// Values are runtime-only and never enter protocol manifests or fingerprints.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

// Profile freezes the bridge origin, vector-space identity, credential name,
// egress authority, and synchronous execution bounds.
type Profile struct {
	Origin           string
	Descriptor       document.EmbeddingDescriptor
	SecretBinding    string
	EgressPolicy     providerhttp.EgressPolicy
	RequestTimeout   time.Duration
	MaxBatchItems    int
	MaxInputBytes    int64
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

// Client implements document.EmbeddingProvider through docbank-embedding/v1.
type Client struct {
	origin           string
	descriptor       document.EmbeddingDescriptor
	secretBinding    string
	secrets          SecretResolver
	http             *http.Client
	requestTimeout   time.Duration
	maxBatchItems    int
	maxInputBytes    int64
	maxRequestBytes  int64
	maxResponseBytes int64
}

// RequestManifest is the deterministic authorization and input identity sent
// as the first multipart part. RequestChecksum is SHA-256 over the canonical
// manifest with that field omitted.
type RequestManifest struct {
	ContractVersion       string                          `json:"contract_version"`
	DescriptorFingerprint string                          `json:"descriptor_fingerprint"`
	PolicyFingerprint     string                          `json:"policy_fingerprint"`
	Authorization         document.EmbeddingAuthorization `json:"authorization"`
	Inputs                []ManifestInput                 `json:"inputs"`
	RequestChecksum       string                          `json:"request_checksum,omitempty"`
}

// ManifestInput binds one exact request position and its rendered text or
// authorization-held original file.
type ManifestInput struct {
	Index       int                                `json:"index"`
	Key         string                             `json:"key"`
	Role        document.EmbeddingRole             `json:"role"`
	Kind        document.EmbeddingInputKind        `json:"kind"`
	ByteLength  int64                              `json:"byte_length"`
	SHA256      string                             `json:"sha256"`
	Text        string                             `json:"text,omitempty"`
	HeadingPath []string                           `json:"heading_path,omitempty"`
	SourceSpans []ManifestSpan                     `json:"source_spans,omitempty"`
	Upload      *document.AuthorizedUploadMetadata `json:"upload,omitempty"`
	FilePart    string                             `json:"file_part,omitempty"`
	FileIndex   *int                               `json:"file_index,omitempty"`
}

// ManifestSpan is the closed wire form of one canonical source span.
type ManifestSpan struct {
	UnitIndex int `json:"unit_index"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`
}

// Response is the strict synchronous result envelope.
type Response struct {
	ContractVersion       string                     `json:"contract_version"`
	DescriptorFingerprint string                     `json:"descriptor_fingerprint"`
	PolicyFingerprint     string                     `json:"policy_fingerprint"`
	RequestChecksum       string                     `json:"request_checksum"`
	Vectors               []document.EmbeddingVector `json:"vectors"`
}
