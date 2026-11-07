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
	resetFlags(rootCmd)
	var invalidStdout, invalidStderr bytes.Buffer
	invalidCode := runProcess([]string{"document", "effective", "/leases/next.txt", "--from", "2025-02-30"}, &invalidStdout, &invalidStderr)
	assert.Equal(t, exitUsage, invalidCode, invalidStderr.String())
	assert.Contains(t, invalidStderr.String(), `invalid calendar date "2025-02-30"`)
	assert.Empty(t, invalidStdout.String())
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
	_, err = runCLI(t, "document", "supersede", "/leases/next.txt", "--replaces", "/leases/old.txt")
	require.NoError(t, err)
	_, err = runCLI(t, "document", "effective", "/leases/next.txt", "--clear")
	require.NoError(t, err)
	out, err = runCLI(t, "document", "stat", "/leases/next.txt", "--json")
	require.NoError(t, err)
	var successor api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &successor))
	selector := formatNodeSelector(successor.ID)
	_, err = runCLI(t, "document", "remove", selector)
	require.NoError(t, err)

	out, err = runCLI(t, "document", "stat", "/leases/old.txt", "--json")
	require.NoError(t, err)
	var predecessor api.Node
	require.NoError(t, json.Unmarshal([]byte(out), &predecessor))
	predecessorSelector := formatNodeSelector(predecessor.ID)
	_, err = runCLI(t, "document", "remove", predecessorSelector)
	require.NoError(t, err)

	out, err = runCLI(t, "document", "supersede", "--remove", selector, "--replaces", predecessorSelector)
	require.NoError(t, err)
	assert.Contains(t, out, "Document validity updated: id:")
}

func TestSupersessionCLIOnlySendsExplicitNote(t *testing.T) {
	var requests []string
	var requestBodies []map[string]string
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/nodes/5":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 5, Path: "/next.txt", Revision: 4}))
		case "GET /api/v1/nodes/3":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 3, Path: "/old.txt", Revision: 7}))
		case "PUT /api/v1/nodes/5/supersedes/3":
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
	assert.Equal(t, "Document validity unchanged: id:3\n", out)
	out, err = runCLI(t, "document", "supersede", "id:5", "--replaces", "id:3", "--note", "")
	require.NoError(t, err)
	assert.Equal(t, "Document validity updated: id:3\n", out)
	require.Len(t, requestBodies, 2)
	assert.NotContains(t, requestBodies[0], "note")
	require.Contains(t, requestBodies[1], "note")
	assert.Empty(t, requestBodies[1]["note"])
	assert.Equal(t, []string{
		"GET /api/v1/nodes/5", "GET /api/v1/nodes/3", "PUT /api/v1/nodes/5/supersedes/3",
		"GET /api/v1/nodes/5", "GET /api/v1/nodes/3", "PUT /api/v1/nodes/5/supersedes/3",
	}, requests)
}

func TestEffectiveCLIClearTrashedFile(t *testing.T) {
	var requests []string
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/nodes/5":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 5, Revision: 7, TrashedAt: "2025-01-01T00:00:00Z"}))
		case "DELETE /api/v1/nodes/5/effective-window":
			assert.Equal(t, `"7"`, r.Header.Get("If-Match"))
			w.Header().Set("ETag", `"8"`)
			assert.NoError(t, json.MarshalWrite(w, api.ValidityReceipt{Node: api.Node{ID: 5, Revision: 8, TrashedAt: "2025-01-01T00:00:00Z"}, Changed: true}))
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	})
	out, err := runCLI(t, "document", "effective", "id:5", "--clear")
	require.NoError(t, err)
	assert.Equal(t, "Effective window updated: id:5\n", out)
	assert.Equal(t, []string{"GET /api/v1/nodes/5", "DELETE /api/v1/nodes/5/effective-window"}, requests)
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
		{[]string{"document", "effective", "/new.txt"}, "provide --from or --to"},
		{[]string{"document", "effective", "/new.txt", "--from", "", "--to", "2025-03-01"}, "use --clear"},
	} {
		_, err := runCLI(t, tc.args...)
		require.ErrorContains(t, err, tc.want)
	}
}

func TestValidityTextOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		view api.NodeValidity
		want string
	}{
		{
			name: "notes",
			view: api.NodeValidity{
				Node: api.Node{ID: 5, Path: "/lease.txt", Revision: 1}, State: "superseded", AsOf: "2025-06-01",
				Predecessors: []api.SupersessionLink{{PredecessorID: 3, Note: "annual renewal"}, {PredecessorID: 4}},
				Successors:   []api.SupersessionLink{{SuccessorID: 6, Note: "corrected terms"}, {SuccessorID: 7}},
			},
			want: "\"/lease.txt\": superseded (as of 2025-06-01)\nReplaces: id:3 \"annual renewal\"\nReplaces: id:4\nSuccessor: id:6 \"corrected terms\"\nSuccessor: id:7\n",
		},
		{
			name: "control characters stay data",
			view: api.NodeValidity{
				Node: api.Node{ID: 5, Path: "/lease\x1b[31m.txt", Revision: 1}, State: "superseded", AsOf: "2025-06-01",
				Predecessors: []api.SupersessionLink{{PredecessorID: 3, Note: "a\nSuccessor: id:9"}},
			},
			want: "\"/lease\\x1b[31m.txt\": superseded (as of 2025-06-01)\nReplaces: id:3 \"a\\nSuccessor: id:9\"\n",
		},
		{
			name: "snapshot path",
			// Another client renamed the file after selector resolution.
			view: api.NodeValidity{
				Node: api.Node{ID: 5, Path: "/renamed-lease.txt", Revision: 2}, State: "not_yet_effective", AsOf: "2025-06-01",
			},
			want: "\"/renamed-lease.txt\": not yet effective (as of 2025-06-01)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/nodes/5":
					assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 5, Path: "/lease.txt", Revision: 1}))
				case "GET /api/v1/nodes/5/validity":
					assert.Equal(t, "2025-06-01", r.URL.Query().Get("as_of"))
					assert.NoError(t, json.MarshalWrite(w, tc.view))
				default:
					http.Error(w, r.URL.Path, http.StatusNotFound)
				}
			})
			out, err := runCLI(t, "document", "validity", "id:5", "--as-of", "2025-06-01")
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)
			assert.Equal(t, []string{"GET /api/v1/nodes/5", "GET /api/v1/nodes/5/validity"}, requests)
		})
	}
}
