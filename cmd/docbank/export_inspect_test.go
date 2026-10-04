package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestExportInspectionPreflight(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "absent-vault")
	t.Setenv("DOCBANK_HOME", vault)
	id := uuid.New().String()
	for _, args := range [][]string{
		{"show-plan", "bad"}, {"problems", "bad"},
		{"problems", id, "--after", "-1"}, {"problems", id, "--after", "2600001"},
	} {
		_, err := runCLI(t, append([]string{"export"}, args...)...)
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true), args)
		require.NoDirExists(t, vault)
	}
}

func TestExportCLIInspection(t *testing.T) {
	setupVaultHome(t)
	dir := t.TempDir()
	addArgs := []string{"add"}
	for i := range 51 {
		addArgs = append(addArgs, filepath.Join(dir, fmt.Sprintf("synthetic-%02d.bin", i)))
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("synthetic-%02d.bin", i)),
			[]byte(fmt.Sprintf("synthetic original %d", i)), 0o600))
	}
	_, err := runCLI(t, append(addArgs, "--dest", "/")...)
	require.NoError(t, err)
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	var members []bundle.Member
	for i := range 51 {
		node, lookupErr := connection.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{
			Query: &apiclient.ResolvePathQuery{Path: fmt.Sprintf("/synthetic-%02d.bin", i)},
		})
		require.NoError(t, lookupErr)
		members = append(members, bundle.Member{NodeID: node.ID, VersionID: node.CurrentVersionID,
			SHA256: node.BlobHash, Size: node.Size})
	}
	source, err := connection.API().CreateExportSource(t.Context(),
		&apiclient.CreateExportSourceRequestOptions{Body: &bundle.SourceRequest{
			OperationID: uuid.New().String(), Kind: "explicit", Members: members,
		}})
	require.NoError(t, err)
	plan, err := connection.API().CreateExportPlan(t.Context(),
		&apiclient.CreateExportPlanRequestOptions{Body: &bundle.PlanRequest{
			OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
			Roles: []bundle.RolePolicy{{Role: "original"}, {Role: "text", AllowUnavailable: true}},
		}})
	require.NoError(t, err)
	output, err := runCLI(t, "export", "show-plan", plan.ID, "--json")
	require.NoError(t, err)
	var got bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.Equal(t, *plan, got)
	output, err = runCLI(t, "export", "show-plan", plan.ID)
	require.NoError(t, err)
	require.Contains(t, output, "admission deadline: "+plan.ExpiresAt)
	require.Contains(t, output, "text")
	require.Contains(t, output, plan.Fingerprint)
	for _, offset := range []int{0, 50, 51} {
		output, err = runCLI(t, "export", "problems", plan.ID, "--after", strconv.Itoa(offset), "--json")
		require.NoError(t, err)
		var page bundle.OutputProblems
		require.NoError(t, json.Unmarshal([]byte(output), &page))
		require.Equal(t, plan.ID, page.PlanID)
		require.Equal(t, plan.Fingerprint, page.Fingerprint)
		require.Equal(t, offset, page.After)
		require.Equal(t, 51, page.Total)
		want := min(50, 51-offset)
		require.Len(t, page.Items, want)
		if offset == 0 {
			require.Equal(t, 50, page.Next)
		} else {
			require.Zero(t, page.Next)
		}
		for i, problem := range page.Items {
			require.Equal(t, members[offset+i].VersionID, problem.VersionID)
			require.Equal(t, members[offset+i].NodeID, problem.NodeID)
			require.Equal(t, "text", problem.Role)
		}
	}
	output, err = runCLI(t, "export", "problems", plan.ID)
	require.NoError(t, err)
	require.Contains(t, output, "docbank export problems "+plan.ID+" --after 50")
	require.Contains(t, output, "problems 1–50 of 51")
	output, err = runCLI(t, "export", "problems", plan.ID, "--after", "50")
	require.NoError(t, err)
	require.Contains(t, output, "problems 51–51 of 51")
	output, err = runCLI(t, "export", "problems", plan.ID, "--after", "51")
	require.NoError(t, err)
	require.Contains(t, output, "No more unavailable outputs")
	_, err = runCLI(t, "export", "problems", plan.ID, "--after", "52")
	require.ErrorContains(t, err, "export_conflict")
	require.Equal(t, exitGeneral, commandExitCode(err, true))
	_, err = runCLI(t, "export", "show-plan", uuid.New().String())
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	_, err = runCLI(t, "put", writeSourceFile(t, "replacement.bin", "replacement"),
		"/synthetic-00.bin", "--progress", "plain")
	require.NoError(t, err)
	output, err = runCLI(t, "export", "show-plan", plan.ID, "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.Equal(t, *plan, got)
}

func TestExportInspectionOutput(t *testing.T) {
	var output bytes.Buffer
	page := bundle.OutputProblems{PlanID: uuid.New().String(), Items: []bundle.OutputProblem{}}
	require.NoError(t, writeExportProblems(&output, page))
	require.Contains(t, output.String(), "No unavailable outputs.")
	require.NotContains(t, output.String(), "Next page:")
	output.Reset()
	page.Total = 1
	page.Items = []bundle.OutputProblem{{NodeID: 17, VersionID: uuid.New().String(),
		Role: "attachment_original", PartPath: "1\n2", Reason: "missing \"quoted\" output\n"}}
	require.NoError(t, writeExportProblems(&output, page))
	require.Contains(t, output.String(), `part "1\n2"`)
	require.Contains(t, output.String(), `"missing \"quoted\" output\n"`)
	output.Reset()
	plan := bundle.Plan{DocumentRows: 2, DuplicatePolicy: "preserve", Volumes: 3,
		VolumeLimits: &bundle.VolumeLimits{RoleBytes: 1024, Roles: 2},
		Roles: []bundle.RolePolicy{{Role: "text", AllowUnavailable: true,
			ProfileFingerprint: strings.Repeat("a", 64), RecipeSHA256: strings.Repeat("b", 64)}},
		Counts: &bundle.OutputCounts{Messages: 1, Attachments: 2, EmailPDFs: 3,
			AttachmentPDFs: 4, Pages: 5, Collapsed: 6, Unavailable: 7, UnavailableInventories: 8}}
	require.NoError(t, writeExportPlanHeader(&output, plan))
	for _, want := range []string{"document rows: 2", "duplicate policy: preserve", "volumes: 3",
		"volume limits: 1024 role bytes · 2 roles", "profile: " + strings.Repeat("a", 64),
		"recipe: " + strings.Repeat("b", 64), "allow unavailable: true", "messages: 1",
		"attachments: 2", "email PDFs: 3", "attachment PDFs: 4", "pages: 5", "collapsed: 6",
		"unavailable: 7", "unavailable inventories: 8"} {
		require.Contains(t, output.String(), want)
	}
}
