package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func fontFixture(t *testing.T, version string) (redaction.Resolved, PageArtifact, redaction.Recipe, []byte) {
	t.Helper()
	plan, artifact, recipe := sanitizedFixture(t, "café 日本")
	recipe.WriterVersion = version
	plan.RecipeSHA256 = mustDigest(t, recipe)
	sealPlan(t, &plan)
	artifact.ResolvedSHA256 = plan.SHA256
	var out bytes.Buffer
	require.NoError(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{artifact}}, plan,
		PDFWriteOptions{Recipe: recipe, StagingDirectory: t.TempDir(), MaxBytes: recipe.MaxStagingBytes}))
	return plan, artifact, recipe, out.Bytes()
}

func TestFreshFontVersions(t *testing.T) {
	for _, version := range []string{"docbank-fresh-objects/v2", "docbank-fresh-objects/v3"} {
		t.Run(version, func(t *testing.T) {
			plan, artifact, recipe, pdf := fontFixture(t, version)
			start := bytes.Index(pdf, []byte("3 0 obj\n"))
			end := bytes.Index(pdf, []byte("4 0 obj\n"))
			require.Positive(t, start)
			require.Greater(t, end, start)
			if version == "docbank-fresh-objects/v2" {
				// Pin the historical object, independently of the writer's dictionary helper.
				expected := []byte("3 0 obj\n<< /Length1 9589900 /Length 9589900 >>\nstream\n")
				expected = append(expected, fontBytes...)
				expected = append(expected, []byte("\nendstream\nendobj\n")...)
				require.Equal(t, expected, pdf[start:end])
			} else {
				require.Less(t, len(pdf), 6_500_000)
				require.Contains(t, string(pdf[start:start+100]), "/Filter /FlateDecode")
			}
			require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(pdf), int64(len(pdf)), verificationFor(t, []PageArtifact{artifact}), recipe))
			require.NoError(t, VerifySaved(t.Context(), bytes.NewReader(pdf), int64(len(pdf)), plan, recipe, nil))
		})
	}
}

func TestFreshFontEncodings(t *testing.T) {
	plan, artifact, recipe, pdf := fontFixture(t, "docbank-fresh-objects/v3")
	for _, level := range []int{zlib.NoCompression, zlib.BestSpeed, zlib.HuffmanOnly, zlib.BestCompression} {
		t.Run(strconv.Itoa(level), func(t *testing.T) {
			compressed := savedStream(t, fontBytes, level)
			changed := rewriteFont(t, pdf, compressed, 9589900, true)
			require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor(t, []PageArtifact{artifact}), recipe))
			require.NoError(t, VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, recipe, nil))
		})
	}
}

func rewriteFont(t *testing.T, pdf, data []byte, length1 int, compressed bool) []byte {
	t.Helper()
	return rewriteFreshFixture(t, pdf, 1, func(objects map[int][]byte) {
		filter := ""
		if compressed {
			filter = " /Filter /FlateDecode"
		}
		head := fmt.Appendf(nil, "3 0 obj\n<< /Length1 %d%s /Length %d >>\nstream\n", length1, filter, len(data))
		objects[3] = append(append(head, data...), []byte("\nendstream\nendobj\n")...)
	}, nil)
}

func TestFreshFontCancellationDoesNotPoisonWriter(t *testing.T) {
	const child = "DOCBANK_TEST_COLD_FONT"
	if os.Getenv(child) != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestFreshFontCancellationDoesNotPoisonWriter$", "-test.v")
		cmd.Env = append(os.Environ(), child+"=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	// This fresh process has never initialized the shared font value. A refused
	// first caller must not cache its context error for the next request.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := storedFont(ctx, "docbank-fresh-objects/v3")
	require.ErrorIs(t, err, context.Canceled)
	plan, artifact, recipe, pdf := fontFixture(t, "docbank-fresh-objects/v3")
	require.NoError(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(pdf), int64(len(pdf)), verificationFor(t, []PageArtifact{artifact}), recipe))
	require.NoError(t, VerifySaved(t.Context(), bytes.NewReader(pdf), int64(len(pdf)), plan, recipe, nil))
}

func TestFreshFontRefusals(t *testing.T) {
	plan, artifact, recipe, pdf := fontFixture(t, "docbank-fresh-objects/v3")
	valid := savedStream(t, fontBytes, zlib.BestSpeed)
	// A valid stream with empty stored blocks exercises the compressed-size cap,
	// not a decoder failure or a second stream.
	stored := savedStream(t, fontBytes, zlib.NoCompression)
	blocks := bytes.Repeat([]byte{0, 0, 0, 255, 255}, (10<<20-len(stored))/5+1)
	oversized := append(append(bytes.Clone(stored[:2]), blocks...), stored[2:]...)
	changedFont := bytes.Clone(fontBytes)
	changedFont[100] ^= 1
	badChecksum := bytes.Clone(valid)
	badChecksum[len(badChecksum)-1] ^= 1
	for _, test := range []struct {
		name       string
		data       []byte
		length1    int
		compressed bool
	}{
		{"raw in v3", fontBytes, 9589900, false},
		{"changed font", savedStream(t, changedFont, zlib.BestSpeed), 9589900, true},
		{"short font", savedStream(t, fontBytes[:len(fontBytes)-1], zlib.BestSpeed), 9589900, true},
		{"extra font byte", savedStream(t, append(bytes.Clone(fontBytes), 0), zlib.BestSpeed), 9589900, true},
		{"Length1", valid, 9589899, true},
		{"payload", append(bytes.Clone(valid), 'x'), 9589900, true},
		{"second stream", append(bytes.Clone(valid), savedStream(t, nil, zlib.NoCompression)...), 9589900, true},
		{"empty dictionary", append([]byte{0x78, 0x20, 0, 0, 0, 1}, valid[2:]...), 9589900, true},
		{"truncated trailer", valid[:len(valid)-1], 9589900, true},
		{"bad checksum", badChecksum, 9589900, true},
		{"stored cap", oversized, 9589900, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := rewriteFont(t, pdf, test.data, test.length1, test.compressed)
			require.ErrorIs(t, VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, recipe, nil), ErrPDFIntegrity)
			require.ErrorIs(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor(t, []PageArtifact{artifact}), recipe), ErrPDFIntegrity)
		})
	}
	oldPlan, oldArtifact, oldRecipe, oldPDF := fontFixture(t, "docbank-fresh-objects/v2")
	crossed := rewriteFont(t, oldPDF, valid, 9589900, true)
	require.ErrorIs(t, VerifySaved(t.Context(), bytes.NewReader(crossed), int64(len(crossed)), oldPlan, oldRecipe, nil), ErrPDFIntegrity)
	require.ErrorIs(t, VerifyFreshWithRecipe(t.Context(), bytes.NewReader(crossed), int64(len(crossed)), verificationFor(t, []PageArtifact{oldArtifact}), oldRecipe), ErrPDFIntegrity)
}
