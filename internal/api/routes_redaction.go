package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
)

func registerRedactionRoutes(mux *http.ServeMux, api huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         processing.RedactionReview
	}
	huma.Register(api, huma.Operation{OperationID: "resolveNativePDF", Method: http.MethodPost,
		Path: "/api/v1/redaction/resolve", Summary: "Review explicit masks against a freshly inspected retained PDF",
		MaxBodyBytes: processing.MaxRedactionRequestBytes, BodyReadTimeout: -1,
		Errors: []int{400, 401, 403, 404, 408, 409, 413, 422, 500, 503, 504},
	}, func(ctx context.Context, input *struct {
		Body    processing.RedactionResolveRequest
		RawBody []byte
	}) (*output, error) {
		request, err := processing.DecodeRedactionResolveRequest(input.RawBody)
		if err != nil {
			return nil, (*redactionWireError)(NewError(400, "invalid_redaction_request", "redaction request is invalid"))
		}
		result, err := d.Processing.ResolveNativePDF(ctx, request, d.PageRuntime)
		if err != nil {
			problem := redactionProblem(ctx, err)
			logRedactionFailure(ctx, d, problem, err)
			return nil, (*redactionWireError)(problem)
		}
		return &output{CacheControl: "no-store", Body: result}, nil
	})
	registerNumberedReservationRoute(api, d)
	registerRedactionRenderOpenAPI(api)
	mux.HandleFunc("POST /api/v1/redaction/render", func(w http.ResponseWriter, r *http.Request) {
		serveRedactionRender(w, r, func(ctx context.Context, request processing.RedactionRenderRequest, send processing.RedactionSender) error {
			err := d.Processing.RenderNativePDF(ctx, request, d.PageRuntime, send)
			if err != nil {
				logRedactionFailure(ctx, d, redactionProblem(ctx, err), err)
			}
			return err
		}, 10*time.Minute)
	})
}

func logRedactionFailure(ctx context.Context, d Deps, problem *Error, err error) {
	switch problem.Status {
	case 500:
		d.Logger.ErrorContext(ctx, "native PDF redaction failed", "error_code", problem.Code, "error", err)
	case 503:
		d.Logger.InfoContext(ctx, "native PDF redaction refused", "error_code", problem.Code, "error", err)
	}
}

func registerRedactionRenderOpenAPI(api huma.API) {
	const redactionDefaultResponse = "default"
	registry := api.OpenAPI().Components.Schemas
	body := registry.Schema(reflect.TypeFor[processing.RedactionRenderRequest](), true, "")
	problem := registry.Schema(reflect.TypeFor[Error](), true, "")
	headers := map[string]*huma.Param{}
	for _, name := range []string{"Content-Length", "X-Content-SHA256", "X-Redaction-Review-SHA256"} {
		headers[name] = &huma.Param{Schema: &huma.Schema{Type: openAPIStringType}}
	}
	api.OpenAPI().AddOperation(&huma.Operation{OperationID: "renderNativePDF", Method: http.MethodPost,
		Path: "/api/v1/redaction/render", Summary: "Render and verify a reviewed native PDF into one ZIP",
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{jsonMediaType: {Schema: body}}},
		Responses: map[string]*huma.Response{
			"200": {Description: "Verified redacted PDF, text and receipt", Headers: headers,
				Content: map[string]*huma.MediaType{"application/zip": {Schema: &huma.Schema{Type: openAPIStringType, Format: "binary"}}}},
			redactionDefaultResponse: {Description: "Error", Content: map[string]*huma.MediaType{redactionProblemMediaType: {Schema: problem}}},
		},
	})
}

type redactionRenderOperation func(context.Context, processing.RedactionRenderRequest, processing.RedactionSender) error

func serveRedactionRender(w http.ResponseWriter, r *http.Request, run redactionRenderOperation, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	for _, set := range []func(time.Time) error{controller.SetReadDeadline, controller.SetWriteDeadline} {
		if err := set(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeError(w, redactionProblem(ctx, err))
			return
		}
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		if !stop() {
			<-interrupted
		}
		_ = controller.SetReadDeadline(time.Time{})
		_ = controller.SetWriteDeadline(time.Time{})
	}()
	if r.ContentLength > processing.MaxRedactionRequestBytes {
		writeError(w, NewError(413, "too_large", "redaction request exceeds 17 MiB"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, processing.MaxRedactionRequestBytes+1))
	if err != nil {
		writeError(w, redactionProblem(ctx, err))
		return
	}
	if int64(len(body)) > processing.MaxRedactionRequestBytes {
		writeError(w, NewError(413, "too_large", "redaction request exceeds 17 MiB"))
		return
	}
	request, err := processing.DecodeRedactionRenderRequest(body)
	if err != nil {
		writeError(w, NewError(400, "invalid_redaction_request", "redaction request is invalid"))
		return
	}
	started := false
	err = run(ctx, request, func(ctx context.Context, reader io.Reader, result processing.RedactionArchive) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		interrupted := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = controller.SetWriteDeadline(time.Now())
			close(interrupted)
		})
		defer func() {
			if !stop() {
				<-interrupted
			}
		}()
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", `attachment; filename="redacted.zip"`)
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
		w.Header().Set("X-Content-Sha256", result.SHA256)
		w.Header().Set("X-Redaction-Review-Sha256", result.Receipt.ReviewSHA256)
		started = true
		w.WriteHeader(http.StatusOK)
		n, err := io.Copy(w, reader)
		if err == nil && n != result.Size {
			err = io.ErrUnexpectedEOF
		}
		return err
	})
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		writeError(w, redactionProblem(ctx, err))
	}
}

func registerNumberedReservationRoute(api huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         processing.NumberedReservation
	}
	huma.Register(api, huma.Operation{OperationID: "reserveNumberedPDF", Method: http.MethodPost,
		Path: "/api/v1/redaction/reservations", Summary: "Retain one reviewed original indefinitely and reserve its Bates numbers",
		MaxBodyBytes: processing.MaxRedactionRequestBytes, BodyReadTimeout: 60 * time.Second,
		Errors: []int{400, 401, 403, 404, 408, 409, 413, 422, 500, 503, 504}}, func(ctx context.Context, input *struct {
		Body    processing.NumberedReservationRequest
		RawBody []byte
	}) (*output, error) {
		request, err := processing.DecodeNumberedReservationRequest(input.RawBody)
		if err != nil {
			return nil, (*redactionWireError)(NewError(400, "invalid_redaction_request", "explicit original-retention consent and a valid reservation request are required"))
		}
		result, err := d.Processing.ReserveNumberedPDF(ctx, request, d.PageRuntime)
		if err != nil {
			problem := redactionProblem(ctx, err)
			if problem.Status >= 500 || problem.Status == 408 {
				problem.Detail += "; resend the identical reservation request with original-retention consent"
			}
			logRedactionFailure(ctx, d, problem, err)
			return nil, (*redactionWireError)(problem)
		}
		return &output{CacheControl: "no-store", Body: result}, nil
	})
}
