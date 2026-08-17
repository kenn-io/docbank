package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/safefileio"
)

const testSuccessfulResponse = `{"model":"mistral-ocr-4-0","pages":[{"index":0}],"usage_info":{"pages_processed":1}}`

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
				result.ReasonCode = reasonBoundUnitsMismatch
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
	makePrivateDirectory(t, directory)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF, ExpectedSize: int64(len(content)),
		ExpectedSHA256: hex.EncodeToString(digest[:]), MaxSpoolBytes: policy.values.MaxDocumentBytes,
		MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	return prepared
}

func makePrivateDirectory(t *testing.T, directory string) {
	t.Helper()
	require.NoError(t, safefileio.EnsurePrivateDir(directory))
}

func requireOnlySpoolReservationFile(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, spoolReservationFile, entries[0].Name())
}

func newProbeFixtureDestination(t *testing.T, name string) string {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "fixture-output")
	makePrivateDirectory(t, parent)
	return filepath.Join(parent, name)
}

func testPDF(label string) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	}
	var output bytes.Buffer
	_, _ = fmt.Fprintf(&output, "%%PDF-1.4\n%%%s\n", hex.EncodeToString([]byte(label)))
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func testPDFXRefStream() []byte {
	var output bytes.Buffer
	output.WriteString("%PDF-1.5\n")
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "1 0 obj\n<< /Type /XRef /Size 2 /Root 2 0 R /W [1 2 1] /Length 4 >>\nstream\n")
	output.Write([]byte{0, 0, 0, 0})
	_, _ = fmt.Fprintf(&output, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xref)
	return output.Bytes()
}

type errorReader struct {
	err   error
	reads *int
}

func (r errorReader) Read([]byte) (int, error) {
	if r.reads != nil {
		(*r.reads)++
	}
	return 0, r.err
}
