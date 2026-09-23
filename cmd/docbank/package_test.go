package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/loadfile"
)

func TestPackagePreflightCLIUsesDaemonAndReturnsTypedResult(t *testing.T) {
	_ = setupVaultHome(t)
	root := t.TempDir()
	const documentID = "DOC-\x1b[31mFORGED\x1b[0m"
	const loadFile = "a\u202e.dat"
	const label = "EXT\x1b]0;FORGED\x07"
	const labelSet = "sender\tset\rFORGED\b\u009b"
	pdf, err := os.ReadFile(filepath.Join("..", "..", "document", "testdata", "scanassessment", "blank.pdf"))
	require.NoError(t, err)
	files := map[string][]byte{
		"VOL001/DATA/" + loadFile:  []byte("þDOCIDþ\x14þNATIVEþ\x14þBEGBATESþ\x14þLABELSETþ\r\nþ" + documentID + "þ\x14þNATIVES/DOC-A.pdfþ\x14þ" + label + "þ\x14þ" + labelSet + "þ\r\n"),
		"VOL001/DATA/a.opt":        []byte(documentID + ",VOL001,IMAGES/DOC-A.tif,Y,1,,\r\n"),
		"VOL001/IMAGES/DOC-A.tif":  []byte("synthetic-image"),
		"VOL001/NATIVES/DOC-A.pdf": pdf,
	}
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, contents, 0o600))
	}
	mappingPath := filepath.Join(root, "mapping.json")
	require.NoError(t, os.WriteFile(mappingPath, []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","source_ordinal":0,"canonical":"loadfile.document.id"},{"source":"NATIVE","source_ordinal":1,"canonical":"loadfile.file.native"},{"source":"BEGBATES","source_ordinal":2,"canonical":"loadfile.label.begin"},{"source":"LABELSET","source_ordinal":3,"canonical":"loadfile.label.set"}]}`), 0o600))
	out, err := runCLI(t, "package", "preflight", root, "--profile", "dat-concordance-v1", "--page-map-profile", "opt-pagecount5-v1", "--encoding", "utf-8", "--map", mappingPath, "--json")
	require.NoError(t, err)
	var result api.PackagePreflight
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, 1, result.Records)
	assert.False(t, result.Blocking)
	operation := uuid.NewString()
	out, err = runCLI(t, "package", "import", result.PreflightID, "--name", "synthetic-package",
		"--operation-id", operation, "--index-supplied-text", "--json")
	require.NoError(t, err)
	var job api.PackageImportJob
	require.NoError(t, json.Unmarshal([]byte(out), &job))
	assert.Equal(t, operation, job.OperationID)
	assert.Equal(t, 1, job.Total)
	out, err = runCLI(t, "package", "import", "status", operation, "--json")
	require.NoError(t, err)
	var status api.PackageImportJob
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Equal(t, job.JobID, status.JobID)
	deadline := time.Now().Add(10 * time.Second)
	for status.State != "complete" && status.State != "partial" && status.State != "failed" {
		require.True(t, time.Now().Before(deadline), "package import stayed in %q", status.State)
		time.Sleep(20 * time.Millisecond)
		out, err = runCLI(t, "package", "import", "status", operation, "--json")
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal([]byte(out), &status))
	}
	require.Equal(t, "complete", status.State)

	out, err = runCLI(t, "package", "list", "--limit", "50", "--json")
	require.NoError(t, err)
	var packages api.PackagePage
	require.NoError(t, json.Unmarshal([]byte(out), &packages))
	require.Len(t, packages.Items, 1)
	assert.Equal(t, status.PackageID, packages.Items[0].PackageID)

	out, err = runCLI(t, "package", "show", status.PackageID, "--json")
	require.NoError(t, err)
	var detail api.PackageDetail
	require.NoError(t, json.Unmarshal([]byte(out), &detail))
	assert.Equal(t, status.PackageID, detail.PackageID)

	out, err = runCLI(t, "package", "members", status.PackageID, "--limit", "50", "--json")
	require.NoError(t, err)
	var members api.PackageMemberPage
	require.NoError(t, json.Unmarshal([]byte(out), &members))
	require.Len(t, members.Items, 1)
	require.NotEmpty(t, members.Items[0].RowID)
	assert.Equal(t, documentID, members.Items[0].DisplayName)

	out, err = runCLI(t, "package", "record", status.PackageID, members.Items[0].RowID, "--json")
	require.NoError(t, err)
	var record api.PackageRecord
	require.NoError(t, json.Unmarshal([]byte(out), &record))
	assert.Equal(t, members.Items[0].RowID, record.RowID)
	assert.Equal(t, loadFile, record.LoadFile)
	var senderRow loadfile.Record
	require.NoError(t, json.Unmarshal(record.RawJSON, &senderRow))
	assert.Equal(t, documentID, senderRow.DocID)

	out, err = runCLI(t, "labels", "lookup", label, "--package", status.PackageID, "--json")
	require.NoError(t, err)
	var labels api.PackageLabelCandidatePage
	require.NoError(t, json.Unmarshal([]byte(out), &labels))
	require.Len(t, labels.Items, 1)
	assert.Equal(t, label, labels.Items[0].Label)
	assert.Equal(t, labelSet, labels.Items[0].LabelSet)

	for _, test := range []struct {
		name  string
		args  []string
		want  string
		lines int
	}{
		{"members", []string{"package", "members", status.PackageID}, `"DOC-\x1b[31mFORGED\x1b[0m"`, 1},
		{"record", []string{"package", "record", status.PackageID, record.RowID}, `"a\u202e.dat"`, 2},
		{"labels", []string{"labels", "lookup", label, "--package", status.PackageID}, `"EXT\x1b]0;FORGED\a"` + "\treceived\t" + `"sender\tset\rFORGED\b\u009b"`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, err := runCLI(t, test.args...)
			require.NoError(t, err)
			assert.Contains(t, out, test.want)
			assert.Equal(t, test.lines, strings.Count(out, "\n"))
			assert.False(t, strings.ContainsAny(out, "\x1b\a\r\b\u009b\u202e"), "unescaped terminal controls: %q", out)
		})
	}
}

func TestPackagePreflightCLIRequiresDeclaredCodec(t *testing.T) {
	_, err := runCLI(t, "package", "preflight", "container-id")
	require.ErrorContains(t, err, "--profile is required")
}

func TestPackagePreflightSourceMakesDirectoryAbsolute(t *testing.T) {
	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	reference, err := packagePreflightSource(".")
	require.NoError(t, err)
	assert.Equal(t, workingDirectory, reference)
}

func TestPackagePreflightSourceRejectsMissingPathAndFile(t *testing.T) {
	root := t.TempDir()
	_, err := packagePreflightSource(filepath.Join(root, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
	path := filepath.Join(root, "source.txt")
	require.NoError(t, os.WriteFile(path, []byte("synthetic"), 0o600))
	_, err = packagePreflightSource(path)
	require.ErrorContains(t, err, "must be a directory")
}
