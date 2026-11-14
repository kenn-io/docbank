package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/processing"
)

var ErrNumberedPublicationUnknown = errors.New("publication outcome unknown; query the allocation or resend the identical publication")

const maxNumberedPublicationBytes = 128 << 10

// PublishNumberedPDF makes one attempt against an operator-created allocation.
// A lost reply never authorizes another reservation or a weaker verification.
func (c *Connection) PublishNumberedPDF(ctx context.Context, request processing.RedactionRenderRequest) (api.BatesExport, error) {
	var result api.BatesExport
	if !validUUIDv4(request.AllocationID.String()) {
		return result, processing.ErrRedactionInvalidRequest
	}
	review, err := processing.MatchRedactionReview(request.Preparation, request.Masks,
		request.Preparation.Selection.Revision, request.ReviewSHA256)
	if err != nil {
		return result, err
	}
	var response *http.Response
	_, err = c.redactionAPI(&response).PublishNumberedPDF(
		runtime.WithStreamingResponse(ctx), &apiclient.PublishNumberedPDFRequestOptions{Body: &request},
		func(_ context.Context, r *http.Request) error {
			if r.ContentLength > processing.MaxRedactionRequestBytes {
				return processing.ErrRedactionPreparationLimit
			}
			return nil
		})
	if err != nil {
		if errors.Is(err, processing.ErrRedactionPreparationLimit) {
			return result, err
		}
		if code, known := ProblemCode(err); known {
			switch code {
			case "redaction_unavailable", "redaction_failed", "redaction_integrity", "redaction_timeout", "redaction_canceled":
				// Shutdown or reading the committed artifact can fail after publication.
			default:
				return result, err
			}
		}
		return result, fmt.Errorf("%w: %w", ErrNumberedPublicationUnknown, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxNumberedPublicationBytes+1))
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrNumberedPublicationUnknown, err)
	}
	var wire struct {
		api.BatesExport

		Schema string `json:"$schema,omitzero"`
	}
	if len(data) > maxNumberedPublicationBytes {
		err = integrityErrorf("numbered publication response exceeds its limit")
	} else {
		err = json.Unmarshal(data, &wire, json.RejectUnknownMembers(true))
	}
	if err == nil && (wire.AllocationID != request.AllocationID.String() || wire.ArtifactID != wire.AllocationID) {
		err = integrityErrorf("numbered publication allocation differs")
	}
	if err == nil {
		var numbered *processing.NumberedReservation
		numbered, err = CheckNumberedBatesExport(wire.BatesExport)
		if err == nil {
			if numbered == nil {
				err = integrityErrorf("numbered publication returned an ordinary stamp")
			} else {
				err = numbered.Recipe.MatchReview(review)
			}
		}
	}
	if err != nil {
		return result, fmt.Errorf("%w: %w: %w", ErrNumberedPublicationUnknown, ErrIntegrity, err)
	}
	return wire.BatesExport, nil
}
