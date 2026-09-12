package api

import (
	"context"
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/emailpdf"
	"go.kenn.io/docbank/internal/store"
	"io"
	"net/http"
	"reflect"
	"strconv"
)

func registerEmailPDFRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	mux.HandleFunc("GET /api/v1/email-pdfs/{version_id}", func(w http.ResponseWriter, r *http.Request) {
		receipts, err := d.Store.EmailPDFReceipts(r.Context(), r.PathValue("version_id"))
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		for _, receipt := range receipts {
			if _, err := verifiedEmailPDFBytes(r.Context(), d, receipt); err != nil {
				writeEmailStoreError(w, err)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, receipts)
	})
	mux.HandleFunc("POST /api/v1/email-pdfs", func(w http.ResponseWriter, r *http.Request) {
		if d.RequestEmailPDF == nil {
			writeError(w, NewError(http.StatusServiceUnavailable, "email_pdf_unavailable", "Configure [email_pdf] with a pinned Chromium bundle and fonts on a Linux daemon with systemd isolation."))
			return
		}
		var input document.EmailPDFRequest
		if !readEmailDocumentJSON(w, r, &input) {
			return
		}
		if input.Paper != "" && input.Paper != "A4" && input.Paper != "Letter" {
			writeError(w, NewError(http.StatusUnprocessableEntity, "validation", "email PDF paper must be A4 or Letter"))
			return
		}
		var out document.EmailPDFJob
		err := g.mutate(func() error { var err error; out, err = d.RequestEmailPDF(r.Context(), input); return err })
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		if out.Receipt != nil {
			if _, err = verifiedEmailPDFBytes(r.Context(), d, *out.Receipt); err != nil {
				writeEmailStoreError(w, err)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/email-pdfs/{version_id}/{profile}", func(w http.ResponseWriter, r *http.Request) {
		receipt, err := d.Store.EmailPDFReceipt(r.Context(), r.PathValue("version_id"), r.PathValue("profile"))
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		if _, err = verifiedEmailPDFBytes(r.Context(), d, receipt); err != nil {
			writeEmailStoreError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, receipt)
	})
	mux.HandleFunc("GET /api/v1/email-pdf-jobs/{job_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		job, err := d.Store.RenditionJobByID(r.Context(), r.PathValue("job_id"))
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			State string `json:"state"`
		}{string(job.State)})
	})
	mux.HandleFunc("GET /api/v1/email-pdfs/{version_id}/{profile}/content", func(w http.ResponseWriter, r *http.Request) {
		receipt, err := d.Store.EmailPDFReceipt(r.Context(), r.PathValue("version_id"), r.PathValue("profile"))
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		b, err := verifiedEmailPDFBytes(r.Context(), d, receipt)
		if err != nil {
			writeEmailStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="message.pdf"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set(BlobHashHeader, receipt.Output.PDFSHA256)
		w.Header().Set("X-Docbank-Email-Pdf-Attachment", receipt.AttachmentID)
		w.Header().Set("Content-Digest", contentDigest(mustDecodeHash(receipt.Output.PDFSHA256)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b) //nolint:gosec // Independently parsed PDF bytes, served as an attachment with application/pdf and nosniff.
	})
	registry := api.OpenAPI().Components.Schemas
	api.OpenAPI().AddOperation(&huma.Operation{OperationID: "renderEmailPDF", Method: "POST", Path: "/api/v1/email-pdfs", Summary: "Render one exact email version as a retained verified PDF", RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, reflect.TypeFor[document.EmailPDFRequest]())}}}, Responses: map[string]*huma.Response{"200": {Description: "Existing rendition job and exact retained receipt", Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, reflect.TypeFor[document.EmailPDFJob]())}}}}})
	for _, op := range []struct {
		id, path, summary string
		result            reflect.Type
	}{
		{"listEmailPDFs", "/api/v1/email-pdfs/{version_id}", "List verified retained PDFs for one original email version", reflect.TypeFor[[]document.EmailPDFReceiptV1]()},
		{"getEmailPDF", "/api/v1/email-pdfs/{version_id}/{profile}", "Read an exact retained PDF receipt", reflect.TypeFor[document.EmailPDFReceiptV1]()},
		{"getEmailPDFJob", "/api/v1/email-pdf-jobs/{job_id}", "Read retained PDF job state", reflect.TypeFor[struct {
			State string `json:"state"`
		}]()},
	} {
		api.OpenAPI().AddOperation(&huma.Operation{OperationID: op.id, Method: "GET", Path: op.path, Summary: op.summary, Responses: map[string]*huma.Response{"200": {Description: op.summary, Content: map[string]*huma.MediaType{emailDocumentJSONMediaType: {Schema: huma.SchemaFromType(registry, op.result)}}}}})
	}
	api.OpenAPI().AddOperation(&huma.Operation{OperationID: "downloadEmailPDF", Method: "GET", Path: "/api/v1/email-pdfs/{version_id}/{profile}/content", Summary: "Download independently verified retained PDF bytes", Responses: map[string]*huma.Response{"200": {Description: "Exact retained PDF", Content: map[string]*huma.MediaType{"application/pdf": {}}}}})
}

func verifiedEmailPDFBytes(ctx context.Context, d Deps, receipt document.EmailPDFReceiptV1) ([]byte, error) {
	r, size, err := d.Blobs.OpenStreamContext(ctx, receipt.Output.PDFSHA256)
	if err != nil {
		return nil, err
	}
	if size != receipt.Output.PDFSize || size < 1 || size > emailpdf.MaxPDFBytes {
		return nil, errors.Join(store.ErrEmailCorrupt, r.Close())
	}
	b, err := io.ReadAll(io.LimitReader(r, size+1))
	err = errors.Join(err, r.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size || !r.Verified() {
		return nil, store.ErrEmailCorrupt
	}
	pages, err := emailpdf.VerifyPDF(b)
	if err != nil || pages != receipt.Output.Pages {
		return nil, errors.Join(store.ErrEmailCorrupt, err)
	}
	return b, nil
}
