package document

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

const suppliedTextContractV1 = "supplied-text/v1"

// SuppliedText is text attributed to the caller's named provider. The name is
// a claim and does not prove that the native document contains the same text.
type SuppliedText struct {
	Provider string
	Text     string
}

type suppliedTextArtifactV1 struct {
	ContractVersion string `json:"contract_version"`
	Provider        string `json:"provider"`
	Text            string `json:"text"`
}

// BuildSuppliedTextSourceEvidenceV1 retains the caller's exact text and builds
// generic, degraded source evidence. It does not infer pages or positions.
func BuildSuppliedTextSourceEvidenceV1(
	text SuppliedText, policy EvidencePolicy,
) (SourceEvidenceV1, RenditionArtifact, error) {
	if err := policy.validate(); err != nil {
		return SourceEvidenceV1{}, RenditionArtifact{}, err
	}
	if err := validateEvidenceIdentifier(text.Provider, "supplied text provider"); err != nil {
		return SourceEvidenceV1{}, RenditionArtifact{}, err
	}
	if err := validateEvidenceText(text.Text, "supplied text"); err != nil {
		return SourceEvidenceV1{}, RenditionArtifact{}, err
	}
	if strings.TrimSpace(text.Text) == "" {
		return SourceEvidenceV1{}, RenditionArtifact{}, errors.New("supplied text must contain non-whitespace text")
	}

	source := SourceEvidenceV1{
		ContractVersion: SourceEvidenceContractV1,
		Completeness:    EvidenceDegradedProvenance,
		Family:          "text",
		Omissions: []SourceEvidenceOmissionV1{{
			Field: "natural_provenance", Kind: EvidenceOmissionField,
			Reason: "supplied text has no page or position provenance",
		}},
		UnitKind: EvidenceUnitGeneric,
		Units: []SourceEvidenceUnitV1{{
			Order: 0, Text: text.Text,
			Locator: SourceEvidenceLocatorV1{
				Kind: EvidenceLocatorGeneric, IndexOrigin: EvidenceIndexOriginNone,
			},
		}},
	}
	if err := policy.validateSource(source); err != nil {
		return SourceEvidenceV1{}, RenditionArtifact{}, err
	}
	if _, err := validateSourceEvidenceV1(source, policy.maxDocumentChars); err != nil {
		return SourceEvidenceV1{}, RenditionArtifact{}, err
	}
	payload, err := canonical.Marshal(suppliedTextArtifactV1{
		ContractVersion: suppliedTextContractV1,
		Provider:        text.Provider,
		Text:            text.Text,
	})
	if err != nil {
		return SourceEvidenceV1{}, RenditionArtifact{}, err
	}
	digest := sha256.Sum256(payload)
	checksum := hex.EncodeToString(digest[:])
	source.Artifacts = []SourceEvidenceArtifactV1{{
		Pointer: "supplied-text.json", ProviderID: "supplied-text",
		Role: EvidenceArtifactStructured, SHA256: checksum,
	}}
	return source, RenditionArtifact{
		Role: EvidenceArtifactStructured, MediaType: "application/json",
		Payload: payload, SHA256: checksum,
	}, nil
}

// BuildSuppliedTextEvidenceV1 normalizes caller-supplied text under the same
// policy used to validate its source evidence.
func BuildSuppliedTextEvidenceV1(
	text SuppliedText, policy EvidencePolicy,
) (NormalizedEvidenceV1, RenditionArtifact, error) {
	source, artifact, err := BuildSuppliedTextSourceEvidenceV1(text, policy)
	if err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	evidence, err := NormalizeEvidenceV1(source, policy)
	if err != nil {
		return NormalizedEvidenceV1{}, RenditionArtifact{}, err
	}
	return evidence, artifact, nil
}
