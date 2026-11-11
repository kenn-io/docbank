package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

const recipientPackagesPath = "/api/v1/redaction/recipient-packages"
const recipientPackageMediaType = "application/zip"

type recipientPackageDownload struct {
	processing.PreparedRecipientPackage

	Contract     string `json:"contract"`
	URL          string `json:"url"`
	Name         string `json:"name"`
	Verification string `json:"verification"`
}

func (s *Server) registerRecipientPackages(mux *http.ServeMux) {
	registerRecipientPackageOpenAPI(s.api)
	mux.HandleFunc("POST "+recipientPackagesPath, s.serveRecipientPackage)
	mux.HandleFunc("DELETE "+recipientPackagesPath, s.serveRecipientPackage)
}

func (s *Server) serveRecipientPackage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if r.URL.RawQuery != "" {
		writeError(w, NewError(400, "invalid_recipient_package", "query parameters are not accepted"))
		return
	}
	owner, ok := workspaceSnapshotOwner(r.Context())
	if !ok {
		writeError(w, NewError(401, "unauthorized", "authentication required"))
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.deps.Processing.DiscardRecipientPackages(owner); err != nil {
			writeError(w, recipientPackageProblem(r.Context(), err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, done, err := redactionPreviewDeadline(w, r)
	if err != nil {
		writeError(w, recipientPackageProblem(r.Context(), err))
		return
	}
	defer done()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, processing.MaxRedactionRequestBytes))
	if err != nil {
		// Close before clearing the failed read deadline; net/http must not drain a stalled body.
		err = errors.Join(err, r.Body.Close())
		problem := NewError(400, "invalid_recipient_package", "package request could not be read")
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			problem = NewError(413, "invalid_recipient_package", "package request exceeds its limit")
		}
		if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
			problem = NewError(408, "redaction_canceled", "package upload timed out")
		}
		if ctx.Err() != nil {
			problem = redactionProblem(ctx, err)
		}
		writeError(w, problem)
		return
	}
	request, err := processing.DecodeRedactionRenderRequest(data)
	if err != nil {
		writeError(w, NewError(400, "invalid_recipient_package", "a numbered allocation and its review are required"))
		return
	}
	if err := http.NewResponseController(w).SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, recipientPackageProblem(ctx, err))
		return
	}
	publish := func(commit func() error) error {
		if !browserSessionRequest(ctx) {
			return commit()
		}
		active, err := s.webSessions.withActiveOwner(owner, commit)
		if !active && err == nil {
			return processing.ErrRecipientPackageUnavailable
		}
		return err
	}
	result, err := s.deps.Processing.BuildRecipientPackage(ctx, owner, request, publish)
	if err != nil {
		problem := recipientPackageProblem(ctx, err)
		logRedactionFailure(ctx, s.deps, problem, err)
		writeError(w, problem)
		return
	}
	var token string
	err = publish(func() error {
		var issueErr error
		token, issueErr = s.webDownloads.issue(webDownloadTicket{owner: owner,
			recipientPackageID: result.ID, blobHash: result.SHA256, size: result.Size,
			name: "recipient.zip", mediaType: recipientPackageMediaType})
		return issueErr
	})
	response := recipientPackageDownload{PreparedRecipientPackage: result,
		Contract: "native-pdf-recipient-download/v1", URL: webDownloadFilePath + "?ticket=" + token,
		Name: "recipient.zip", Verification: "masks_text_and_numbers"}
	encoded, encodeErr := json.Marshal(response)
	if err == nil {
		err = encodeErr
	}
	if err == nil && len(encoded) > 256<<10 {
		err = processing.ErrRecipientPackageLimit
	}
	if err != nil {
		s.webDownloads.cancel(owner, token)
		cleanup := s.deps.Processing.DiscardRecipientPackage(owner, result.ID)
		writeError(w, recipientPackageProblem(ctx, errors.Join(err, cleanup)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if _, err := w.Write(encoded); err != nil {
		s.webDownloads.cancel(owner, token)
		if err := s.deps.Processing.DiscardRecipientPackage(owner, result.ID); err != nil {
			slog.WarnContext(ctx, "removing undelivered recipient package", "error", err)
		}
	}
}

func deliverRecipientPackage(w http.ResponseWriter, r *http.Request, s *processing.Service, ticket webDownloadTicket) {
	ctx, done, err := redactionPreviewDeadline(w, r)
	if err != nil {
		writeError(w, recipientPackageProblem(r.Context(), err))
		return
	}
	defer done()
	reader, err := s.AcquireRecipientPackage(ctx, ticket.owner, ticket.recipientPackageID)
	if err != nil {
		writeError(w, recipientPackageProblem(ctx, err))
		return
	}
	defer func() {
		if err := reader.Close(); err != nil {
			slog.WarnContext(ctx, "recipient package delivery cleanup", "error", err)
		}
	}()
	if reader.Package.Size != ticket.size || reader.Package.SHA256 != ticket.blobHash {
		writeError(w, recipientPackageProblem(ctx, processing.ErrRecipientPackageIntegrity))
		return
	}
	controller := http.NewResponseController(w)
	interrupted := make(chan struct{})
	stop := context.AfterFunc(reader.Context(), func() {
		_ = controller.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	w.Header().Set("Content-Type", recipientPackageMediaType)
	w.Header().Set("Content-Disposition", `attachment; filename="recipient.zip"`)
	w.Header().Set("Content-Length", strconv.FormatInt(reader.Package.Size, 10))
	w.Header().Set("X-Content-Sha256", reader.Package.SHA256)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if n, err := io.Copy(w, reader); err != nil || n != reader.Package.Size {
		panic(http.ErrAbortHandler)
	}
}

func recipientPackageProblem(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		return redactionProblem(ctx, err)
	}
	switch {
	case errors.Is(err, processing.ErrRecipientPackageBusy):
		return NewError(503, "recipient_package_busy", err.Error())
	case errors.Is(err, processing.ErrRecipientPackageUnavailable):
		return NewError(410, "recipient_package_unavailable", "recipient package is unavailable")
	case errors.Is(err, processing.ErrRecipientPackageNotPublished):
		return NewError(409, "recipient_package_not_published", "allocation has no published numbered PDF")
	case errors.Is(err, processing.ErrRecipientPackageLimit):
		return NewError(422, "recipient_package_limit", "recipient package exceeds its size or import limits")
	case errors.Is(err, processing.ErrRecipientPackageInvalid):
		return NewError(400, "invalid_recipient_package", err.Error())
	case errors.Is(err, processing.ErrRecipientPackageIntegrity), errors.Is(err, pdfproduction.ErrPDFIntegrity):
		return NewError(500, "redaction_integrity", "retained PDF or recipient package failed verification")
	case errors.Is(err, processing.ErrRedactionPreparationUnavailable):
		return NewError(503, "redaction_unavailable", "native PDF operation unavailable; check the daemon")
	default:
		return redactionProblem(ctx, err)
	}
}

func registerRecipientPackageOpenAPI(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas
	result := schemas.Schema(reflect.TypeFor[recipientPackageDownload](), true, "")
	problem := schemas.Schema(reflect.TypeFor[Error](), true, "")
	build := &huma.Operation{OperationID: "buildRecipientPackage", Method: "POST", Path: recipientPackagesPath,
		Summary:      "Prepare a recipient ZIP from one completed numbered review",
		MaxBodyBytes: processing.MaxRedactionRequestBytes, BodyReadTimeout: requestTimeout,
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			jsonMediaType: {Schema: schemas.Schema(reflect.TypeFor[processing.RedactionRenderRequest](), true, "")}}},
		Responses: map[string]*huma.Response{
			"201":           {Description: "Verified recipient ZIP and one-use download ticket", Content: map[string]*huma.MediaType{jsonMediaType: {Schema: result}}},
			defaultResponse: {Description: "Package preparation failed", Content: map[string]*huma.MediaType{redactionProblemMediaType: {Schema: problem}}}}}
	api.OpenAPI().AddOperation(build)
	api.OpenAPI().AddOperation(&huma.Operation{OperationID: "discardRecipientPackages", Method: "DELETE", Path: recipientPackagesPath,
		Summary: "Cancel this owner's recipient package and transfer", Responses: map[string]*huma.Response{
			"204": {Description: "Package retired"}, defaultResponse: {Description: "Package cleanup failed", Content: map[string]*huma.MediaType{redactionProblemMediaType: {Schema: problem}}}}})
}
