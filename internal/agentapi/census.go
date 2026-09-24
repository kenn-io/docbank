package agentapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/agentops"
)

// ValidateCensus requires one classified route for every active daemon route.
// POST effects still require a handler review: HTTP method alone cannot tell a
// search request from a durable job. PUT, PATCH and DELETE always mutate.
func ValidateCensus(actual []string, declared []agentops.Route) error {
	seen := make(map[string]bool, len(actual))
	for _, pattern := range actual {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || !validMethod(method) || !strings.HasPrefix(path, "/") || seen[pattern] {
			return fmt.Errorf("invalid or duplicate active route %q", pattern)
		}
		seen[pattern] = true
	}
	classified := make(map[string]bool, len(declared))
	for _, route := range declared {
		pattern := route.Method + " " + route.Pattern
		if route.ID == "" || !seen[pattern] || classified[pattern] ||
			!agentops.PolicyAdmin.Allows(route.Class) {
			return fmt.Errorf("invalid, duplicate or inactive classified route %q", pattern)
		}
		if (route.Method == http.MethodPut || route.Method == http.MethodPatch || route.Method == http.MethodDelete) &&
			(route.Class == agentops.Read || route.Class == agentops.Session) {
			return fmt.Errorf("mutating route classified as read: %s", pattern)
		}
		classified[pattern] = true
	}
	missing := make([]string, 0)
	for pattern := range seen {
		if !classified[pattern] {
			missing = append(missing, pattern)
		}
	}
	slices.Sort(missing)
	if len(missing) != 0 {
		return fmt.Errorf("active route lacks classification: %s", missing[0])
	}
	return nil
}
