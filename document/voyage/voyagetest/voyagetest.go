// Package voyagetest provides synthetic capability evidence for applications
// that test Voyage integrations.
//
// Synthetic manifests are fabricated test inputs. They are not observations
// from an authenticated provider probe and must not be used as production
// upload authority. Use go.kenn.io/docbank/document/media/mediatest for
// synthetic media bytes.
package voyagetest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/internal/probecontract"
)

// SyntheticManifest returns a complete, validated manifest for application
// tests. The listed capability identifiers pass; every other capability is
// recorded as a scrubbed transient failure. With no identifiers, every
// capability passes.
func SyntheticManifest(policy voyage.Policy, passed ...string) (voyage.CapabilityManifest, error) {
	values := policy.Values()
	if values.Provider == "" {
		return voyage.CapabilityManifest{}, errors.New("voyagetest requires a valid policy")
	}
	capabilities := voyage.Capabilities()
	manifest := voyage.CapabilityManifest{
		SchemaVersion: voyage.CapabilitySchemaVersion, ProbeFixtureContract: voyage.ProbeFixtureContract,
		ObservedOn: "2026-08-18", Endpoint: values.Endpoint, Model: values.Model,
		Dimension: values.Dimension, MaxBatchItems: values.MaxBatchItems,
		Results: make([]voyage.CapabilityResult, 0, len(capabilities)),
	}
	for _, capability := range capabilities {
		digest := sha256.Sum256([]byte(capability.ID))
		result := voyage.CapabilityResult{
			CapabilityID:  capability.ID,
			Status:        voyage.ProbeStatusPassed,
			FixtureDigest: hex.EncodeToString(digest[:])[:16],
			RequestFingerprint: probecontract.Fingerprint(
				values.Endpoint, values.Model, values.Dimension, capability.ID, capability.InputType,
			),
		}
		if len(passed) > 0 && !slices.Contains(passed, capability.ID) {
			result.Status = voyage.ProbeStatusFailed
			result.ReasonCode = voyage.ReasonTransientExhausted
		}
		manifest.Results = append(manifest.Results, result)
	}
	if err := manifest.ValidateComplete(); err != nil {
		return voyage.CapabilityManifest{}, fmt.Errorf("build synthetic Voyage manifest: %w", err)
	}
	return manifest, nil
}
