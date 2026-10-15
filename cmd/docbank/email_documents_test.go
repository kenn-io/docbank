package main

import (
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestEmailDocumentsCLIForwardsRequestsAndPrintsResponses(t *testing.T) {
	const parentVersion = "00000000-0000-4000-8000-000000000001"
	relation := document.EmailDocumentRelation{OperationID: "cli-receipt", Order: 1, Parent: document.EmailDocumentIdentity{NodeID: 2, VersionID: parentVersion, SHA256: strings.Repeat("b", 64), Size: 1}, GenerationID: strings.Repeat("c", 64), AttachmentID: strings.Repeat("d", 64), PartPath: "1.1", Outcome: "unsupported"}
	page := document.EmailDocumentRelationPage{Total: 1, NextOperationID: "next", NextOrder: 1, Items: []document.EmailDocumentRelationStatus{{Relation: relation, State: "current"}}}
	queries := make(chan url.Values, 1)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/email-document-relations":
			queries <- r.URL.Query()
			assert.NoError(t, json.MarshalWrite(w, page))
		default:
			http.NotFound(w, r)
		}
	})
	out, err := runCLI(t, "email-documents", "relations", "--parent-version", parentVersion, "--limit", "64", "--after-operation", "prior", "--after-order", "7")
	require.NoError(t, err, out)
	var relations document.EmailDocumentRelationPage
	require.NoError(t, json.Unmarshal([]byte(out), &relations))
	require.Equal(t, page, relations)
	require.Len(t, queries, 1)
	require.Equal(t, url.Values{"parent_version_id": {parentVersion}, "limit": {"64"}, "after_operation_id": {"prior"}, "after_order": {"7"}}, <-queries)
}
