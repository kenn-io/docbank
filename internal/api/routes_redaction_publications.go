package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
)

const redactionPublicationsPath = "/api/v1/redaction/publications"

func registerRedactionPublicationRoute(api huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         BatesExport
	}
	huma.Register(api, huma.Operation{OperationID: "publishNumberedPDF", Method: http.MethodPost,
		Path: redactionPublicationsPath, Summary: "Publish one reviewed numbered PDF into retained storage",
		MaxBodyBytes: processing.MaxRedactionRequestBytes, BodyReadTimeout: requestTimeout,
		Errors: []int{400, 401, 403, 404, 408, 409, 413, 422, 500, 503, 504}},
		func(ctx context.Context, input *struct {
			Body    processing.RedactionRenderRequest
			RawBody []byte
		}) (*output, error) {
			request, err := processing.DecodeRedactionRenderRequest(input.RawBody)
			if err != nil {
				return nil, (*redactionWireError)(NewError(400, "invalid_redaction_request", "a valid numbered allocation and review are required"))
			}
			if err = clearRequestReadDeadline(ctx); err == nil {
				var artifactResult BatesExport
				artifact, e := d.Processing.PublishNumberedPDF(ctx, request, d.PageRuntime)
				err = e
				if err == nil {
					artifactResult, err = batesExportDetail(ctx, d.Store, artifact)
				}
				if err == nil {
					return &output{CacheControl: "private, no-store", Body: artifactResult}, nil
				}
			}
			problem := redactionProblem(ctx, err)
			logRedactionFailure(ctx, d, problem, err)
			return nil, (*redactionWireError)(problem)
		})
}

// This route uses the usual finite body-read deadline, but its work may take
// ten minutes. Keep the controller installed even though it is timeout-exempt.
func redactionPublicationContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	ctx := context.WithValue(r.Context(), responseControllerContextKey{}, http.NewResponseController(w))
	return context.WithTimeout(ctx, 10*time.Minute)
}
