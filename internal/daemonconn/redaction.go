package daemonconn

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/processing"
)

// ResolveNativePDF makes one request; a failed response never retries the inspection.
func (c *Connection) ResolveNativePDF(ctx context.Context, request processing.RedactionResolveRequest) (processing.RedactionReview, error) {
	if err := processing.ValidateRedactionMasks(request.Masks); err != nil {
		return processing.RedactionReview{}, err
	}
	if err := processing.ValidateRedactionPreparation(request.Preparation); err != nil {
		return processing.RedactionReview{}, err
	}
	var response *http.Response
	_, err := c.apiWithResponseLimit(&response, processing.MaxRedactionResponseBytes).ResolveNativePDF(runtime.WithStreamingResponse(ctx), &apiclient.ResolveNativePDFRequestOptions{Body: &request})
	if err != nil {
		return processing.RedactionReview{}, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, processing.MaxRedactionResponseBytes+1))
	if err != nil {
		return processing.RedactionReview{}, err
	}
	if int64(len(data)) > processing.MaxRedactionResponseBytes {
		return processing.RedactionReview{}, integrityErrorf("redaction review exceeds response limit")
	}
	var transport struct {
		processing.RedactionReview

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(data, &transport, json.RejectUnknownMembers(true)); err != nil {
		return processing.RedactionReview{}, fmt.Errorf("%w: decoding redaction review: %w", ErrIntegrity, err)
	}
	if err := processing.ValidateRedactionReview(request.Preparation, request.Masks, transport.RedactionReview); err != nil {
		return processing.RedactionReview{}, fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	return transport.RedactionReview, nil
}

type redactionReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r redactionReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
