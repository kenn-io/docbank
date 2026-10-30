package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/processing"
)

func TestRecipientPackageLabelBoundary(t *testing.T) {
	pdf := reportPDF(t, "Recipient label boundary")
	for _, length := range []int{256, 257} {
		t.Run(strconv.Itoa(length), func(t *testing.T) {
			label := strings.Repeat("A", 128) + "000001" + strings.Repeat("B", length-134)
			document := processing.RecipientPackageDocument{
				Ordinal: 1, DocumentID: label, PageCount: 1, Labels: []string{label},
				AllocationID: "11111111-1111-4111-8111-111111111111", NamespaceID: "22222222-2222-4222-8222-222222222222",
				ArtifactManifestSHA256: strings.Repeat("a", 64), RecipeSHA256: strings.Repeat("b", 64), ReviewSHA256: strings.Repeat("c", 64),
				PDF:  processing.RecipientPackageFile{Path: "VOL001/PDF/000001.pdf", Size: int64(len(pdf)), SHA256: redactionPDFHash(pdf)},
				Text: processing.RecipientPackageFile{Path: "VOL001/TEXT/000001.txt", Size: 25, SHA256: redactionPDFHash([]byte("Recipient label boundary\f"))},
			}
			var archive bytes.Buffer
			requested := false
			_, err := processing.WriteRecipientPackage(t.Context(), &archive, []processing.RecipientPackageDocument{document},
				func(context.Context, int) (io.ReadCloser, []byte, error) {
					requested = true
					return io.NopCloser(bytes.NewReader(pdf)), []byte("Recipient label boundary\f"), nil
				})
			if length == 257 {
				require.ErrorIs(t, err, processing.ErrRecipientPackageInvalid)
				require.Equal(t, exitUsage, commandExitCode(recipientCLIError(err), true))
				require.False(t, requested, "reject labels the recipient cannot import before requesting a PDF")
				require.Zero(t, archive.Len())
				return
			}
			require.NoError(t, err)
			root, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(root)) })
			home := t.TempDir()
			t.Setenv("DOCBANK_HOME", home)
			startServe(t)
			waitForDaemon(t, home)
			out, err := runCLI(t, "package", "preflight", root, "--profile", "dat-concordance-v1", "--page-map-profile", "opt-standard-v1",
				"--encoding", "utf-8", "--map", filepath.Join(root, "mapping.json"), "--json")
			require.NoError(t, err, out)
			var preflight api.PackagePreflight
			require.NoError(t, json.Unmarshal([]byte(out), &preflight))
			require.False(t, preflight.Blocking, out)
			out, err = runCLI(t, "package", "import", preflight.PreflightID, "--name", "label-boundary", "--json")
			require.NoError(t, err, out)
			var job api.PackageImportJob
			require.NoError(t, json.Unmarshal([]byte(out), &job))
			require.Eventually(t, func() bool {
				out, err = runCLI(t, "package", "import", "status", job.OperationID, "--json")
				require.NoError(t, err, out)
				require.NoError(t, json.Unmarshal([]byte(out), &job))
				return job.State == "complete" || job.State == "partial" || job.State == "failed"
			}, time.Minute, 50*time.Millisecond)
			require.Equal(t, "complete", job.State, out)
			for _, set := range []string{"received", document.NamespaceID} {
				out, err = runCLI(t, "labels", "lookup", label, "--package", job.PackageID, "--label-set", set, "--json")
				require.NoError(t, err, out)
				var matches api.PackageLabelCandidatePage
				require.NoError(t, json.Unmarshal([]byte(out), &matches))
				require.NotEmpty(t, matches.Items)
				for _, match := range matches.Items {
					require.Equal(t, label, match.Label)
					require.Equal(t, set, match.LabelSet)
					require.Equal(t, "received", match.Provenance)
				}
			}
		})
	}
}
