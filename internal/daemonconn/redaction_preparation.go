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

// PrepareNativePDF makes one request and checks every returned identity before use.
func (c *Connection) PrepareNativePDF(ctx context.Context,
	request processing.RedactionPreparationRequest,
) (processing.RedactionPreparation, error) {
	if err := processing.ValidateRedactionPreparationRequest(request); err != nil {
		return processing.RedactionPreparation{}, err
	}
	var response *http.Response
	_, err := c.apiWithResponse(&response).PrepareNativePDF(runtime.WithStreamingResponse(ctx),
		&apiclient.PrepareNativePDFRequestOptions{Body: &request})
	if err != nil {
		return processing.RedactionPreparation{}, err
	}
	defer func() { _ = response.Body.Close() }()
	const wireLimit = 16 << 20
	encoded, err := io.ReadAll(io.LimitReader(response.Body, wireLimit+1))
	if err != nil {
		return processing.RedactionPreparation{}, fmt.Errorf("reading native PDF preparation: %w", err)
	}
	if len(encoded) > wireLimit {
		return processing.RedactionPreparation{}, fmt.Errorf("%w: native PDF preparation response too large", ErrIntegrity)
	}
	var transport struct {
		processing.RedactionPreparation

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(encoded, &transport, json.RejectUnknownMembers(true)); err != nil {
		return processing.RedactionPreparation{}, fmt.Errorf("%w: decoding native PDF preparation: %w", ErrIntegrity, err)
	}
	result := transport.RedactionPreparation
	if result.Selection != request.Selection {
		return processing.RedactionPreparation{}, fmt.Errorf("%w: native PDF selection differs", ErrIntegrity)
	}
	if err := processing.ValidateRedactionPreparation(result); err != nil {
		return processing.RedactionPreparation{}, fmt.Errorf("%w: %w", ErrIntegrity, err)
	}
	return result, nil
}
