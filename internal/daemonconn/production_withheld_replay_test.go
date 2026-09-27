package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProductionWithheldClientAcceptsCanonicalMemberReplay(t *testing.T) {
	const setID = "89898989-8989-4898-8898-898989898931"
	members := []documentproduction.WithheldMember{
		{
			ID: "89898989-8989-4898-8898-898989898932", Ordinal: 1,
			SourceVersionID: "89898989-8989-4898-8898-898989898933",
			SourceSHA256:    strings.Repeat("a", 64), SourceSize: 12, FamilyOrder: 1,
			Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: "89898989-8989-4898-8898-898989898933"},
		},
		{
			ID: "89898989-8989-4898-8898-898989898934", Ordinal: 2,
			SourceVersionID: "89898989-8989-4898-8898-898989898935",
			SourceSHA256:    strings.Repeat("b", 64), SourceSize: 13, FamilyOrder: 1,
			Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: "89898989-8989-4898-8898-898989898935"},
		},
	}
	selection := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "89898989-8989-4898-8898-898989898936", SetID: setID, Revision: 1,
		PolicySHA256: strings.Repeat("c", 64), Members: members,
	}
	_, digest, err := documentproduction.CanonicalWithheldSelection(selection)
	require.NoError(t, err)
	selection.SHA256 = digest
	require.NoError(t, documentproduction.ValidateWithheldSelection(selection))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/productions/sets/"+setID+"/revisions/1/withheld-selection", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.MarshalWrite(w, selection))
	}))
	t.Cleanup(server.Close)
	request := api.ProductionWithheldSelectionCreateRequest{
		OperationID: "89898989-8989-4898-8898-898989898937", SelectionID: selection.ID,
		PolicySHA256: selection.PolicySHA256,
		Members:      []documentproduction.WithheldMember{members[1], members[0]},
	}
	actual, err := daemonconn.New(server.URL, "synthetic-api-key").
		CreateProductionWithheldSelection(t.Context(), setID, 1, request)
	require.NoError(t, err)
	require.Equal(t, selection, actual)
}
