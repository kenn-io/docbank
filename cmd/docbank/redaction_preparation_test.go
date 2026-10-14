package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
)

func TestPDFRedactionSourcePreparation(t *testing.T) {
	// The CLI owns global flags; PDF readers also run sequentially to bound RSS.
	temporary := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, temporary)
	}
	data, identity := downloadRedactionSource(t)
	// From here on, input comes only from the verified download and its receipt.
	frames, err := pagerender.InspectSource(data, identity, "application/pdf")
	require.NoError(t, err)
	require.Len(t, frames, 1)
	require.Equal(t, identity, frames[0].Source)
	require.EqualValues(t, 85000, frames[0].Width)
	require.EqualValues(t, 110000, frames[0].Height)
	require.Zero(t, frames[0].Rotation)
	_, frameHash, err := document.MarshalPageFrameV1(frames[0])
	require.NoError(t, err)
	text := readRedactionPDFText(t, data)
	require.Equal(t, "SECRET KEEP SECRET", text)
	policy, err := document.NewEvidencePolicy(1 << 20)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{Text: text,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
	}, policy)
	require.NoError(t, err)
	source := pdfproduction.Source{
		Reader: bytes.NewReader(data), Size: identity.Size, SHA256: identity.SHA256,
	}
	// The PDF worker's RSS ceiling counts the whole test binary, so return the
	// freed heap from the daemon, earlier tests, and the text read to the OS before measuring.
	debug.FreeOSMemory()
	mapping, err := production.Align(t.Context(), production.AlignmentInput{
		PDF: source, Pages: frames, Evidence: evidence, EvidenceSHA256: evidence.Checksum,
	})
	require.NoError(t, err)
	require.Equal(t, "SECRET KEEP SECRET", strings.TrimSpace(mapping.Text))
	require.Empty(t, mapping.Gaps)
	require.Equal(t, identity.SHA256, mapping.PDFSHA256)
	require.Equal(t, evidence.Checksum, mapping.EvidenceSHA256)
	require.Len(t, mapping.Pages, 1)
	require.Equal(t, frameHash, mapping.Pages[0].FrameSHA256)
	decision := redaction.Decision{
		ID: "00000000-0000-4000-8000-000000000001", Action: "redact",
		MemberID: "00000000-0000-4000-8000-000000000002",
		Selector: redaction.Selector{Kind: "rectangle", MapSHA256: mapping.SHA256,
			Boxes: []redaction.Box{{Page: 1, FrameSHA256: frameHash,
				X0: 9000, Y0: 7800, X1: 17000, Y1: 11000}}},
	}
	plan, err := redaction.Resolve(mapping, "redact_selected",
		[]redaction.Decision{decision}, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	sidecar, err := redaction.Text(plan)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(sidecar))

	output := filepath.Join(t.TempDir(), "redacted.pdf")
	artifact, original := writeRedactionPDF(t, output, source, plan)
	saved, err := os.ReadFile(output)
	require.NoError(t, err)
	verifyRedactionPDF(t, saved, sidecar, artifact)
	require.Equal(t, "[REDACTED] KEEP SECRET",
		strings.Join(strings.Fields(readRedactionPDFText(t, saved)), " "))
	assertRedactionPixels(t, saved, original)
}

func downloadRedactionSource(t *testing.T) ([]byte, document.PageSource) {
	t.Helper()
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	stop := startServe(t)
	waitForDaemon(t, vault)
	original := redactionSourcePDF(t)
	input := filepath.Join(t.TempDir(), "source.pdf")
	require.NoError(t, os.WriteFile(input, original, 0o600))
	_, err := runCLI(t, "add", input, "--dest", "/inbox", "--json")
	require.NoError(t, err)
	statJSON, err := runCLI(t, "stat", "/inbox/source.pdf", "--json")
	require.NoError(t, err)
	var node api.Node
	require.NoError(t, json.Unmarshal([]byte(statJSON), &node))
	output := filepath.Join(t.TempDir(), "downloaded.pdf")
	getJSON, err := runCLI(t, "get", "/inbox/source.pdf", output, "--json")
	require.NoError(t, err)
	var receipt getReceipt
	require.NoError(t, json.Unmarshal([]byte(getJSON), &receipt))
	require.Equal(t, node.ID, receipt.NodeID)
	require.Equal(t, node.CurrentVersionID, receipt.VersionID)
	require.Equal(t, node.BlobHash, receipt.BlobHash)
	require.Equal(t, node.Size, receipt.Size)
	require.Equal(t, output, receipt.Output)
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, original, data)
	require.EqualValues(t, len(data), receipt.Size)
	require.Equal(t, redactionPDFHash(data), receipt.BlobHash)
	stop()
	return data, document.PageSource{
		VersionID: receipt.VersionID, SHA256: receipt.BlobHash, Size: receipt.Size,
	}
}

func redactionPDFHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
