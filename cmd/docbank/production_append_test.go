package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestProductionDraftAppendCLIBoundsInputBeforeConnecting(t *testing.T) {
	_, err := runCLI(t, "production", "drafts", "append", "bad-set", "1")
	require.ErrorContains(t, err, "--etag is required")
	membersFile := filepath.Join(t.TempDir(), "members.json")
	require.NoError(t, os.WriteFile(membersFile, []byte(`[{"unknown":true}]`), 0o600))
	_, err = runCLI(t, "production", "drafts", "append", "bad-set", "1",
		"--etag", "1", "--members-file", membersFile)
	require.ErrorContains(t, err, "invalid production members JSON")
	require.NoError(t, os.WriteFile(membersFile,
		[]byte(strings.Repeat("x", redaction.MaxCommandBytes+1)), 0o600))
	_, err = runCLI(t, "production", "drafts", "append", "bad-set", "1",
		"--etag", "1", "--members-file", membersFile)
	require.ErrorContains(t, err, "members exceed")
}
