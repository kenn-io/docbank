package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestProductionResolverProblemsKeepTypedHTTPStatus(t *testing.T) {
	for _, scenario := range []struct {
		code   string
		status int
	}{
		{"source_stale", http.StatusConflict},
		{"decision_conflict", http.StatusConflict},
		{"selection_expansion_required", http.StatusConflict},
		{"mapping_incomplete", http.StatusUnprocessableEntity},
		{"invalid_mode", http.StatusUnprocessableEntity},
		{"render_limit", http.StatusRequestEntityTooLarge},
	} {
		t.Run(scenario.code, func(t *testing.T) {
			response := productionSetError(&redaction.Problem{Code: scenario.code})
			wire, ok := errors.AsType[*Error](response)
			require.True(t, ok)
			require.Equal(t, scenario.status, wire.Status)
			require.Equal(t, scenario.code, wire.Code)
		})
	}
}
