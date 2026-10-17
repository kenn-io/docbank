package main

import (
	"bytes"
	"encoding/json/v2"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactionPrepareArguments(t *testing.T) {
	for _, args := range [][]string{{"prepare", "relative"}, {"prepare", "/source.pdf", "--version", "bad"}} {
		command := newRedactionCommand()
		command.SetArgs(args)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		err := command.Execute()
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true))
	}
}

func TestRedactionPreparationMemoryAdvice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.MarshalWrite(w, api.NewError(503, "redaction_preparation_memory", "daemon memory above renderer ceiling"))
	}))
	defer server.Close()
	connection := daemonconn.New(server.URL, "synthetic-key")
	defer func() { require.NoError(t, connection.Close()) }()
	_, cause := connection.PrepareNativePDF(t.Context(), processing.RedactionPreparationRequest{
		Selection: store.PageBinding{NodeID: 1, Revision: 1, Source: document.PageSource{
			VersionID: "00000000-0000-4000-8000-000000000001", SHA256: strings.Repeat("a", 64), Size: 1}}})
	err := redactionPreparationCLIError(cause)
	require.ErrorIs(t, err, cause)
	require.Equal(t, exitBusy, commandExitCode(err, true))
	hint := commandErrorHint(newRedactionCommand(), err, exitBusy, true)
	require.Contains(t, hint, "memory")
	require.NotContains(t, hint, "jobs")
}
