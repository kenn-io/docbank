package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func testPolicy(t *testing.T, maxDocumentBytes int64, maxUnits int) Policy {
	t.Helper()
	normalizePolicy, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	policy, err := NewPolicy(PolicyConfig{
		Region: defaultRegion, Model: defaultModel, Retention: "zdr", Training: "opted-out",
		MaxDocumentBytes: maxDocumentBytes, MaxResponseBytes: 1 << 20, MaxUnits: maxUnits,
		ExtractHeader: true, ExtractFooter: true, NormalizePolicy: normalizePolicy,
	})
	require.NoError(t, err)
	return policy
}

func syntheticManifest(t *testing.T, policy Policy, pdfBound bool) CapabilityManifest {
	t.Helper()
	manifest := CapabilityManifest{
		SchemaVersion: CapabilitySchemaVersion, ProbeFixtureContract: probeFixtureContract,
		ObservedOn: "2026-08-16", Endpoint: defaultEndpoint, Region: defaultRegion,
		RequestedModel: defaultModel, MaxUnits: policy.values.MaxUnits,
		Results: make([]CapabilityResult, 0, len(candidateFormats)),
	}
	for _, candidate := range candidateFormats {
		digest := sha256.Sum256([]byte(candidate.ID))
		result := CapabilityResult{
			FormatID: candidate.ID, Family: candidate.Family, MediaType: candidate.MediaType,
			UnitKind: candidate.UnitKind, Status: ProbeStatusPassed,
			FixtureDigest: hex.EncodeToString(digest[:])[:16],
			RequestFingerprint: requestFingerprint(candidate, probeRequestOptions(
				candidate, manifest.MaxUnits, policy.values.ExtractHeader, policy.values.ExtractFooter,
			)),
			ReturnedModel: defaultModel, UnitCount: 1, UnitsProcessed: 1,
			UnitBoundMethod: UnitBoundNone,
		}
		if candidate.ID == "pdf" {
			result.UnitCount = 2
			result.UnitsProcessed = 2
			if pdfBound {
				result.UnitBoundMethod = UnitBoundProviderRequest
				result.FixtureUnits = 2
				result.BoundRequestedUnits = 1
				result.BoundUnitsProcessed = 1
			} else {
				result.ReasonCode = reasonBoundUnverified
			}
		}
		manifest.Results = append(manifest.Results, result)
	}
	require.NoError(t, manifest.ValidateComplete())
	return manifest
}

func prepareTestDocument(
	t *testing.T,
	policy Policy,
	content []byte,
) *PreparedDocument {
	t.Helper()
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(directory, 0o700))
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF, ExpectedSize: int64(len(content)),
		ExpectedSHA256: hex.EncodeToString(digest[:]), MaxSpoolBytes: policy.values.MaxDocumentBytes,
		MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	return prepared
}
