package embedding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EgressPurpose distinguishes privacy approvals that must not be reused for a
// different class of plaintext.
type EgressPurpose string

const (
	EgressDistillation      EgressPurpose = "document_distillation"
	EgressDocumentEmbedding EgressPurpose = "document_embedding"
	EgressQueryEmbedding    EgressPurpose = "query_embedding"
)

// EgressIdentity contains privacy-relevant provider settings. Credentials are
// deliberately excluded so rotation does not invalidate consent.
type EgressIdentity struct {
	Purpose       EgressPurpose `json:"purpose"`
	Provider      string        `json:"provider"`
	Endpoint      string        `json:"endpoint"`
	Model         string        `json:"model"`
	ModelRevision string        `json:"model_revision,omitempty"`
}

// VectorSpaceIdentity contains the endpoint-independent settings required to
// compare and reuse vectors safely. Endpoint belongs to EgressIdentity because
// changing destination requires fresh consent without necessarily requiring a
// corpus rebuild.
type VectorSpaceIdentity struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ModelRevision string `json:"model_revision"`
	Dimension     int    `json:"dimension"`
	Normalization string `json:"normalization"`
}

// CanonicalJSON validates and returns the canonical vector-space identity.
func (identity VectorSpaceIdentity) CanonicalJSON() ([]byte, error) {
	if identity.Provider == "" || identity.Model == "" || identity.ModelRevision == "" || identity.Dimension < 1 || identity.Normalization == "" {
		return nil, errors.New("vector space provider, model, revision, dimension, and normalization are required")
	}
	for _, value := range [...]struct{ name, text string }{
		{name: "vector space provider", text: identity.Provider},
		{name: "vector space model", text: identity.Model},
		{name: "vector space model revision", text: identity.ModelRevision},
		{name: "vector space normalization", text: identity.Normalization},
	} {
		if err := validateIdentityText(value.name, value.text); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode vector space identity: %w", err)
	}
	return encoded, nil
}

// Fingerprint returns lowercase SHA-256 over CanonicalJSON.
func (identity VectorSpaceIdentity) Fingerprint() (string, error) {
	encoded, err := identity.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return fingerprint(encoded), nil
}

// CanonicalJSON validates and canonicalizes an egress identity. The endpoint
// must identify an exact HTTP(S) destination without credentials, query, or
// fragment components.
func (identity EgressIdentity) CanonicalJSON() ([]byte, error) {
	if identity.Purpose != EgressDistillation && identity.Purpose != EgressDocumentEmbedding && identity.Purpose != EgressQueryEmbedding {
		return nil, fmt.Errorf("unsupported embedding egress purpose %q", identity.Purpose)
	}
	if identity.Provider == "" || identity.Model == "" {
		return nil, errors.New("embedding egress provider and model are required")
	}
	for _, value := range [...]struct{ name, text string }{
		{name: "embedding egress provider", text: identity.Provider},
		{name: "embedding egress model", text: identity.Model},
		{name: "embedding egress model revision", text: identity.ModelRevision},
	} {
		if err := validateIdentityText(value.name, value.text); err != nil {
			return nil, err
		}
	}
	endpoint, err := canonicalEndpoint(identity.Endpoint)
	if err != nil {
		return nil, err
	}
	identity.Endpoint = endpoint
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode embedding egress identity: %w", err)
	}
	return encoded, nil
}

// Fingerprint returns lowercase SHA-256 over CanonicalJSON.
func (identity EgressIdentity) Fingerprint() (string, error) {
	encoded, err := identity.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return fingerprint(encoded), nil
}

func canonicalEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse embedding egress endpoint: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("embedding egress endpoint must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("embedding egress endpoint cannot contain credentials, query, or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	hostname := parsed.Hostname()
	address, _, _ := strings.Cut(hostname, "%")
	if net.ParseIP(address) == nil {
		parsed.Host = strings.Replace(parsed.Host, hostname, strings.ToLower(hostname), 1)
	}
	if parsed.RawPath == "" && parsed.Path == "/" {
		parsed.Path = ""
	}
	return parsed.String(), nil
}

func validateIdentityText(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s contains invalid UTF-8", name)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains a control character", name)
	}
	return nil
}

func fingerprint(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
