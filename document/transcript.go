package document

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

const suppliedTranscriptContractV1 = "supplied-transcript/v1"

// SuppliedTranscript is caller-provided transcript text attributed to the
// provider that produced it. The attribution records a caller claim; this
// type does not verify provider identity or transcript authenticity.
type SuppliedTranscript struct {
	Provider string
	Text     string
}

type suppliedTranscriptArtifactV1 struct {
	ContractVersion string `json:"contract_version"`
	Provider        string `json:"provider"`
	Text            string `json:"text"`
}

// BuildTranscriptEvidenceV1 turns supplied transcript text into the existing
// provider-neutral evidence and retained transcript artifact contracts.
// Transcript text has generic audio provenance because this operation does
// not inspect audio or infer timing, speakers, or authenticity.
func BuildTranscriptEvidenceV1(
	transcript SuppliedTranscript, policy EvidencePolicy,
) (NormalizedEvidenceV1, RenditionArtifact, error) {
	if err := policy.validate(); err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	if err := validateEvidenceIdentifier(transcript.Provider, "transcript provider"); err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	if err := validateEvidenceText(transcript.Text, "transcript text"); err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	if strings.TrimSpace(transcript.Text) == "" {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, errors.New("transcript text must contain non-whitespace text")
	}

	source := SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceDegradedProvenance,
		Family:          "audio",
		Omissions: []SourceEvidenceOmissionV1{{
			Field: "natural_provenance", Kind: EvidenceOmissionField,
			Reason: "supplied transcript has no timing or speaker provenance",
		}},
		UnitKind: EvidenceUnitGeneric,
		Units: []SourceEvidenceUnitV1{{
			Order: 0, Text: transcript.Text,
			Locator: SourceEvidenceLocatorV1{
				Kind: EvidenceLocatorGeneric, IndexOrigin: EvidenceIndexOriginNone,
			},
		}},
	}
	if err := policy.validateSource(source); err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	if _, err := validateSourceEvidenceV1(source, policy.maxDocumentChars); err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}

	payload, err := canonical.Marshal(suppliedTranscriptArtifactV1{
		ContractVersion: suppliedTranscriptContractV1,
		Provider:        transcript.Provider,
		Text:            transcript.Text,
	})
	if err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	source.Artifacts = []SourceEvidenceArtifactV1{{
		Pointer: "transcript.json", ProviderID: "transcript",
		Role: EvidenceArtifactTranscript, SHA256: checksum,
	}}
	evidence, err := NormalizeEvidenceV1(source, policy)
	if err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	return evidence, RenditionArtifact{
		Role: EvidenceArtifactTranscript, MediaType: "application/json",
		Payload: payload, SHA256: checksum,
	}, nil
}
