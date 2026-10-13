package main

import (
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestEmailDocumentsCLIForwardsRequestsAndPrintsResponses(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	const token = "synthetic-proof-token"
	const parentVersion = "00000000-0000-4000-8000-000000000001"
	receipt := document.EmailDocumentPublicationReceipt{OperationID: "cli-receipt", RequestDigest: strings.Repeat("a", 64), CreatedAt: "2026-01-01T00:00:00Z", InventoryState: "complete"}
	page := document.EmailDocumentRelationPage{Total: 64, NextOperationID: "next", NextOrder: 64}
	for i := range 64 {
		relation := document.EmailDocumentRelation{OperationID: receipt.OperationID, Order: i + 1, Parent: document.EmailDocumentIdentity{NodeID: 2, VersionID: parentVersion, SHA256: strings.Repeat("b", 64), Size: 1}, GenerationID: strings.Repeat("c", 64), AttachmentID: strings.Repeat("d", 64), PartPath: fmt.Sprintf("1.%d", i+1), Outcome: "unsupported"}
		receipt.Relations = append(receipt.Relations, relation)
		page.Items = append(page.Items, document.EmailDocumentRelationStatus{Relation: relation, State: "current"})
	}
	queries := make(chan url.Values, 1)
	releases := make(chan struct{ id, digest string }, 1)
	runs := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: daemonconn.Service, Version: version.Version, PID: os.Getpid()}))
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)}))
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
		case "/api/v1/trash/empty":
			var input struct {
				Run bool `json:"run"`
			}
			assert.NoError(t, json.UnmarshalRead(r.Body, &input))
			runs <- input.Run
			report := api.TrashEmptyReport{CandidateRoots: 1, Deleted: 1, RetainedRoots: 1, Run: true}
			assert.NoError(t, json.MarshalWrite(w, report))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.RuntimeStore(vault).Write(daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-key", token, ""))
	require.NoError(t, err)
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
	out, err = runCLI(t, "trash", "empty", "--run")
	require.NoError(t, err, out)
	require.Contains(t, out, "retained 1 trashed root(s) referenced by email publications or photo checkouts")
	require.Contains(t, out, "deleted 1 trashed root(s)")
	out, err = runCLI(t, "email-documents", "release", receipt.OperationID, "--request-digest", receipt.RequestDigest)
	require.NoError(t, err, out)
	require.Len(t, releases, 1)
	require.Equal(t, struct{ id, digest string }{receipt.OperationID, receipt.RequestDigest}, <-releases)
	require.Len(t, runs, 1)
	require.True(t, <-runs)
}
