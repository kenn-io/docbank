package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSearchValidityCLIReasons(t *testing.T) {
	cmd, out := processingTestCommand()
	report := api.DocumentSearchReport{AsOf: "2025-01-01", Results: []api.DocumentSearchResult{
		{NodeID: 2, Rank: 1, Path: "/new.txt", Validity: &api.DocumentResultValidity{State: "current"}},
		{NodeID: 1, Rank: 2, Path: "/old.txt", Validity: &api.DocumentResultValidity{State: "superseded", SupersededBy: []api.DocumentValiditySuccessor{{NodeID: 2, Path: "/new.txt"}}}},
		{NodeID: 3, Rank: 3, Path: "/expired.txt", Validity: &api.DocumentResultValidity{State: "expired"}},
		{NodeID: 4, Rank: 4, Path: "/future.txt", Validity: &api.DocumentResultValidity{State: "not_yet_effective"}},
	}}
	require.NoError(t, writeDocumentSearchReport(cmd, report, false, true, nil))
	require.Contains(t, out.String(), "/old.txt (superseded by /new.txt)")
	require.Contains(t, out.String(), "/expired.txt (expired)")
	require.Contains(t, out.String(), "/future.txt (not yet effective)")
	hostile := api.DocumentSearchReport{Results: []api.DocumentSearchResult{
		{NodeID: 1, Rank: 1, Path: "/old.txt", Validity: &api.DocumentResultValidity{State: "superseded", SupersededBy: []api.DocumentValiditySuccessor{{NodeID: 2, Path: "/new\n99\t/x\x1b[31m.txt"}}}},
	}}
	cmd, out = processingTestCommand()
	require.NoError(t, writeDocumentSearchReport(cmd, hostile, false, true, nil))
	require.Contains(t, out.String(), "/old.txt (superseded by /new 99 /x.txt)")
	require.NotContains(t, out.String(), "\x1b")
	cmd, out = processingTestCommand()
	require.NoError(t, writeDocumentSearchAgent(cmd, "lease", report, nil))
	require.Contains(t, out.String(), "validity=superseded")
	require.Contains(t, out.String(), `validity_reason="(superseded by /new.txt)"`)
}
