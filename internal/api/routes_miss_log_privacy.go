package api

import (
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// The transformer runs before Huma's schema-link wrapper and only applies to
// private receipt operations. Keep schema limits while removing rejected values,
// including nested or unknown fields that Huma would otherwise echo wholesale.
func sanitizeMissLogErrorResponse(ctx huma.Context, _ string, value any) (any, error) {
	op := ctx.Operation()
	if op == nil {
		return value, nil
	}
	if !strings.Contains(op.OperationID, "MissLog") && op.OperationID != "completeEmptySearch" {
		return value, nil
	}
	problem, ok := value.(*Error)
	if !ok {
		return value, nil
	}
	sanitized := *problem
	sanitized.Errors = nil
	if sanitized.Status >= http.StatusInternalServerError {
		sanitized.Detail = "miss log operation failed"
	} else {
		sanitized.Detail = "private search receipt request failed"
	}
	return &sanitized, nil
}
