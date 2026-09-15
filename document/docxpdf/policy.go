// Package docxpdf converts byte-verified DOCX sources to PDF with a pinned local LibreOffice renderer.
package docxpdf

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"

	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/internal/canonical"
)

// ConverterVersion identifies the renderer argument and conversion contract.
const ConverterVersion = "docxpdf-v1-libreoffice-headless-writer_pdf_Export"

// MaxExecutableBytes is the largest renderer executable accepted by a policy.
const MaxExecutableBytes = int64(64 << 20)

// Renderer identifies the installed executable and its operator-declared runtime.
type Renderer struct {
	Executable       string
	ExecutableSHA256 string
	RuntimeIdentity  string
}

// Limits bounds one conversion. NewPolicy permits tightening the defaults only.
type Limits struct {
	MaxSourceBytes int64         `json:"max_source_bytes"`
	MaxPDFBytes    int64         `json:"max_pdf_bytes"`
	MaxPages       int           `json:"max_pages"`
	Timeout        time.Duration `json:"timeout_ns"`
}

// DefaultLimits returns the hard ceilings for a conversion.
func DefaultLimits() Limits {
	return Limits{
		MaxSourceBytes: 50 << 20,
		MaxPDFBytes:    50 << 20,
		MaxPages:       1000,
		Timeout:        10 * time.Minute,
	}
}

// Policy captures an immutable renderer and conversion contract. Its zero value is invalid.
type Policy struct {
	renderer    Renderer
	limits      Limits
	fingerprint string
}

// NewPolicy validates the renderer, limits and complete conversion identity.
func NewPolicy(renderer Renderer, limits Limits) (Policy, error) {
	if !filepath.IsAbs(renderer.Executable) || filepath.Clean(renderer.Executable) != renderer.Executable {
		return Policy{}, errors.New("DOCX renderer executable must be an absolute clean path")
	}
	if _, err := providerutil.LoadPinnedExecutable(renderer.Executable, renderer.ExecutableSHA256, MaxExecutableBytes); err != nil {
		return Policy{}, errors.New("DOCX renderer executable is not pinned")
	}
	if err := validateRuntimeIdentity(renderer.RuntimeIdentity); err != nil {
		return Policy{}, err
	}
	ceiling := DefaultLimits()
	if limits.MaxSourceBytes <= 0 || limits.MaxSourceBytes > ceiling.MaxSourceBytes ||
		limits.MaxPDFBytes <= 0 || limits.MaxPDFBytes > ceiling.MaxPDFBytes ||
		limits.MaxPages <= 0 || limits.MaxPages > ceiling.MaxPages ||
		limits.Timeout <= 0 || limits.Timeout > ceiling.Timeout {
		return Policy{}, errors.New("DOCX PDF limits must be positive and within DefaultLimits")
	}
	encoded, err := canonical.Marshal(struct {
		Version          string   `json:"version"`
		Arguments        []string `json:"arguments"`
		Executable       string   `json:"executable"`
		ExecutableSHA256 string   `json:"executable_sha256"`
		RuntimeIdentity  string   `json:"runtime_identity"`
		Limits           struct {
			MaxSourceBytes int64 `json:"max_source_bytes"`
			MaxPDFBytes    int64 `json:"max_pdf_bytes"`
			MaxPages       int   `json:"max_pages"`
			Timeout        int64 `json:"timeout_ns"`
		} `json:"limits"`
	}{
		Version:          ConverterVersion,
		Arguments:        rendererArguments("WORK"),
		Executable:       renderer.Executable,
		ExecutableSHA256: renderer.ExecutableSHA256,
		RuntimeIdentity:  renderer.RuntimeIdentity,
		Limits: struct {
			MaxSourceBytes int64 `json:"max_source_bytes"`
			MaxPDFBytes    int64 `json:"max_pdf_bytes"`
			MaxPages       int   `json:"max_pages"`
			Timeout        int64 `json:"timeout_ns"`
		}{
			MaxSourceBytes: limits.MaxSourceBytes,
			MaxPDFBytes:    limits.MaxPDFBytes,
			MaxPages:       limits.MaxPages,
			Timeout:        int64(limits.Timeout),
		},
	})
	if err != nil {
		return Policy{}, err
	}
	return Policy{renderer: renderer, limits: limits, fingerprint: digest(encoded)}, nil
}

// Fingerprint returns the conversion identity; an invalid policy returns empty.
func (p Policy) Fingerprint() string { return p.fingerprint }

func validateRuntimeIdentity(value string) error {
	if len(value) == 0 || len(value) > 256 {
		return errors.New("DOCX renderer runtime identity must be 1-256 printable ASCII bytes")
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7e {
			return errors.New("DOCX renderer runtime identity must be 1-256 printable ASCII bytes")
		}
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
