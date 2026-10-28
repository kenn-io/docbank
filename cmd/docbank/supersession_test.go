package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestSupersessionCLI(t *testing.T) {
	_ = setupVaultHome(t)
	old := writeSourceFile(t, "old.txt", "old lease")
	next := writeSourceFile(t, "next.txt", "new lease")
	_, err := runCLI(t, "add", old, "--dest", "/leases")
	require.NoError(t, err)
	_, err = runCLI(t, "add", next, "--dest", "/leases")
	require.NoError(t, err)
	_, err = runCLI(t, "effective", "/leases/next.txt", "--from", "2025-01-01")
	require.NoError(t, err)
	_, err = runCLI(t, "effective", "/leases/next.txt", "--to", "2025-12-31")
	require.NoError(t, err)
	out, err := runCLI(t, "validity", "/leases/next.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	var window api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &window))
	assert.Equal(t, "2025-01-01", window.EffectiveFrom)
	assert.Equal(t, "2025-12-31", window.EffectiveTo)
	successorID := window.Node.ID
	_, err = runCLI(t, "effective", "/leases/next.txt", "--from", "2025-02-01")
	require.NoError(t, err)
	out, err = runCLI(t, "validity", "/leases/next.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	window = api.NodeValidity{}
	require.NoError(t, json.Unmarshal([]byte(out), &window))
	assert.Equal(t, "2025-02-01", window.EffectiveFrom)
	assert.Equal(t, "2025-12-31", window.EffectiveTo)
	_, err = runCLI(t, "supersede", "/leases/next.txt", "--replaces", "/leases/old.txt", "--note", "renewed lease")
	require.NoError(t, err)
	out, err = runCLI(t, "supersede", "/leases/next.txt", "--replaces", "/leases/old.txt")
	require.NoError(t, err)
	assert.Contains(t, out, fmt.Sprintf("Document validity updated: id:%d (changed: false)", successorID))
	out, err = runCLI(t, "validity", "/leases/old.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	var v api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	assert.Equal(t, "superseded", v.State)
	require.Len(t, v.Successors, 1)
	assert.Equal(t, "renewed lease", v.Successors[0].Note)
	_, err = runCLI(t, "supersede", "/leases/next.txt", "--replaces", "/leases/old.txt", "--note", "")
	require.NoError(t, err)
	out, err = runCLI(t, "validity", "/leases/old.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	require.Len(t, v.Successors, 1)
	assert.Empty(t, v.Successors[0].Note)
	_, err = runCLI(t, "effective", "/leases/next.txt", "--clear")
	require.NoError(t, err)
	out, err = runCLI(t, "validity", "/leases/next.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	window = api.NodeValidity{}
	require.NoError(t, json.Unmarshal([]byte(out), &window))
	assert.Empty(t, window.EffectiveFrom)
	assert.Empty(t, window.EffectiveTo)
	out, err = runCLI(t, "stat", "/leases/next.txt", "--json")
	require.NoError(t, err)
	var successor api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &successor))
	selector := formatNodeSelector(successor.ID)
	_, err = runCLI(t, "rm", selector)
	require.NoError(t, err)

	_, err = runCLI(t, "supersede", "--remove", selector, "--replaces", "/leases/old.txt")
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
	out, err := runCLI(t, "validity", "id:42", "--as-of", "2025-06-01")
	require.NoError(t, err)
	assert.Equal(t, "/renamed-lease.txt: current (as of 2025-06-01)\n", out)
	assert.Equal(t, []string{"GET /api/v1/nodes/42", "GET /api/v1/nodes/42/validity"}, requests)
}

func TestEffectiveCLIMergedWindowExitsUsage(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "lease.txt", "signed lease")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	_, err = runCLI(t, "effective", "/lease.txt", "--to", "2025-03-01")
	require.NoError(t, err)

	resetFlags(rootCmd)
	var stdout, stderr bytes.Buffer
	code := runProcess([]string{"effective", "/lease.txt", "--from", "2025-04-01"}, &stdout, &stderr)
	assert.Equal(t, exitUsage, code, stderr.String())
	assert.Contains(t, stderr.String(), "ends before")
	assert.NotContains(t, stdout.String(), "Effective window updated")
	out, err := runCLI(t, "validity", "/lease.txt", "--json")
	require.NoError(t, err)
	var view api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &view))
	assert.Empty(t, view.EffectiveFrom)
	assert.Equal(t, "2025-03-01", view.EffectiveTo)
}

func TestEffectiveCLIRejectsAuditedWindowUpdate(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "lease.txt", "signed lease")
	_, err := runCLI(t, "add", source, "--dest", "/leases")
	require.NoError(t, err)
	out, err := runCLI(t, "audit", "enable", "/leases", "--json")
	require.NoError(t, err)
	var preview api.AuditEnrollmentPreview
	require.NoError(t, json.Unmarshal([]byte(out), &preview))
	_, err = runCLI(t, "audit", "enable", "--run", "--token", preview.PreviewToken, "--acknowledge-permanent-retention")
	require.NoError(t, err)
	out, err = runCLI(t, "validity", "/leases/lease.txt", "--json")
	require.NoError(t, err)
	var before api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &before))

	out, err = runCLI(t, "effective", "/leases/lease.txt", "--to", "2026-12-31")
	require.ErrorContains(t, err, "mutation is not supported for an audited vault")
	assert.NotContains(t, out, "Effective window updated")
	out, err = runCLI(t, "validity", "/leases/lease.txt", "--as-of", "2025-06-01", "--json")
	require.NoError(t, err)
	var view api.NodeValidity
	require.NoError(t, json.Unmarshal([]byte(out), &view))
	assert.Empty(t, view.EffectiveFrom)
	assert.Empty(t, view.EffectiveTo)
	assert.Equal(t, before.Node.Revision, view.Node.Revision)
}

func TestSupersessionCLIRejectsInvalidFlags(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"supersede", "/new.txt"}, "--replaces"},
		{[]string{"supersede", "/new.txt", "--replaces", "/old.txt", "--remove", "--note", "note"}, "--note"},
		{[]string{"effective", "/new.txt", "--clear", "--from", "2025-01-01"}, "--clear"},
		{[]string{"effective", "/new.txt", "--from", "2025-02-30"}, "invalid calendar date"},
		{[]string{"effective", "/new.txt"}, "provide --from or --to"},
		{[]string{"effective", "/new.txt", "--from", "", "--to", "2025-03-01"}, "use --clear"},
		{[]string{"validity", "/new.txt", "--as-of", "2025-02-30"}, "invalid calendar date"},
	} {
		_, err := runCLI(t, tc.args...)
		require.ErrorContains(t, err, tc.want)
	}
}
