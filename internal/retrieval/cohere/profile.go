// Package cohere implements the fixed hosted Cohere Rerank v4 contract.
package cohere

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/cohereapi"
)

const (
	origin               = "https://api.cohere.com"
	rerankPath           = "/v2/rerank"
	adapterContract      = "docbank-cohere-rerank-v4/v1"
	maximumCandidates    = 1000
	maximumQueryBytes    = 64 << 10
	maximumExcerptBytes  = 64 << 10
	maximumTotalExcerpts = int64(64 << 20)
	maximumRequestBytes  = int64(64 << 20)
	maximumResponseBytes = int64(64 << 20)
	maximumTokensPerDoc  = 4096
	maximumTimeout       = 5 * time.Minute
	maximumTokenBytes    = 128
)

type Model string

const (
	ModelPro  Model = "rerank-v4.0-pro"
	ModelFast Model = "rerank-v4.0-fast"
)

type SecretResolver interface {
	ResolveSecret(ctx context.Context, binding string) (string, error)
}

type Profile struct {
	ID                   string
	Model                Model
	CompatibilityEpoch   string
	ModelRevision        string
	SecretBinding        string
	RequestTimeout       time.Duration
	MaxCandidates        int
	MaxQueryBytes        int
	MaxExcerptBytes      int
	MaxTotalExcerptBytes int64
	MaxRequestBytes      int64
	MaxResponseBytes     int64
	MaxTokensPerDocument int // Provider truncation limit in tokens, independent of excerpt bytes.
	EgressPolicy         providerhttp.EgressPolicy
}

type Client struct {
	profile     Profile
	fingerprint string
	secrets     SecretResolver
	http        *http.Client
}

type policyIdentity struct {
	AdapterContract      string                   `json:"adapter_contract"`
	Origin               string                   `json:"origin"`
	Route                string                   `json:"route"`
	ID                   string                   `json:"id"`
	Model                Model                    `json:"model"`
	CompatibilityEpoch   string                   `json:"compatibility_epoch"`
	ModelRevision        string                   `json:"model_revision"`
	SecretBinding        string                   `json:"secret_binding"`
	RequestTimeout       int64                    `json:"request_timeout_nanos"`
	MaxCandidates        int                      `json:"max_candidates"`
	MaxQueryBytes        int                      `json:"max_query_bytes"`
	MaxExcerptBytes      int                      `json:"max_excerpt_bytes"`
	MaxTotalExcerptBytes int64                    `json:"max_total_excerpt_bytes"`
	MaxRequestBytes      int64                    `json:"max_request_bytes"`
	MaxResponseBytes     int64                    `json:"max_response_bytes"`
	MaxTokensPerDocument int                      `json:"max_tokens_per_document"`
	Egress               cohereapi.EgressIdentity `json:"egress"`
}

func PolicyFingerprint(profile Profile) (string, error) {
	profile, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(policyIdentity{AdapterContract: adapterContract, Origin: origin, Route: rerankPath,
		ID: profile.ID, Model: profile.Model, CompatibilityEpoch: profile.CompatibilityEpoch,
		ModelRevision: profile.ModelRevision, SecretBinding: profile.SecretBinding,
		RequestTimeout: int64(profile.RequestTimeout), MaxCandidates: profile.MaxCandidates,
		MaxQueryBytes: profile.MaxQueryBytes, MaxExcerptBytes: profile.MaxExcerptBytes,
		MaxTotalExcerptBytes: profile.MaxTotalExcerptBytes, MaxRequestBytes: profile.MaxRequestBytes,
		MaxResponseBytes: profile.MaxResponseBytes, MaxTokensPerDocument: profile.MaxTokensPerDocument,
		Egress: cohereapi.IdentifyEgress(profile.EgressPolicy)}, json.Deterministic(true))
	if err != nil {
		return "", errors.New("cohere rerank: policy identity encoding failed")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func New(profile Profile, secrets SecretResolver, resolver providerhttp.Resolver, supplied *http.Client) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("cohere rerank: HTTP client settings source is required")
	}
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	if cohereapi.IsNil(secrets) {
		return nil, errors.New("cohere rerank: named API-key resolver is required")
	}
	fingerprint, err := PolicyFingerprint(profile)
	if err != nil {
		return nil, err
	}
	transport, err := providerhttp.NewTransport(normalized.EgressPolicy, resolver)
	if err != nil {
		return nil, errors.New("cohere rerank: sealed egress policy is invalid")
	}
	isolated := *supplied
	isolated.Transport = transport
	isolated.CheckRedirect = providerhttp.RefuseRedirects
	isolated.Jar = nil
	isolated.Timeout = 0
	return &Client{profile: normalized, fingerprint: fingerprint, secrets: secrets, http: &isolated}, nil
}

func (client *Client) ProfileID() string {
	if client == nil {
		return ""
	}
	return client.profile.ID
}

func (client *Client) Model() Model {
	if client == nil {
		return ""
	}
	return client.profile.Model
}

func (client *Client) PolicyFingerprint() string {
	if client == nil {
		return ""
	}
	return client.fingerprint
}

func normalizeProfile(profile Profile) (Profile, error) {
	profile.EgressPolicy.AllowedCIDRs = slices.Clone(profile.EgressPolicy.AllowedCIDRs)
	profile.EgressPolicy.TLS.SPKISHA256 = slices.Clone(profile.EgressPolicy.TLS.SPKISHA256)
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = 30 * time.Second
	}
	if profile.MaxCandidates == 0 {
		profile.MaxCandidates = 100
	}
	if profile.MaxQueryBytes == 0 {
		profile.MaxQueryBytes = 4096
	}
	if profile.MaxExcerptBytes == 0 {
		profile.MaxExcerptBytes = 4096
	}
	if profile.MaxTotalExcerptBytes == 0 {
		profile.MaxTotalExcerptBytes = 4 << 20
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = 8 << 20
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = 8 << 20
	}
	if profile.MaxTokensPerDocument == 0 {
		profile.MaxTokensPerDocument = 4096
	}
	if !cohereapi.ValidToken(profile.ID, maximumTokenBytes) || (profile.Model != ModelPro && profile.Model != ModelFast) ||
		!cohereapi.ValidToken(profile.CompatibilityEpoch, maximumTokenBytes) || profile.ModelRevision != profile.CompatibilityEpoch ||
		!cohereapi.ValidToken(profile.SecretBinding, maximumTokenBytes) || profile.RequestTimeout <= 0 || profile.RequestTimeout > maximumTimeout ||
		profile.MaxCandidates < 1 || profile.MaxCandidates > maximumCandidates ||
		profile.MaxQueryBytes < 1 || profile.MaxQueryBytes > maximumQueryBytes ||
		profile.MaxExcerptBytes < 1 || profile.MaxExcerptBytes > maximumExcerptBytes ||
		profile.MaxTotalExcerptBytes < int64(profile.MaxExcerptBytes) || profile.MaxTotalExcerptBytes > maximumTotalExcerpts ||
		profile.MaxRequestBytes < 1 || profile.MaxRequestBytes > maximumRequestBytes ||
		profile.MaxResponseBytes < 1 || profile.MaxResponseBytes > maximumResponseBytes ||
		profile.MaxTokensPerDocument < 1 || profile.MaxTokensPerDocument > maximumTokensPerDoc {
		return Profile{}, errors.New("cohere rerank: profile is invalid")
	}
	if err := cohereapi.NormalizeEgress(&profile.EgressPolicy); err != nil {
		return Profile{}, err
	}
	return profile, nil
}
