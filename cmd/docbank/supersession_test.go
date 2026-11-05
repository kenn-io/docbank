package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestSupersessionCLI(t *testing.T) {
	_ = setupVaultHome(t)
	old := writeSourceFile(t, "old.txt", "old lease")
	next := writeSourceFile(t, "next.txt", "new lease")
	_, err := runCLI(t, "document", "add", old, "--dest", "/leases")
	require.NoError(t, err)
	_, err = runCLI(t, "document", "add", next, "--dest", "/leases")
	require.NoError(t, err)
	_, err = runCLI(t, "document", "effective", "/leases/next.txt", "--from", "2025-01-01")
	require.NoError(t, err)
	_, err = runCLI(t, "document", "effective", "/leases/next.txt", "--to", "2025-12-31")
	require.NoError(t, err)
	out, err := runCLI(t, "document", "validity", "/leases/next.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	var window api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &window))
	assert.Equal(t, "2025-01-01", window.EffectiveFrom)
	assert.Equal(t, "2025-12-31", window.EffectiveTo)
	successorID := window.Node.ID
	out, err = runCLI(t, "document", "effective", "/leases/next.txt", "--from", "2025-02-01")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Effective window updated: id:%d\n", successorID), out)
	out, err = runCLI(t, "document", "effective", "/leases/next.txt", "--from", "2025-02-01")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Effective window unchanged: id:%d\n", successorID), out)
	out, err = runCLI(t, "document", "validity", "/leases/next.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	window = api.NodeValidity{}
	require.NoError(t, json.Unmarshal([]byte(out), &window))
	assert.Equal(t, "2025-02-01", window.EffectiveFrom)
	assert.Equal(t, "2025-12-31", window.EffectiveTo)
	out, err = runCLI(t, "document", "supersede", "/leases/next.txt", "--replaces", "/leases/old.txt", "--note", "renewed lease")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Document validity updated: id:%d\n", successorID), out)
	out, err = runCLI(t, "document", "supersede", "/leases/next.txt", "--replaces", "/leases/old.txt")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Document validity unchanged: id:%d\n", successorID), out)
	out, err = runCLI(t, "document", "validity", "/leases/old.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	var v api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	assert.Equal(t, "superseded", v.State)
	require.Len(t, v.Successors, 1)
	assert.Equal(t, "renewed lease", v.Successors[0].Note)
	_, err = runCLI(t, "document", "supersede", "/leases/next.txt", "--replaces", "/leases/old.txt", "--note", "")
	require.NoError(t, err)
	out, err = runCLI(t, "document", "validity", "/leases/old.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	require.Len(t, v.Successors, 1)
	assert.Empty(t, v.Successors[0].Note)
	resetFlags(rootCmd)
	var stdout, stderr bytes.Buffer
	code := runProcess([]string{"document", "effective", "/leases/next.txt", "--from", "2026-01-01"}, &stdout, &stderr)
	assert.Equal(t, exitUsage, code, stderr.String())
	assert.Contains(t, stderr.String(), "ends before")
	assert.NotContains(t, stdout.String(), "Effective window updated")
	_, err = runCLI(t, "document", "effective", "/leases/next.txt", "--clear")
	require.NoError(t, err)
	out, err = runCLI(t, "document", "validity", "/leases/next.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	window = api.NodeValidity{}
	require.NoError(t, json.Unmarshal([]byte(out), &window))
	assert.Empty(t, window.EffectiveFrom)
	assert.Empty(t, window.EffectiveTo)
	out, err = runCLI(t, "document", "stat", "/leases/next.txt", "--json")
	require.NoError(t, err)
	var successor api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &successor))
	selector := formatNodeSelector(successor.ID)
	_, err = runCLI(t, "document", "remove", selector)
	require.NoError(t, err)

	_, err = runCLI(t, "document", "supersede", "--remove", selector, "--replaces", "/leases/old.txt")
	require.NoError(t, err)
}

func TestValidityUsesSnapshotPath(t *testing.T) {
	var requests []string
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/nodes/42":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Path: "/lease.txt", Revision: 1}))
		case "GET /api/v1/nodes/42/validity":
			assert.Equal(t, "2025-06-01", r.URL.Query().Get("as_of"))
			// Another client renamed the file after selector resolution.
			assert.NoError(t, json.MarshalWrite(w, api.NodeValidity{
				Node:  api.Node{ID: 42, Path: "/renamed-lease.txt", Revision: 2},
				State: "current", AsOf: "2025-06-01",
			}))
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	})
	out, err := runCLI(t, "document", "validity", "id:42", "--as-of", "2025-06-01")
	require.NoError(t, err)
	assert.Equal(t, "/renamed-lease.txt: current (as of 2025-06-01)\n", out)
	assert.Equal(t, []string{"GET /api/v1/nodes/42", "GET /api/v1/nodes/42/validity"}, requests)
}

func TestValidityHumanOutputUsesNaturalStateLabel(t *testing.T) {
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/nodes/42":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Path: "/future.txt", Revision: 1}))
		case "GET /api/v1/nodes/42/validity":
			assert.NoError(t, json.MarshalWrite(w, api.NodeValidity{
				Node: api.Node{ID: 42, Path: "/future.txt", Revision: 1}, State: "not_yet_effective", AsOf: "2025-06-01",
			}))
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	})

	out, err := runCLI(t, "document", "validity", "id:42", "--as-of", "2025-06-01")
	require.NoError(t, err)
	assert.Equal(t, "/future.txt: not yet effective (as of 2025-06-01)\n", out)

	out, err = runCLI(t, "document", "validity", "id:42", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	var view api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &view))
	assert.Equal(t, "not_yet_effective", view.State)
}

