// Package providerutil contains shared implementation details for rendition
// provider adapters.
package providerutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
)

const maxConsecutiveEmptyReads = 100

// CloneDescriptor returns a descriptor whose slices do not alias the input.
func CloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

// AllowsStructured reports whether an authorization can retain one structured artifact.
func AllowsStructured(authorization document.RenditionAuthorization) bool {
	return slices.Contains(authorization.AllowedArtifactRoles, document.EvidenceArtifactStructured) &&
		authorization.MaxArtifacts > 0 && authorization.MaxArtifactBytes > 0
}

// DegradedEvidence builds one generic Markdown evidence unit with an explicit
// natural-provenance omission.
func DegradedEvidence(family, markdown, reason string) document.SourceEvidenceV1 {
	return document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceDegradedProvenance,
		Family:          family,
		UnitKind:        document.EvidenceUnitGeneric,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "natural_provenance", Reason: reason,
		}},
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: markdown,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorGeneric, IndexOrigin: document.EvidenceIndexOriginNone,
			},
		}},
	}
}

// InjectsDocbankFrontmatter reports whether provider Markdown claims Docbank's
// reserved sanitized-Markdown marker.
func InjectsDocbankFrontmatter(markdown []byte) bool {
	frontmatter := bytes.HasPrefix(markdown, []byte("---\n")) ||
		bytes.HasPrefix(markdown, []byte("---\r\n")) ||
		bytes.HasPrefix(markdown, []byte("---\r"))
	return frontmatter && bytes.Contains(markdown, []byte("docbank-sanitized-markdown/v1"))
}

// ReadAuthorizedUpload reads and verifies the exact authorized bytes. The
// caller owns interrupting upload when ctx ends so blocked reads can stop.
func ReadAuthorizedUpload(
	ctx context.Context, upload io.Reader, metadata document.AuthorizedUploadMetadata, providerName string,
) ([]byte, error) {
	limited := &io.LimitedReader{R: upload, N: metadata.ByteLength + 1}
	data := make([]byte, 0, min(metadata.ByteLength, 32<<10))
	buffer := make([]byte, 32<<10)
	emptyReads := 0
	for limited.N > 0 {
		if err := ctx.Err(); err != nil {
			return nil, ClassifiedError(providerName, document.RenditionErrorCanceled,
				providerName+" rendering canceled", err)
		}
		count, err := limited.Read(buffer)
		if count > 0 {
			data = append(data, buffer[:count]...)
			emptyReads = 0
		}
		switch {
		case errors.Is(err, io.EOF):
			limited.N = 0
		case err != nil:
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, ClassifiedError(providerName, document.RenditionErrorCanceled,
					providerName+" rendering canceled", contextErr)
			}
			return nil, ClassifiedError(providerName, document.RenditionErrorTransient,
				"could not read the authorized upload", err)
		case count == 0:
			emptyReads++
			if emptyReads >= maxConsecutiveEmptyReads {
				return nil, ClassifiedError(providerName, document.RenditionErrorTransient,
					"authorized upload stopped making progress", io.ErrNoProgress)
			}
		}
	}
	if int64(len(data)) != metadata.ByteLength || SHA256Hex(data) != metadata.SHA256 {
		return nil, ClassifiedError(providerName, document.RenditionErrorPolicyRejected,
			"authorized upload identity mismatch", nil)
	}
	return data, nil
}

// Wait blocks for delay or returns the context error.
func Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of value.
func SHA256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// ClassifiedError constructs a provider error with a stable public class and
// a provider-specific private cause.
func ClassifiedError(
	providerName string, code document.RenditionErrorCode, message string, cause error,
) error {
	if cause == nil {
		cause = errors.New(message)
	} else {
		cause = fmt.Errorf("%s: %w", message, cause)
	}
	providerError, err := document.NewRenditionProviderError(code, 0, cause)
	if err == nil {
		return providerError
	}
	fallback, fallbackErr := document.NewRenditionProviderError(document.RenditionErrorMalformedEvidence,
		0, fmt.Errorf("%s returned an invalid error: %w", providerName, err))
	if fallbackErr == nil {
		return fallback
	}
	return errors.Join(err, fallbackErr)
}
