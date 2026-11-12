package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestDocumentFactsCommands(t *testing.T) {
	f := newDocumentInspectionFixture(t)
	t.Setenv("DOCBANK_HOME", f.root)
	stop := startServe(t)
	t.Cleanup(stop)
	waitForDaemon(t, f.root)
	for _, noun := range []string{"dates", "people"} {
		output, err := runCLI(t, "document", noun, "/notes.txt", "--json", "--full")
		require.NoError(t, err)
		if noun == "dates" {
			var got api.VersionDates
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			require.Equal(t, f.nodes["notes.txt"].CurrentVersionID, got.ContentVersionID)
		} else {
			var got api.VersionPeople
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			require.Equal(t, f.nodes["notes.txt"].CurrentVersionID, got.ContentVersionID)
		}
	}
}
