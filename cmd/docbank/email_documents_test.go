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
	receipt := document.EmailDocumentPublicationReceipt{OperationID: relation.OperationID, RequestDigest: strings.Repeat("e", 64), CreatedAt: "2026-01-01T00:00:00Z", InventoryState: "complete", Relations: []document.EmailDocumentRelation{relation}}
	queries := make(chan url.Values, 1)
	released := make(chan string, 1)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/email-document-relations":
			queries <- r.URL.Query()
			assert.NoError(t, json.MarshalWrite(w, page))
		case "/api/v1/email-document-publications/cli-receipt":
			switch r.Method {
			case http.MethodGet:
				assert.NoError(t, json.MarshalWrite(w, receipt))
			case http.MethodDelete:
				var body struct {
					RequestDigest string `json:"request_digest"`
				}
				assert.NoError(t, json.UnmarshalRead(r.Body, &body))
				released <- body.RequestDigest
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			}
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
	out, err = runCLI(t, "email-documents", "show", receipt.OperationID)
	require.NoError(t, err, out)
	var shown document.EmailDocumentPublicationReceipt
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	require.Equal(t, receipt, shown)
	out, err = runCLI(t, "email-documents", "release", receipt.OperationID, "--request-digest", receipt.RequestDigest)
	require.NoError(t, err, out)
	require.Equal(t, "released email document publication cli-receipt; child documents remain\n", out)
	require.Len(t, released, 1)
	require.Equal(t, receipt.RequestDigest, <-released)
}
