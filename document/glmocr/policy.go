package glmocr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/ocr"
)

const (
	Provider = "glmocr"

	DefaultEndpoint       = "http://127.0.0.1:30004/glmocr/parse"
	DefaultModel          = "zai-org/GLM-OCR"
	DefaultServedModel    = "glm-ocr"
	DefaultModelRevision  = "ca5d8b3e287e52589e37c28385d9655ee4372f9d"
	DefaultSDKRevision    = "cef4d0ea120d1741f5cefe8985eee45f6c8eff1d"
	DefaultLayoutModel    = "PaddlePaddle/PP-DocLayoutV3_safetensors"
	DefaultLayoutRevision = "97d101e6db2642e162a1d05392d1b0231c91033e"

	MaxDocumentBytes = int64(64 << 20)
	MaxResponseBytes = int64(512 << 20)
	MaxUnits         = 5_000

	canonicalPolicyVersion = 1
)

// PolicyConfig fixes every input, output, and artifact bound that can affect
// local OCR evidence.
type PolicyConfig struct {
	Endpoint         string
	Model            string
	ServedModel      string
	ModelRevision    string
	SDKRevision      string
	LayoutModel      string
	LayoutRevision   string
	MaxDocumentBytes int64
	MaxResponseBytes int64
	MaxUnits         int
	NormalizePolicy  document.NormalizePolicy
}

// PolicyValues is a read-only copy of the effective local OCR policy.
type PolicyValues struct {
	Provider         string                           `json:"provider"`
	Endpoint         string                           `json:"endpoint"`
	Model            string                           `json:"model"`
	ServedModel      string                           `json:"served_model"`
	ModelRevision    string                           `json:"model_revision"`
	SDKRevision      string                           `json:"sdk_revision"`
	LayoutModel      string                           `json:"layout_model"`
	LayoutRevision   string                           `json:"layout_revision"`
	MaxDocumentBytes int64                            `json:"max_document_bytes"`
	MaxResponseBytes int64                            `json:"max_response_bytes"`
	MaxUnits         int                              `json:"max_units"`
	Normalization    document.NormalizePolicyIdentity `json:"normalization"`
}

// Policy is an immutable local GLM-OCR processing policy.
type Policy struct {
	values          PolicyValues
	normalizePolicy document.NormalizePolicy
	fingerprint     string
	identity        ocr.Identity
}

// NewPolicy validates a loopback-only endpoint and immutable artifact pins.
func NewPolicy(config PolicyConfig) (Policy, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.ServedModel == "" {
		config.ServedModel = DefaultServedModel
	}
	if config.ModelRevision == "" {
		config.ModelRevision = DefaultModelRevision
	}
	if config.SDKRevision == "" {
		config.SDKRevision = DefaultSDKRevision
	}
	if config.LayoutModel == "" {
		config.LayoutModel = DefaultLayoutModel
	}
	if config.LayoutRevision == "" {
		config.LayoutRevision = DefaultLayoutRevision
	}
	if err := validateEndpoint(config.Endpoint); err != nil {
		return Policy{}, err
	}
	if config.Model != DefaultModel || config.ServedModel != DefaultServedModel ||
		config.ModelRevision != DefaultModelRevision || config.SDKRevision != DefaultSDKRevision ||
		config.LayoutModel != DefaultLayoutModel || config.LayoutRevision != DefaultLayoutRevision {
		return Policy{}, errors.New("GLM-OCR policy artifacts must use the package-pinned identities")
	}
	if config.MaxDocumentBytes <= 0 || config.MaxDocumentBytes > MaxDocumentBytes ||
		config.MaxResponseBytes <= 0 || config.MaxResponseBytes > MaxResponseBytes ||
		config.MaxUnits <= 0 || config.MaxUnits > MaxUnits {
		return Policy{}, errors.New("GLM-OCR policy bounds are invalid")
	}
	normalization := config.NormalizePolicy.Identity()
	if normalization.Version == 0 {
		return Policy{}, errors.New("GLM-OCR normalization policy is invalid; use document.NewNormalizePolicy")
	}
	values := PolicyValues{
		Provider: Provider, Endpoint: config.Endpoint, Model: config.Model,
		ServedModel: config.ServedModel, ModelRevision: config.ModelRevision,
		SDKRevision: config.SDKRevision, LayoutModel: config.LayoutModel,
		LayoutRevision: config.LayoutRevision, MaxDocumentBytes: config.MaxDocumentBytes,
		MaxResponseBytes: config.MaxResponseBytes, MaxUnits: config.MaxUnits,
		Normalization: normalization,
	}
	payload := struct {
		PolicyValues

		Version int `json:"version"`
	}{Version: canonicalPolicyVersion, PolicyValues: values}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Policy{}, fmt.Errorf("encode GLM-OCR policy: %w", err)
	}
	digest := sha256.Sum256(encoded)
	identity, err := ocr.NewIdentity(Provider, config.Model, config.ModelRevision)
	if err != nil {
		return Policy{}, fmt.Errorf("configure GLM-OCR model identity: %w", err)
	}
	return Policy{
		values: values, normalizePolicy: config.NormalizePolicy,
		fingerprint: hex.EncodeToString(digest[:]), identity: identity,
	}, nil
}

func validateEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != "/glmocr/parse" || parsed.Port() == "" {
		return errors.New("GLM-OCR endpoint must be an explicit loopback HTTP /glmocr/parse URL")
	}
	host := parsed.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("GLM-OCR endpoint must use a loopback host")
		}
	}
	return nil
}

// Values returns every effective policy value.
func (p Policy) Values() PolicyValues { return p.values }

// Fingerprint returns the stable digest of endpoint, artifacts, and bounds.
func (p Policy) Fingerprint() string { return p.fingerprint }

// Identity returns the stable provider/model identity.
func (p Policy) Identity() ocr.Identity { return p.identity }

// NormalizePolicy returns the executable normalization policy.
func (p Policy) NormalizePolicy() document.NormalizePolicy { return p.normalizePolicy }
