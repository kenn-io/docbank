package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
)

const redactionPreviewsPath = "/api/v1/redaction/previews"

type redactionPreviewConfirmation struct {
	Confirmed     bool   `json:"confirmed"`
	ReviewSHA256  string `json:"review_sha256"`
	ArchiveSHA256 string `json:"archive_sha256"`
}
type redactionPreviewTicket struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func redactionPreviewID(id string) bool {
	if len(id) != 48 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func redactionBrowserRequestAllowed(r *http.Request) bool {
	if r.URL.RawQuery != "" {
		return false
	}
	if r.URL.Path == redactionPreviewsPath {
		return r.Method == http.MethodPost || r.Method == http.MethodDelete
	}
	if r.URL.Path == "/api/v1/redaction/prepare" || r.URL.Path == "/api/v1/redaction/resolve" {
		return r.Method == http.MethodPost
	}
	rest, ok := strings.CutPrefix(r.URL.Path, redactionPreviewsPath+"/")
	if !ok {
		return false
	}
	id, part, _ := strings.Cut(rest, "/")
	if !redactionPreviewID(id) {
		return false
	}
	switch part {
	case "":
		return r.Method == http.MethodGet || r.Method == http.MethodDelete
	case "text", "bundle":
		return r.Method == http.MethodGet
	case "download":
		return r.Method == http.MethodPost
	}
	if page, ok := strings.CutPrefix(part, "pages/"); ok {
		n, err := strconv.Atoi(page)
		return err == nil && n >= 1 && n <= 16 && strconv.Itoa(n) == page && r.Method == http.MethodGet
	}
	return false
}

func (s *Server) registerRedactionPreviews(mux *http.ServeMux) {
	registerRedactionPreviewOpenAPI(s.api)
	routes := []string{"POST " + redactionPreviewsPath, "DELETE " + redactionPreviewsPath, "GET " + redactionPreviewsPath + "/{id}", "DELETE " + redactionPreviewsPath + "/{id}",
		"GET " + redactionPreviewsPath + "/{id}/pages/{page}", "GET " + redactionPreviewsPath + "/{id}/text", "GET " + redactionPreviewsPath + "/{id}/bundle", "POST " + redactionPreviewsPath + "/{id}/download"}
	for _, route := range routes {
		mux.HandleFunc(route, s.serveRedactionPreview)
	}
}

func (s *Server) serveRedactionPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		writeError(w, NewError(400, "invalid_redaction_request", "query parameters are not accepted"))
		return
	}
	owner, ok := workspaceSnapshotOwner(r.Context())
	if !ok {
		writeError(w, NewError(401, "unauthorized", "authentication required"))
		return
	}
	ctx, done, err := redactionPreviewDeadline(w, r, 10*time.Minute)
	if err != nil {
		writeError(w, redactionProblem(r.Context(), err))
		return
	}
	defer done()
	r = r.WithContext(ctx)
	id := r.PathValue("id")
	if r.URL.Path == redactionPreviewsPath {
		if r.Method == http.MethodDelete {
			if err := s.deps.Processing.DiscardRedactionPreviews(owner); err != nil {
				slog.Error("discarding redaction preview", "error", err)
				writeError(w, redactionProblem(ctx, err))
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.createRedactionPreviewHTTP(w, r, owner)
		return
	}
	if !redactionPreviewID(id) {
		writeError(w, redactionProblem(ctx, processing.ErrRedactionPreviewUnavailable))
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.deps.Processing.DiscardRedactionPreview(owner, id); err != nil {
			writeError(w, redactionProblem(ctx, err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost {
		s.redactionPreviewDownload(w, r, owner, id)
		return
	}
	if r.URL.Path == redactionPreviewsPath+"/"+id {
		result, err := s.deps.Processing.RedactionPreview(owner, id)
		if err != nil {
			writeError(w, redactionProblem(ctx, err))
			return
		}
		writeRedactionPreviewJSON(w, 200, result)
		return
	}
	part, page := "text", 0
	switch {
	case strings.HasSuffix(r.URL.Path, "/bundle"):
		if browserSessionRequest(ctx) {
			writeError(w, NewError(403, "download_ticket_required", "confirm the preview to download its ZIP"))
			return
		}
		part = "bundle"
	case r.PathValue("page") != "":
		part = "page"
		page, err = strconv.Atoi(r.PathValue("page"))
		if err != nil || page < 1 || page > 16 || strconv.Itoa(page) != r.PathValue("page") {
			writeError(w, redactionProblem(ctx, processing.ErrRedactionPreviewUnavailable))
			return
		}
	}
	deliverRedactionPreview(w, r, s.deps.Processing, owner, id, part, page, "", "")
}

func (s *Server) createRedactionPreviewHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, processing.MaxRedactionRequestBytes))
	if err != nil {
		writeError(w, NewError(413, "invalid_redaction_request", "redaction request exceeds its limit"))
		return
	}
	// Presence is forbidden, even null: previews never reserve or publish numbers.
	var raw struct {
		Preparation  jsontext.Value `json:"preparation"`
		Masks        jsontext.Value `json:"masks"`
		ReviewSHA256 string         `json:"review_sha256"`
	}
	if err = json.Unmarshal(data, &raw, json.RejectUnknownMembers(true)); err != nil {
		writeError(w, NewError(400, "invalid_redaction_request", "preview requires preparation, masks and review_sha256 only"))
		return
	}
	request, err := processing.DecodeRedactionRenderRequest(data)
	if err != nil {
		writeError(w, NewError(400, "invalid_redaction_request", "redaction request is invalid"))
		return
	}
	if err = http.NewResponseController(w).SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, redactionProblem(r.Context(), err))
		return
	}
	var publish func(func() error) error
	if browserSessionRequest(r.Context()) {
		publish = func(commit func() error) error {
			active, err := s.webSessions.withActiveOwner(owner, commit)
			if !active && err == nil {
				return processing.ErrRedactionPreviewUnavailable
			}
			return err
		}
	}
	result, err := s.deps.Processing.CreateRedactionPreview(r.Context(), owner, request, s.deps.PageRuntime, publish)
	if err != nil {
		problem := redactionProblem(r.Context(), err)
		logRedactionFailure(r.Context(), s.deps, problem, err)
		writeError(w, problem)
		return
	}
	writeRedactionPreviewJSON(w, http.StatusCreated, result)
}

