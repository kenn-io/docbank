package ocr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/docbank/document"
)

const identityVersion = 1

var providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Processor converts one authoritative source into normalized OCR evidence.
// Process takes ownership of Source.Content and closes it on every path.
type Processor interface {
	Identity() Identity
	Process(ctx context.Context, source Source) (Result, error)
}

// Source describes one authoritative document stream. NewSource validates the
// metadata before a provider can take ownership of Content.
type Source struct {
	Content   io.ReadCloser
	MediaType string
	Size      int64
	SHA256    string
}

// NewSource validates authoritative metadata without reading content.
func NewSource(content io.ReadCloser, mediaType string, size int64, sha256Hex string) (Source, error) {
	if content == nil {
		return Source{}, errors.New("OCR source content is required")
	}
	parsedType, parameters, err := mime.ParseMediaType(mediaType)
	if err != nil || parsedType == "" || len(parameters) != 0 {
		return Source{}, errors.New("OCR source media type must be canonical and have no parameters")
	}
	if size <= 0 {
		return Source{}, errors.New("OCR source size must be positive")
	}
	if len(sha256Hex) != sha256.Size*2 || sha256Hex != strings.ToLower(sha256Hex) {
		return Source{}, errors.New("OCR source requires a lowercase SHA-256")
	}
	if _, err := hex.DecodeString(sha256Hex); err != nil {
		return Source{}, errors.New("OCR source requires a lowercase SHA-256")
	}
	return Source{Content: content, MediaType: parsedType, Size: size, SHA256: sha256Hex}, nil
}

// Validate checks that Source was constructed with complete canonical metadata.
func (s Source) Validate() error {
	if s.Content == nil {
		return errors.New("OCR source content is required")
	}
	parsedType, parameters, err := mime.ParseMediaType(s.MediaType)
	if err != nil || parsedType != s.MediaType || len(parameters) != 0 {
		return errors.New("OCR source media type must be canonical and have no parameters")
	}
	if s.Size <= 0 {
		return errors.New("OCR source size must be positive")
	}
	if len(s.SHA256) != sha256.Size*2 || s.SHA256 != strings.ToLower(s.SHA256) {
		return errors.New("OCR source requires a lowercase SHA-256")
	}
	if _, err := hex.DecodeString(s.SHA256); err != nil {
		return errors.New("OCR source requires a lowercase SHA-256")
	}
	return nil
}

// Identity is the immutable provider/model identity attached to OCR evidence.
// Revision is an immutable model artifact reference, not a mutable tag.
type Identity struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Revision    string `json:"revision"`
	Fingerprint string `json:"fingerprint"`
}

// NewIdentity returns an identity whose fingerprint is stable canonical JSON.
func NewIdentity(provider, model, revision string) (Identity, error) {
	if !providerPattern.MatchString(provider) {
		return Identity{}, errors.New("OCR provider identity is invalid")
	}
	if invalidIdentityPart(model) || invalidIdentityPart(revision) {
		return Identity{}, errors.New("OCR model identity is invalid")
	}
	payload := struct {
		Version  int    `json:"version"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Revision string `json:"revision"`
	}{Version: identityVersion, Provider: provider, Model: model, Revision: revision}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Identity{}, fmt.Errorf("encode OCR model identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return Identity{
		Provider: provider, Model: model, Revision: revision,
		Fingerprint: hex.EncodeToString(digest[:]),
	}, nil
}

func invalidIdentityPart(value string) bool {
	return value == "" || value != strings.TrimSpace(value) || len(value) > 512 || strings.ContainsAny(value, "\r\n\x00")
}

// Result contains transient provider evidence and its normalized equivalent.
// Source and Structure may contain full provider output and must not be
// persisted unless an application explicitly adopts that privacy posture.
type Result struct {
	Source            document.SourceDocument
	Document          document.NormalizedDocument
	Structure         []UnitStructure
	Identity          Identity
	PolicyFingerprint string
	UnitsProcessed    int
	ProviderBytes     *int64
	Metrics           RequestMetrics
	CleanupError      error
}

// UnitStructure preserves provider-reported reading order and element kinds.
type UnitStructure struct {
	UnitIndex int
	Elements  []Element
}

// Element is one ordered heading, text, table, formula, or image region.
type Element struct {
	Index    int
	Kind     string
	Markdown string
	Bounds   *ElementBounds
}

// ElementBounds contains normalized 0-1000 provider coordinates.
type ElementBounds struct {
	Left   int
	Top    int
	Right  int
	Bottom int
}

// RequestMetrics describes provider HTTP work for one logical source.
type RequestMetrics struct {
	Requests int
	Retries  int
	Latency  time.Duration
}

// ErrorKind is the provider-neutral scheduling and reporting classification.
type ErrorKind string

const (
	ErrorInvalidInput      ErrorKind = "invalid_input"
	ErrorCapacity          ErrorKind = "capacity"
	ErrorTransient         ErrorKind = "transient"
	ErrorRejected          ErrorKind = "rejected"
	ErrorResponseTooLarge  ErrorKind = "response_too_large"
	ErrorCapabilityChanged ErrorKind = "capability_changed"
	ErrorMalformedOutput   ErrorKind = "malformed_output"
)

// ProviderError retains a provider cause, stable kind, and request accounting.
type ProviderError struct {
	Kind    ErrorKind
	Metrics RequestMetrics
	Cause   error
}

func (e *ProviderError) Error() string {
	if e == nil || e.Cause == nil {
		return "OCR provider error"
	}
	return e.Cause.Error()
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ErrorKindOf returns the first provider-neutral classification in err.
func ErrorKindOf(err error) ErrorKind {
	if providerErr, ok := errors.AsType[*ProviderError](err); ok {
		return providerErr.Kind
	}
	return ""
}

// MetricsFromError recovers request accounting from a failed Process call.
func MetricsFromError(err error) RequestMetrics {
	if providerErr, ok := errors.AsType[*ProviderError](err); ok {
		return providerErr.Metrics
	}
	return RequestMetrics{}
}

// IsRetryable reports whether an application may schedule the source again.
func IsRetryable(err error) bool {
	switch ErrorKindOf(err) {
	case ErrorCapacity, ErrorTransient:
		return true
	default:
		return false
	}
}
