package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestEmailPDFUnavailableDoesNotQueueOrFabricateOutput(t *testing.T) {
	t.Parallel()
	const reason = "email PDF unavailable: enable memory and pids controller delegation for the systemd user manager"
	ts, _ := newTestServer(t, func(d *api.Deps) { d.EmailPDFUnavailableReason = reason })
	response, body := do(t, ts, http.MethodPost, "/api/v1/email-pdfs", nil, map[string]any{"version_id": "00000000-0000-4000-8000-000000000001", "generation_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "paper": "A4"})
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	require.Contains(t, body, "email_pdf_unavailable")
	require.Contains(t, body, reason)
}

type emailPDFTestRenderer []byte

func (pdf emailPDFTestRenderer) Render(context.Context, emailpdf.HTML) ([]byte, int64, error) {
	return pdf, 1, nil
}

func TestEmailPDFMetadataDoesNotReadPDFBytes(t *testing.T) {
	t.Parallel()
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "", 12)
	pdf.Text(20, 20, "Synthetic API PDF")
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	var runtime *processing.EmailPDFRuntime
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) {
		runtime = &processing.EmailPDFRuntime{
			Catalog: d.Store, Blobs: d.Blobs, Renderer: emailPDFTestRenderer(output.Bytes()), Spool: t.TempDir(),
			Recipe: document.EmailPDFRecipeV1{
				Contract: document.EmailPDFContract, RendererVersion: "synthetic",
				RendererSHA256: strings.Repeat("a", 64), WorkerSHA256: strings.Repeat("b", 64),
				BubblewrapSHA256: strings.Repeat("d", 64),
				FontsSHA256:      strings.Repeat("c", 64), Paper: "A4",
			},
		}
		d.Gate = gate
		d.RequestEmailPDF = runtime.Submit
	})
	hash, size, err := s.Blobs.Write(strings.NewReader("Subject: Synthetic\r\nContent-Type: text/plain\r\n\r\nbody"))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.eml", hash, size, "message/rfc822")
	require.NoError(t, err)
	request := document.EmailPDFRequest{VersionID: node.CurrentVersionID, Paper: "A4"}
	response, body := do(t, ts, http.MethodPost, "/api/v1/email-pdfs", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var job document.EmailPDFJob
	require.NoError(t, json.Unmarshal([]byte(body), &job))
	worker, err := processing.NewRenditionWorker(processing.RenditionWorkerConfig{
		Catalog: s.Store, Blobs: s.Blobs, Runtime: runtime, Gate: gate,
		Owner: "email-pdf-api-test", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	_, err = worker.RunJob(t.Context(), job.JobID)
	require.NoError(t, err)
	receipt, err := s.EmailPDFReceipt(t.Context(), node.CurrentVersionID, job.ProfileFingerprint)
	require.NoError(t, err)
	listPath := "/api/v1/email-pdfs/" + node.CurrentVersionID
	receiptPath := listPath + "/" + job.ProfileFingerprint
	response, body = get(t, ts, receiptPath+"/content", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, output.String(), body)

	for _, state := range []string{"corrupt", "missing"} {
		t.Run(state, func(t *testing.T) {
			status := http.StatusInternalServerError
			if state == "corrupt" {
				path := filepath.Join(s.BlobsDir, receipt.Output.PDFSHA256[:2], receipt.Output.PDFSHA256)
				corrupt := bytes.Clone(output.Bytes())
				corrupt[len(corrupt)/2] ^= 1
				require.NoError(t, os.WriteFile(path, corrupt, 0o600))
			} else {
				require.NoError(t, s.Blobs.Remove(receipt.Output.PDFSHA256))
				status = http.StatusServiceUnavailable
			}
			response, body := get(t, ts, receiptPath+"/content", nil)
			require.Equal(t, status, response.StatusCode, body)
			response, body = do(t, ts, http.MethodPost, "/api/daemon/web-download", nil, map[string]any{
				"node_id": node.ID, "revision": node.Revision, "version_id": node.CurrentVersionID,
				"blob_hash": receipt.Output.PDFSHA256, "size": receipt.Output.PDFSize,
				"email_pdf_profile": receipt.ProfileFingerprint, "email_pdf_attachment": receipt.AttachmentID,
			})
			require.Equal(t, status, response.StatusCode, body)

			t.Run("list", func(t *testing.T) {
				response, body := get(t, ts, listPath, nil)
				require.Equal(t, http.StatusOK, response.StatusCode, body)
				var receipts []document.EmailPDFReceiptV1
				require.NoError(t, json.Unmarshal([]byte(body), &receipts))
				require.Equal(t, []document.EmailPDFReceiptV1{receipt}, receipts)
			})
			t.Run("receipt", func(t *testing.T) {
				response, body := get(t, ts, receiptPath, nil)
				require.Equal(t, http.StatusOK, response.StatusCode, body)
				var got document.EmailPDFReceiptV1
				require.NoError(t, json.Unmarshal([]byte(body), &got))
				require.Equal(t, receipt, got)
			})
			t.Run("reused_job", func(t *testing.T) {
				response, body := do(t, ts, http.MethodPost, "/api/v1/email-pdfs", nil, request)
				require.Equal(t, http.StatusOK, response.StatusCode, body)
				var reused document.EmailPDFJob
				require.NoError(t, json.Unmarshal([]byte(body), &reused))
				require.Equal(t, job.JobID, reused.JobID)
				require.Equal(t, string(store.RenditionJobCompleted), reused.State)
				require.Equal(t, &receipt, reused.Receipt)
			})
		})
	}
}