func (s *Server) redactionPreviewDownload(w http.ResponseWriter, r *http.Request, owner, id string) {
	if !browserSessionRequest(r.Context()) {
		writeError(w, NewError(403, "browser_session_required", "use a browser session for a download ticket"))
		return
	}
	var confirmation redactionPreviewConfirmation
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, 1024), &confirmation, json.RejectUnknownMembers(true)); err != nil || !confirmation.Confirmed {
		writeError(w, NewError(400, "invalid_redaction_request", "confirm the reviewed preview before downloading"))
		return
	}
	preview, err := s.deps.Processing.RedactionPreview(owner, id)
	if err != nil {
		writeError(w, redactionProblem(r.Context(), err))
		return
	}
	if confirmation.ReviewSHA256 != preview.Archive.Receipt.ReviewSHA256 || confirmation.ArchiveSHA256 != preview.Archive.SHA256 {
		writeError(w, redactionProblem(r.Context(), processing.ErrRedactionReviewChanged))
		return
	}
	var token string
	active, err := s.webSessions.withActiveOwner(owner, func() error {
		var err error
		token, err = s.webDownloads.issue(webDownloadTicket{owner: owner, previewID: id, previewReview: confirmation.ReviewSHA256,
			blobHash: preview.Archive.SHA256, size: preview.Archive.Size, name: "redacted.zip", mediaType: "application/zip"})
		return err
	})
	if !active || err != nil {
		writeError(w, redactionProblem(r.Context(), processing.ErrRedactionPreviewUnavailable))
		return
	}
	writeRedactionPreviewJSON(w, 200, redactionPreviewTicket{URL: webDownloadFilePath + "?ticket=" + token, Size: preview.Archive.Size, SHA256: preview.Archive.SHA256})
}

func writeRedactionPreviewJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 256<<10 {
		writeError(w, NewError(500, "redaction_failed", "preview response exceeds its limit"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func redactionPreviewDeadline(w http.ResponseWriter, r *http.Request, timeout time.Duration) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	var bodyDeadline time.Time
	if r.Method == http.MethodPost {
		bodyDeadline = time.Now().Add(requestTimeout)
	}
	for _, step := range []struct {
		set      func(time.Time) error
		deadline time.Time
	}{{controller.SetReadDeadline, bodyDeadline}, {controller.SetWriteDeadline, deadline}} {
		if err := step.set(step.deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			cancel()
			return nil, nil, err
		}
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	return ctx, func() {
		if !stop() {
			<-interrupted
		}
		cancel()
		_ = controller.SetReadDeadline(time.Time{})
		_ = controller.SetWriteDeadline(time.Time{})
	}, nil
}

func deliverRedactionPreview(w http.ResponseWriter, r *http.Request, service *processing.Service, owner, id, part string, page int, reviewSHA, archiveSHA string) {
	ctx, done, err := redactionPreviewDeadline(w, r, 10*time.Minute)
	if err != nil {
		writeError(w, redactionProblem(r.Context(), err))
		return
	}
	defer done()
	reader, err := service.AcquireRedactionPreview(ctx, owner, id, part, page)
	if err != nil {
		writeError(w, redactionProblem(ctx, err))
		return
	}
	defer func() { _ = reader.Close() }()
	if reviewSHA != "" && (reader.Manifest.Archive.Receipt.ReviewSHA256 != reviewSHA || reader.Manifest.Archive.SHA256 != archiveSHA) {
		writeError(w, redactionProblem(ctx, processing.ErrRedactionReviewChanged))
		return
	}
	controller := http.NewResponseController(w)
	interrupted := make(chan struct{})
	stop := context.AfterFunc(reader.Context(), func() { _ = controller.SetWriteDeadline(time.Now()); close(interrupted) })
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	mediaType := "text/plain; charset=utf-8"
	if part == "page" {
		mediaType = "image/png"
	}
	if part == "bundle" {
		mediaType = "application/zip"
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "redacted.zip"}))
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(reader.Digest.Size, 10))
	w.Header().Set("X-Content-Sha256", reader.Digest.SHA256)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, reader); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func registerRedactionPreviewOpenAPI(api huma.API) {
	const defaultResponse = "default"
	schemas := api.OpenAPI().Components.Schemas
	manifest := schemas.Schema(reflect.TypeFor[processing.RedactionPreview](), true, "")
	problem := schemas.Schema(reflect.TypeFor[Error](), true, "")
	type previewInput struct {
		Preparation  processing.RedactionPreparation `json:"preparation"`
		Masks        []processing.RedactionMask      `json:"masks"`
		ReviewSHA256 string                          `json:"review_sha256"`
	}
	for _, route := range []struct {
		method, path, id, summary string
		input                     reflect.Type
		output                    *huma.Schema
		status                    string
	}{
		{"POST", redactionPreviewsPath, "createRedactionPreview", "Render one temporary verified PDF preview", reflect.TypeFor[previewInput](), manifest, "201"},
		{"DELETE", redactionPreviewsPath, "discardRedactionPreviews", "Cancel this owner's redaction preview and transfers", nil, nil, "204"},
		{"GET", redactionPreviewsPath + "/{id}", "getRedactionPreview", "Read a temporary redaction preview", nil, manifest, "200"},
		{"DELETE", redactionPreviewsPath + "/{id}", "discardRedactionPreview", "Discard a temporary redaction preview", nil, nil, "204"},
		{"POST", redactionPreviewsPath + "/{id}/download", "downloadRedactionPreview", "Confirm and request a one-use ZIP download", reflect.TypeFor[redactionPreviewConfirmation](), schemas.Schema(reflect.TypeFor[redactionPreviewTicket](), true, ""), "200"},
	} {
		op := &huma.Operation{OperationID: route.id, Method: route.method, Path: route.path, Summary: route.summary, Responses: map[string]*huma.Response{defaultResponse: {Description: "Preview request failed", Content: map[string]*huma.MediaType{redactionProblemMediaType: {Schema: problem}}}, route.status: {Description: route.summary}}}
		if strings.Contains(route.path, "{id}") {
			op.Parameters = []*huma.Param{{Name: "id", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{48}$"}}}
		}
		if route.input != nil {
			op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{jsonMediaType: {Schema: schemas.Schema(route.input, true, "")}}}
		}
		if route.output != nil {
			op.Responses[route.status].Content = map[string]*huma.MediaType{jsonMediaType: {Schema: route.output}}
		}
		api.OpenAPI().AddOperation(op)
	}
	for _, part := range []string{"pages/{page}", "text", "bundle"} {
		params := []*huma.Param{{Name: "id", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: openAPIStringType, Pattern: "^[0-9a-f]{48}$"}}}
		media, id := "application/zip", "downloadRedactionPreviewBundle"
		if part == "text" {
			media, id = "text/plain", "readRedactionPreviewText"
		}
		if part == "pages/{page}" {
			media, id = "image/png", "readRedactionPreviewPage"
			params = append(params, &huma.Param{Name: "page", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: mailboxIntegerType, Minimum: new(float64(1)), Maximum: new(float64(16))}})
		}
		api.OpenAPI().AddOperation(&huma.Operation{OperationID: id, Method: "GET", Path: redactionPreviewsPath + "/{id}/" + part, Summary: "Read verified temporary redaction output", Parameters: params, Responses: map[string]*huma.Response{
			"200": {Description: "Verified output bytes", Content: map[string]*huma.MediaType{media: {Schema: &huma.Schema{Type: openAPIStringType, Format: "binary"}}}}, defaultResponse: {Description: "Preview request failed", Content: map[string]*huma.MediaType{redactionProblemMediaType: {Schema: problem}}}}})
	}
}
