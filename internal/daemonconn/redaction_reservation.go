package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/processing"
)

var ErrNumberedReservationUnknown = errors.New("reservation outcome unknown; resend the identical request with retention consent")

// ReserveNumberedPDF makes one attempt. An uncertain outcome keeps an exact replay
// possible; changing namespace or start would request another permanent retention.
func (c *Connection) ReserveNumberedPDF(ctx context.Context, request processing.NumberedReservationRequest) (processing.NumberedReservation, error) {
	var result processing.NumberedReservation
	if !request.RetainOriginal || request.StartAt < 1 || canonical.ValidateUUIDv4(request.NamespaceID.String()) != nil {
		return result, processing.ErrRedactionInvalidRequest
	}
	review, err := processing.BuildRedactionReview(request.Preparation, request.Masks, request.Preparation.Selection.Revision)
	if err != nil {
		return result, err
	}
	if review.SHA256 != request.ReviewSHA256 {
		return result, processing.ErrRedactionReviewChanged
	}
	var response *http.Response
	_, err = c.apiWithResponseLimit(&response, processing.MaxRedactionResponseBytes).ReserveNumberedPDF(runtime.WithStreamingResponse(ctx), &apiclient.ReserveNumberedPDFRequestOptions{Body: &request})
	if err != nil {
		if code, known := ProblemCode(err); known {
			switch code {
			case "redaction_unavailable", "redaction_failed", "redaction_integrity", "redaction_timeout", "redaction_canceled":
				// Shutdown, a deadline or a post-commit read can fail after retention.
				// Preserve the original error and the exact replay advice.
			default:
				return result, err
			}
		}
		return result, fmt.Errorf("%w: %w", ErrNumberedReservationUnknown, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, processing.MaxNumberedReservationBytes+1))
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrNumberedReservationUnknown, err)
	}
	if len(data) > processing.MaxNumberedReservationBytes {
		return result, ErrNumberedReservationUnknown
	}
	var wire struct {
		processing.NumberedReservation

		Schema string `json:"$schema,omitzero"`
	}
	if err := json.Unmarshal(data, &wire, json.RejectUnknownMembers(true)); err != nil {
		return result, fmt.Errorf("%w: %w", ErrNumberedReservationUnknown, err)
	}
	result = wire.NumberedReservation
	if err := processing.ValidateNumberedReservation(result, &review); err != nil {
		return processing.NumberedReservation{}, fmt.Errorf("%w: %w", ErrNumberedReservationUnknown, err)
	}
	if result.Recipe.Namespace.ID != request.NamespaceID || result.Recipe.Namespace.Start != request.StartAt {
		return processing.NumberedReservation{}, ErrNumberedReservationUnknown
	}
	return result, nil
}
