// Package mediatranscript defines the retained canonical artifact for timed
// supplied and generated media transcripts.
package mediatranscript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	contractV1          = "media-transcript/v1"
	maxMetadataBytes    = 128
	maxSegments         = 25_000
	maxSegmentEndMS     = 86_400_000
	transcriptPointerV1 = "media-transcript.json"

	// MaxArtifactBytes is the largest encoded media-transcript/v1 artifact.
	MaxArtifactBytes = 16 << 20
)

// Segment is one ordered half-open media time span in milliseconds.
type Segment struct {
	EndMS   int64  `json:"end_ms"`
	Order   int    `json:"order"`
	Speaker string `json:"speaker,omitempty"`
	StartMS int64  `json:"start_ms"`
	Text    string `json:"text"`
}

// ArtifactV1 is a bounded, canonical timed transcript.
type ArtifactV1 struct {
	ContractVersion string    `json:"contract_version"`
	Language        string    `json:"language,omitempty"`
	Model           string    `json:"model,omitempty"`
	Origin          string    `json:"origin"`
	Provider        string    `json:"provider"`
	ProviderVersion string    `json:"provider_version,omitempty"`
	Segments        []Segment `json:"segments"`
}

// Marshal validates a timed transcript and returns its exact canonical bytes
// and SHA-256 identity.
func Marshal(artifact ArtifactV1) ([]byte, string, error) {
	if artifact.ContractVersion != contractV1 ||
		(artifact.Origin != "supplied" && artifact.Origin != "generated") {
		return nil, "", errors.New("invalid transcript identity")
	}
	for subject, value := range map[string]string{
		"provider": artifact.Provider, "provider version": artifact.ProviderVersion,
		"model": artifact.Model, "language": artifact.Language,
	} {
		if err := validateMetadata(value, subject, subject == "provider"); err != nil {
			return nil, "", err
		}
	}
	if len(artifact.Segments) == 0 || len(artifact.Segments) > maxSegments {
		return nil, "", errors.New("segment_limit")
	}
	total := 0
	for index, segment := range artifact.Segments {
		if segment.Order != index || segment.StartMS < 0 || segment.EndMS <= segment.StartMS ||
			segment.EndMS > maxSegmentEndMS ||
			(index > 0 && segment.StartMS < artifact.Segments[index-1].StartMS) ||
			!utf8.ValidString(segment.Text) || strings.TrimSpace(segment.Text) == "" ||
			strings.ContainsRune(segment.Text, '\x00') || !utf8.ValidString(segment.Speaker) ||
			len(segment.Speaker) > maxMetadataBytes || strings.ContainsRune(segment.Speaker, '\x00') {
			return nil, "", errors.New("invalid transcript segment")
		}
		width := len(segment.Text) + len(segment.Speaker)
		if width > MaxArtifactBytes-total {
			return nil, "", errors.New("transcript byte_limit")
		}
		total += width
	}
	raw, err := canonical.Marshal(artifact)
	if err != nil {
		return nil, "", err
	}
	if len(raw) > MaxArtifactBytes {
		return nil, "", errors.New("transcript byte_limit")
	}
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), nil
}

// Unmarshal accepts only the exact canonical byte form of a bounded artifact.
func Unmarshal(raw []byte) (ArtifactV1, error) {
	var artifact ArtifactV1
	if len(raw) > MaxArtifactBytes {
		return artifact, errors.New("transcript byte_limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&artifact); err != nil {
		return ArtifactV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("transcript has trailing content")
		}
		return ArtifactV1{}, err
	}
	canonicalBytes, _, err := Marshal(artifact)
	if err != nil {
		return ArtifactV1{}, err
	}
	if !bytes.Equal(raw, canonicalBytes) {
		return ArtifactV1{}, errors.New("transcript is not canonical")
	}
	return artifact, nil
}

// Build maps a timed artifact to provider-neutral segment evidence while
// retaining the exact artifact bytes that support it.
func Build(
	artifact ArtifactV1,
	family string,
	policy document.EvidencePolicy,
) (document.SourceEvidenceV1, document.RenditionArtifact, error) {
	raw, checksum, err := Marshal(artifact)
	if err != nil {
		return document.SourceEvidenceV1{}, document.RenditionArtifact{}, err
	}
	source := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Family:          family,
		Completeness:    document.EvidencePartial,
		UnitKind:        document.EvidenceUnitSegment,
		Omissions: []document.SourceEvidenceOmissionV1{{
			Kind: document.EvidenceOmissionField, Field: "non_speech_content",
			Reason: "speech transcript does not describe non-speech content",
		}},
		Artifacts: []document.SourceEvidenceArtifactV1{{
			ProviderID: "transcript", Pointer: transcriptPointerV1,
			Role: document.EvidenceArtifactTranscript, SHA256: checksum,
		}},
		Units: make([]document.SourceEvidenceUnitV1, len(artifact.Segments)),
	}
	for index, segment := range artifact.Segments {
		source.Units[index] = document.SourceEvidenceUnitV1{
			Order: segment.Order, Speaker: segment.Speaker, Text: segment.Text,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorSegment, IndexOrigin: document.EvidenceIndexOriginZero,
				Start: segment.StartMS, End: segment.EndMS,
			},
		}
	}
	if _, err := document.NormalizeEvidenceV1(source, policy); err != nil {
		return document.SourceEvidenceV1{}, document.RenditionArtifact{}, err
	}
	return source, document.RenditionArtifact{
		Role: document.EvidenceArtifactTranscript, MediaType: "application/json",
		Payload: raw, SHA256: checksum,
	}, nil
}

func validateMetadata(value, subject string, required bool) error {
	if required && value == "" {
		return fmt.Errorf("transcript %s is required", subject)
	}
	if value == "" || (utf8.ValidString(value) && len(value) <= maxMetadataBytes &&
		!strings.ContainsRune(value, '\x00')) {
		return nil
	}
	return fmt.Errorf("transcript %s must be bounded UTF-8", subject)
}
