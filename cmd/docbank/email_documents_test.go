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
	receipt := document.EmailDocumentPublicationReceipt{OperationID: "cli-receipt", RequestDigest: strings.Repeat("a", 64), CreatedAt: "2026-01-01T00:00:00Z", InventoryState: "complete"}
	relation := document.EmailDocumentRelation{OperationID: receipt.OperationID, Order: 1, Parent: document.EmailDocumentIdentity{NodeID: 2, VersionID: parentVersion, SHA256: strings.Repeat("b", 64), Size: 1}, GenerationID: strings.Repeat("c", 64), AttachmentID: strings.Repeat("d", 64), PartPath: "1.1", Outcome: "unsupported"}
	receipt.Relations = []document.EmailDocumentRelation{relation}
	page := document.EmailDocumentRelationPage{Total: 1, NextOperationID: "next", NextOrder: 1, Items: []document.EmailDocumentRelationStatus{{Relation: relation, State: "current"}}}
	queries := make(chan url.Values, 1)
	releases := make(chan struct{ id, digest string }, 1)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/email-document-publications/" + receipt.OperationID:
			if r.Method == http.MethodDelete {
				var input struct {
					RequestDigest string `json:"request_digest"`
				}
				assert.NoError(t, json.UnmarshalRead(r.Body, &input))
				releases <- struct{ id, digest string }{strings.TrimPrefix(r.URL.Path, "/api/v1/email-document-publications/"), input.RequestDigest}
				w.WriteHeader(http.StatusNoContent)
			} else {
				assert.NoError(t, json.MarshalWrite(w, receipt))
			}
		case "/api/v1/email-document-relations":
			queries <- r.URL.Query()
			assert.NoError(t, json.MarshalWrite(w, page))
		default:
			http.NotFound(w, r)
		}
	})
	out, err := runCLI(t, "email-documents", "show", receipt.OperationID)
	require.NoError(t, err, out)
	var shown document.EmailDocumentPublicationReceipt
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	require.Equal(t, receipt, shown)
	out, err = runCLI(t, "email-documents", "relations", "--parent-version", parentVersion, "--limit", "64", "--after-operation", "prior", "--after-order", "7")
	require.NoError(t, err, out)
	var relations document.EmailDocumentRelationPage
	require.NoError(t, json.Unmarshal([]byte(out), &relations))
	require.Equal(t, page, relations)
	require.Len(t, queries, 1)
	require.Equal(t, url.Values{"parent_version_id": {parentVersion}, "limit": {"64"}, "after_operation_id": {"prior"}, "after_order": {"7"}}, <-queries)
	out, err = runCLI(t, "email-documents", "release", receipt.OperationID, "--request-digest", receipt.RequestDigest)
	require.NoError(t, err, out)
	require.Len(t, releases, 1)
	require.Equal(t, struct{ id, digest string }{receipt.OperationID, receipt.RequestDigest}, <-releases)
}
