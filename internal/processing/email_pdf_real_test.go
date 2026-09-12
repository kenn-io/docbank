package processing

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

// Explicitly retains only synthetic qualification artifacts for independent
// visual inspection and the real restore/browser download harness.
func TestEmailPDFRealBackupFixture(t *testing.T) {
	output := os.Getenv("DOCBANK_EMAILPDF_TEST_EXPORT")
	if output == "" {
		t.Skip("set an unused absolute DOCBANK_EMAILPDF_TEST_EXPORT directory to retain synthetic proof")
	}
	require.True(t, filepath.IsAbs(output))
	require.NoError(t, os.Mkdir(output, 0700), "never overwrite an existing proof directory")
	bundle := os.Getenv("DOCBANK_EMAILPDF_TEST_BUNDLE")
	fonts := os.Getenv("DOCBANK_EMAILPDF_TEST_FONTS")
	workerPath := os.Getenv("DOCBANK_EMAILPDF_TEST_WORKER")
	bundleHash, err := emailpdf.TreeSHA256(bundle)
	require.NoError(t, err)
	fontsHash, err := emailpdf.TreeSHA256(fonts)
	require.NoError(t, err)
	workerHash, err := emailpdf.FileSHA256(workerPath)
	require.NoError(t, err)
	f := newEmailPipelineFixture(t)
	renderer, err := emailpdf.NewRuntime(emailpdf.RuntimeConfig{Worker: workerPath, WorkerSHA256: workerHash, Chromium: filepath.Join(bundle, "chrome"), Bundle: bundle, BundleSHA256: bundleHash, Fonts: fonts, FontsSHA256: fontsHash, Version: "151.0.7922.34", Spool: f.spool})
	require.NoError(t, err)
	icon := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			pixel := color.RGBA{R: 20, G: 110, B: 220, A: 255}
			if x >= 16 {
				pixel = color.RGBA{R: 245, G: 140, B: 30, A: 255}
			}
			icon.SetRGBA(x, y, pixel)
		}
	}
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, icon))
	source := "Subject: Synthetic résumé review\r\nFrom: Sender <sender@example.test>\r\nTo: Recipient <recipient@example.test>\r\nCc: Copy <copy@example.test>\r\nBcc: Archive <archive@example.test>\r\nDate: Mon, 01 Jan 2024 12:34:56 +0530\r\nMessage-ID: <db29-synthetic@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n" +
		"--m\r\nContent-Type: multipart/related; boundary=r\r\n\r\n--r\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<h1>Résumé 世界</h1><p>Synthetic exact-version PDF proof.</p><img src='cid:synthetic-icon'><blockquote hidden>Full quoted original content.</blockquote><img src='https://example.test/blocked'><table>" + strings.Repeat("<tr><td>Synthetic row</td><td>Complete table content</td></tr>", 80) + "</table><p>" + strings.Repeat("LongURLabcdef", 150) + "</p>\r\n" +
		"--r\r\nContent-Type: image/png\r\nContent-ID: <synthetic-icon>\r\nContent-Disposition: inline; filename=synthetic-icon.png\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(pngBytes.Bytes()) + "\r\n--r--\r\n" +
		"--m\r\nContent-Type: text/csv\r\nContent-Disposition: attachment; filename=synthetic-inventory.csv\r\n\r\nitem,count\r\nsynthetic,2\r\n--m--\r\n"
	target := f.add(t, "synthetic.eml", source, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	r := &EmailPDFRuntime{Catalog: f.catalog, Blobs: f.blobs, Renderer: renderer, Spool: f.spool, Recipe: document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: "151.0.7922.34", RendererSHA256: bundleHash, WorkerSHA256: workerHash, FontsSHA256: fontsHash, Paper: "A4"}}
	request, err := r.Request(t.Context(), target.Version.ID, view.Generation.ID, "A4")
	require.NoError(t, err)
	grantWorkerConsent(t, f.catalog, request)
	job, _, err := f.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: r, Gate: api.NewOperationGate(), Owner: "synthetic-pdf-proof", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	_, err = worker.RunOne(t.Context())
	require.NoError(t, err)
	status, err := f.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, status.State, "%+v", status)
	receipt, err := f.catalog.EmailPDFReceipt(t.Context(), target.Version.ID, request.Profile.Fingerprint)
	require.NoError(t, err)
	stream, _, err := f.blobs.OpenStreamContext(t.Context(), receipt.Output.PDFSHA256)
	require.NoError(t, err)
	pdf, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, receipt.Output.PDFSHA256, renditionBytesSHA256(pdf))
	pdfPath := filepath.Join(output, "synthetic.pdf")
	require.NoError(t, os.WriteFile(pdfPath, pdf, 0600))
	text, err := exec.CommandContext(t.Context(), "pdftotext", pdfPath, "-").Output() //nolint:gosec // Bounded synthetic PDF path; separate arguments and no shell.
	require.NoError(t, err)
	for _, want := range []string{"Résumé", "世界", "Full quoted original content", "+0530", "synthetic-inventory.csv", "Complete table content"} {
		require.Contains(t, string(text), want)
	}
	require.NotContains(t, string(text), "missing inline image")
	images, err := exec.CommandContext(t.Context(), "pdfimages", "-list", pdfPath).Output() //nolint:gosec // Independently inspect the same bounded synthetic PDF without a shell.
	require.NoError(t, err)
	foundIcon := false
	for line := range strings.SplitSeq(string(images), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[2] == "image" && fields[3] == "32" && fields[4] == "32" {
			foundIcon = true
		}
	}
	require.True(t, foundIcon, "independent PDF reader must find the verified inline bitmap: %s", images)
	proof, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(output, "receipt.json"), proof, 0600))
	repositoryPath := filepath.Join(output, "backup")
	repository, err := backup.Init(repositoryPath)
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repository, "email-pdf", f.catalog, f.blobs, backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	verified, err := backup.Verify(t.Context(), repository, backupapp.New("email-pdf"), backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
	require.NoError(t, os.WriteFile(filepath.Join(repositoryPath, "DB29-SYNTHETIC"), []byte("docbank-email-pdf-synthetic/v1\n"), 0600))
	t.Logf("retained synthetic proof=%s pdf=%s pages=%d bytes=%d", output, receipt.Output.PDFSHA256, receipt.Output.Pages, receipt.Output.PDFSize)
}
