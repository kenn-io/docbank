// Package mistraltest provides deterministic synthetic documents and
// capability evidence for applications that test Mistral integrations.
//
// Synthetic manifests are fabricated test inputs. They are not observations
// from an authenticated provider probe and must not be used as production
// upload authority.
package mistraltest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/internal/probecontract"
	"go.kenn.io/docbank/document/mistral/internal/testfixture"
)

// SyntheticManifest returns a complete, validated manifest for application
// tests. When pdfBound is true, PDF has provider-request upload authority;
// every other format remains unbounded.
func SyntheticManifest(policy mistral.Policy, pdfBound bool) (mistral.CapabilityManifest, error) {
	values := policy.Values()
	if values.Provider == "" {
		return mistral.CapabilityManifest{}, errors.New("mistraltest requires a valid policy")
	}
	manifest := mistral.CapabilityManifest{
		SchemaVersion:        mistral.CapabilitySchemaVersion,
		ProbeFixtureContract: probecontract.FixtureVersion,
		ObservedOn:           "2026-08-16", Endpoint: values.Endpoint, Region: values.Region,
		RequestedModel: values.Model, MaxUnits: values.MaxUnits,
		Results: make([]mistral.CapabilityResult, 0, len(mistral.CandidateFormats())),
	}
	for _, candidate := range mistral.CandidateFormats() {
		digest := sha256.Sum256([]byte(candidate.ID))
		options := probecontract.RequestOptions{
			ExtractHeader: values.ExtractHeader, ExtractFooter: values.ExtractFooter,
		}
		if candidate.Family == "pdf" {
			options.Pages = fmt.Sprintf("0-%d", manifest.MaxUnits-1)
		}
		result := mistral.CapabilityResult{
			FormatID: candidate.ID, Family: candidate.Family, MediaType: candidate.MediaType,
			UnitKind: candidate.UnitKind, Status: mistral.ProbeStatusPassed,
			FixtureDigest: hex.EncodeToString(digest[:])[:16],
			RequestFingerprint: probecontract.Fingerprint(values.Endpoint, values.Model, probecontract.Candidate{
				ID: candidate.ID, Family: candidate.Family, MediaType: candidate.MediaType,
				UnitKind: candidate.UnitKind,
			}, options),
			ReturnedModel: values.Model, UnitCount: 1, UnitsProcessed: 1,
			UnitBoundMethod: mistral.UnitBoundNone,
		}
		if candidate.ID == "pdf" {
			result.UnitCount = 2
			result.UnitsProcessed = 2
			if pdfBound {
				result.UnitBoundMethod = mistral.UnitBoundProviderRequest
				result.FixtureUnits = 2
				result.BoundRequestedUnits = 1
				result.BoundUnitsProcessed = 1
			} else {
				result.ReasonCode = probecontract.ReasonBoundUnitsMismatch
			}
		}
		manifest.Results = append(manifest.Results, result)
	}
	if err := manifest.ValidateComplete(); err != nil {
		return mistral.CapabilityManifest{}, fmt.Errorf("build synthetic Mistral manifest: %w", err)
	}
	return manifest, nil
}

// MinimalPDF returns deterministic one-page PDF bytes for format, staging, and
// application orchestration tests. It is not a capability-probe fixture.
func MinimalPDF(label string) []byte { return testfixture.MinimalPDF(label) }