func TestSupersessionCLIOnlySendsExplicitNote(t *testing.T) {
	var requests []string
	var requestBodies []map[string]string
	successorResolved := false
	predecessorResolved := false
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/nodes/5":
			successorResolved = true
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 5, Path: "/next.txt", Revision: 4}))
		case "GET /api/v1/nodes/3":
			assert.True(t, successorResolved)
			predecessorResolved = true
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 3, Path: "/old.txt", Revision: 7}))
		case "PUT /api/v1/nodes/5/supersedes/3":
			assert.True(t, predecessorResolved)
			// The store-backed regression exercises trash/restore before this write.
			var body map[string]string
			assert.NoError(t, json.UnmarshalRead(r.Body, &body))
			requestBodies = append(requestBodies, body)
			changed, revision := false, int64(7)
			if _, present := body["note"]; present {
				changed, revision = true, 8
			}
			w.Header().Set("ETag", fmt.Sprintf("%q", strconv.FormatInt(revision, 10)))
			assert.NoError(t, json.MarshalWrite(w, api.ValidityReceipt{Node: api.Node{ID: 3, Revision: revision}, Changed: changed}))
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	})

	out, err := runCLI(t, "document", "supersede", "id:5", "--replaces", "id:3")
	require.NoError(t, err)
	assert.Equal(t, "Document validity unchanged: id:5\n", out)
	out, err = runCLI(t, "document", "supersede", "id:5", "--replaces", "id:3", "--note", "")
	require.NoError(t, err)
	assert.Equal(t, "Document validity updated: id:5\n", out)
	require.Len(t, requestBodies, 2)
	assert.NotContains(t, requestBodies[0], "note")
	require.Contains(t, requestBodies[1], "note")
	assert.Empty(t, requestBodies[1]["note"])
	assert.Equal(t, []string{
		"GET /api/v1/nodes/5", "GET /api/v1/nodes/3", "PUT /api/v1/nodes/5/supersedes/3",
		"GET /api/v1/nodes/5", "GET /api/v1/nodes/3", "PUT /api/v1/nodes/5/supersedes/3",
	}, requests)
}

func TestSupersessionCLIRejectsInvalidFlags(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"document", "supersede", "/new.txt"}, "--replaces"},
		{[]string{"document", "supersede", "/new.txt", "--replaces", "/old.txt", "--remove", "--note", "note"}, "--note"},
		{[]string{"document", "effective", "/new.txt", "--clear", "--from", "2025-01-01"}, "--clear"},
		{[]string{"document", "effective", "/new.txt", "--from", "2025-02-30"}, "invalid calendar date"},
		{[]string{"document", "effective", "/new.txt"}, "provide --from or --to"},
		{[]string{"document", "effective", "/new.txt", "--from", "", "--to", "2025-03-01"}, "use --clear"},
		{[]string{"document", "validity", "/new.txt", "--as-of", "2025-02-30"}, "invalid calendar date"},
	} {
		_, err := runCLI(t, tc.args...)
		require.ErrorContains(t, err, tc.want)
	}
}
