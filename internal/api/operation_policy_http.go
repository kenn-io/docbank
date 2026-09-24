package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// A scoped credential can enter only routes with a source-aware operation
// check in the handler. New routes stay closed until that check is added.
func scopedRequestAllowed(method, path string) bool {
	if !strings.HasPrefix(path, "/api/v1/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/")
	if slices.Contains(parts, "") {
		return false
	}
	switch method {
	case http.MethodGet:
		switch {
		case len(parts) == 1 && (parts[0] == "capabilities" || parts[0] == "coverage" || parts[0] == "tags"):
			return true
		case len(parts) == 2 && parts[0] == "versions":
			return true
		case len(parts) == 3 && parts[0] == "versions" && parts[2] == "content":
			return true
		case len(parts) == 3 && parts[0] == "processing" && parts[1] == "jobs":
			return true
		case len(parts) == 2 && parts[0] == "renditions":
			return true
		}
	case http.MethodPost:
		switch {
		case len(parts) == 2 && parts[0] == "documents" && parts[1] == "scoped":
			return true
		case len(parts) == 1 && (parts[0] == "search" || parts[0] == "tags"):
			return true
		case len(parts) == 2 && parts[0] == "search" && parts[1] == "similar":
			return true
		case len(parts) == 2 && parts[0] == "processing" && (parts[1] == "plans" || parts[1] == "jobs"):
			return true
		case len(parts) == 3 && parts[0] == "processing" && parts[1] == "source-fences" && parts[2] == "resolve":
			return true
		case len(parts) == 2 && parts[0] == "renditions" && (parts[1] == "windows" || parts[1] == "select"):
			return true
		}
	case http.MethodPatch:
		return len(parts) == 2 && parts[0] == "tags"
	case http.MethodDelete:
		return len(parts) == 2 && parts[0] == "tags"
	}
	return false
}

func authorizeRequest(ctx context.Context, d Deps, operation Operation, sourceIDs []string, requireAll, protected bool) (OperationAuthorization, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return OperationAuthorization{}, operationHTTPError(ErrOperationDenied, protected)
	}
	decision, err := d.OperationPolicy.Authorize(ctx, OperationAuthorizationRequest{
		Principal: principal, Operation: operation, SourceIDs: sourceIDs,
		RequireAll: requireAll, Protected: protected,
	})
	if err != nil {
		return OperationAuthorization{}, operationHTTPError(err, protected)
	}
	return decision, nil
}

func operationHTTPError(err error, protected bool) error {
	if protected || errors.Is(err, ErrOperationNotFound) {
		return NewError(http.StatusNotFound, "not_found", "not found")
	}
	if errors.Is(err, ErrOperationScopeTooLarge) {
		return NewError(http.StatusUnprocessableEntity, "source_scope_too_large", "source scope exceeds the supported limit")
	}
	return NewError(http.StatusForbidden, "operation_denied", "operation is not permitted")
}
