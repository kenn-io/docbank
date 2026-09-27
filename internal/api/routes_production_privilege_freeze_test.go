package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
)

func TestProductionPrivilegeFreezePreservesApprovalGateCodes(t *testing.T) {
	for _, code := range []documentproduction.ProblemCode{
		documentproduction.ProblemApprovalRequired,
		documentproduction.ProblemApprovalStale,
	} {
		t.Run(string(code), func(t *testing.T) {
			err := productionPrivilegeFreezeError(&documentproduction.Problem{
				Code: code, Detail: "Synthetic private approval evidence.",
			})
			wire, ok := errors.AsType[*Error](err)
			require.True(t, ok)
			require.Equal(t, http.StatusConflict, wire.Status)
			require.Equal(t, string(code), wire.Code)
			require.NotContains(t, wire.Detail, "Synthetic private approval evidence.")
		})
	}
}
