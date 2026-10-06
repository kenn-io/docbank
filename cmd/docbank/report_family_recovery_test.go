package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
	"go.kenn.io/docbank/sqlite"
)

func TestReportFamilyRecovery(t *testing.T) {
	f := newFamilyReportFixture(t, "shared")
	var configuration bytes.Buffer
	require.NoError(t, toml.NewEncoder(&configuration).Encode(map[string]any{
		"backup": map[string]string{"repo": filepath.Join(t.TempDir(), "backup")},
	}))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "config.toml"),
		configuration.Bytes(), 0o600))
	originals := []reportOriginal{f.originals["Q"], f.originals["R"], f.originals["X"]}
	slices.SortFunc(originals, func(a, b reportOriginal) int {
		return cmp.Compare(a.member.NodeID, b.member.NodeID)
	})
	var packets []string
	capture := func(trashed bool) familyReportObservation {
		observed := f.captureRecovery(t, trashed)
		packets = append(packets, observed.packet)
		return observed
	}
	var archives []struct {
		path    string
		receipt bundle.Receipt
	}
	export := func() {
		path, receipt := exportReportOriginals(t, originals)
		assertOriginalArchive(t, path, receipt, originals)
		archives = append(archives, struct {
			path    string
			receipt bundle.Receipt
		}{path, receipt})
	}

	stop := f.start(t, f.root)
	initial := capture(false)
	relations := recoveryRelations(t, initial.packet)
	maintainFamilyStorage(t)
	f.assertRecoveryNodes(t, false)
	assertRecoveryPacketUnchanged(t, initial)
	maintained := capture(false)
	require.ElementsMatch(t, relations, recoveryRelations(t, maintained.packet))
	export()
	_, err := runCLI(t, "backup", "init")
	require.NoError(t, err)
	liveHistory := reportHistory(t)
	liveSnapshot := backupReportSnapshot(t, "family-live")

	_, err = runCLI(t, "rm", formatNodeSelector(f.identity("X").NodeID))
	require.NoError(t, err)
	f.assertRecoveryNodes(t, true)
	out, err := runCLI(t, "trash", "empty", "--run", "--json")
	require.NoError(t, err)
	var emptied api.TrashEmptyReport
	require.NoError(t, json.Unmarshal([]byte(out), &emptied))
	require.EqualValues(t, 1, emptied.RetainedRoots)
	require.Zero(t, emptied.Deleted)
	_, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)
	f.assertRecoveryNodes(t, true)
	capture(true)
	assertFamilyCLIRefused(t, f.request)
	trashedHistory := reportHistory(t)
	trashedSnapshot := backupReportSnapshot(t, "family-trashed")
	liveRoot := restoreReportSnapshot(t, liveSnapshot)
	trashedRoot := restoreReportSnapshot(t, trashedSnapshot)
	stop()
	assertRecoveryNoJobs(t, f.root)

	stop = f.start(t, liveRoot)
	require.Equal(t, liveHistory, reportHistory(t))
	f.assertRecoveryNodes(t, false)
	assertRecoveryHandleUnavailable(t, initial.summary.ID)
	live := capture(false)
	require.NotEqual(t, initial.summary.ID, live.summary.ID)
	require.NotEqual(t, maintained.summary.ID, live.summary.ID)
	require.ElementsMatch(t, relations, recoveryRelations(t, live.packet))
	export()
	stop()
	assertRecoveryNoJobs(t, liveRoot)

	stop = f.start(t, trashedRoot)
	require.Equal(t, trashedHistory, reportHistory(t))
	f.assertRecoveryNodes(t, true)
	incomplete := capture(true)
	assertFamilyCLIRefused(t, f.request)
	_, err = runCLI(t, "restore", formatNodeSelector(f.identity("X").NodeID), "--json")
	require.NoError(t, err)
	f.assertRecoveryNodes(t, false)
	reconnected := capture(false)
	require.ElementsMatch(t, relations, recoveryRelations(t, reconnected.packet))
	export()
	assertRecoveryPacketUnchanged(t, incomplete)
	stop()
	assertRecoveryNoJobs(t, trashedRoot)

	verifyFamilyPackets(t, packets...)
	for _, archive := range archives {
		assertOriginalArchive(t, archive.path, archive.receipt, originals)
	}
}

func (f *familyReportFixture) captureRecovery(t *testing.T, trashed bool) familyReportObservation {
	t.Helper()
	request := f.request
	want := familyReportExpectation{
		counts: []report.Counts{
			{Hits: 1, HitsPlusFamily: 2, UniqueHits: 1, UniqueFamilies: 0, UniqueHitsPlusFamily: 2},
			{Hits: 1, HitsPlusFamily: 2, UniqueHits: 1, UniqueFamilies: 0, UniqueHitsPlusFamily: 2},
		},
		csv:     [][]string{{"1", "2", "1", "0", "2"}, {"1", "2", "1", "0", "2"}},
		members: []report.Identity{f.identity("Q"), f.identity("R")},
		edges: [][2]report.Identity{
			{f.identity("Q"), f.identity("X")}, {f.identity("R"), f.identity("X")},
		},
		familySizes: []int{2},
	}
	if trashed {
		request.CoverageMode = "available_only"
		want.counts = []report.Counts{
			{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1, UniqueFamilies: 1, UniqueHitsPlusFamily: 1},
			{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1, UniqueFamilies: 1, UniqueHitsPlusFamily: 1},
		}
		want.csv = [][]string{{"1", "1", "1", "1", "1"}, {"1", "1", "1", "1", "1"}}
		want.edges, want.familySizes, want.incomplete = nil, []int{1, 1}, 2
	}
	observed := captureFamilyCLI(t, request)
	f.assertReport(t, observed, want)
	if !trashed {
		warnings := strings.Join(observed.summary.Coverage.Warnings, "\n")
		require.Contains(t, warnings, f.identity("X").VersionID)
		require.Contains(t, warnings, "shared attachment child")
		require.Contains(t, warnings, "2 parents")
	}
	return observed
}

func recoveryRelations(t *testing.T, packet string) []report.Relation {
	t.Helper()
	z, err := zip.OpenReader(packet)
	require.NoError(t, err)
	defer func() { require.NoError(t, z.Close()) }()
	relations := familyPacketRows[report.Relation](t, z, "families.jsonl")
	for _, relation := range relations {
		require.NotEmpty(t, relation.EvidenceID)
		require.Regexp(t, `^[0-9a-f]{64}$`, relation.EvidenceSHA256)
	}
	return relations
}

func (f *familyReportFixture) assertRecoveryNodes(t *testing.T, trashed bool) {
	t.Helper()
	for _, key := range []string{"Q", "R", "X"} {
		want := f.originals[key].member
		out, err := runCLI(t, "stat", formatNodeSelector(want.NodeID), "--json")
		require.NoError(t, err)
		var node api.Node
		require.NoError(t, json.Unmarshal([]byte(out), &node))
		require.Equal(t, want.NodeID, node.ID)
		require.Equal(t, want.VersionID, node.CurrentVersionID)
		require.Equal(t, want.SHA256, node.BlobHash)
		require.Equal(t, want.Size, node.Size)
		if trashed && key == "X" {
			require.NotEmpty(t, node.TrashedAt)
		} else {
			require.Empty(t, node.TrashedAt)
		}
	}
}

func assertRecoveryPacketUnchanged(t *testing.T, observed familyReportObservation) {
	t.Helper()
	require.Equal(t, observed.summary, showReport(t, observed.summary.ID))
	path := filepath.Join(t.TempDir(), "frozen.zip")
	_, err := runCLI(t, "search-export", "download", observed.summary.ID, "--output", path)
	require.NoError(t, err)
	want, err := os.ReadFile(observed.packet)
	require.NoError(t, err)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, actual)
}

func assertRecoveryHandleUnavailable(t *testing.T, id string) {
	t.Helper()
	_, err := runCLI(t, "search-export", "show", id, "--json")
	requireReportProblem(t, err, "report_unavailable")
	path := filepath.Join(t.TempDir(), "unavailable.zip")
	_, err = runCLI(t, "search-export", "download", id, "--output", path)
	requireReportProblem(t, err, "report_unavailable")
	require.NoFileExists(t, path)
}

func assertRecoveryNoJobs(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "docbank.db")
	wal, err := os.Stat(path + "-wal")
	if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
		require.Zero(t, wal.Size(), "immutable read requires an empty WAL")
	}
	db, err := store.DefaultSQLiteDriver().Open(path,
		sqlite.OpenOptions{Access: sqlite.ReadOnlyImmutable})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var jobs int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT
		(SELECT COUNT(*) FROM rendition_jobs) + (SELECT COUNT(*) FROM embedding_jobs)`).Scan(&jobs))
	require.Zero(t, jobs, "recovery must not enqueue provider processing")
}

func maintainFamilyStorage(t *testing.T) {
	t.Helper()
	// Repack selects packs with fewer than half their entries live. One dead
	// original among the fixture's rendition artifacts would never be selected.
	for i := range 32 {
		data := append(bytes.Repeat([]byte{0, 255, 85, 170}, 128), byte(i))
		source := writeSourceFile(t, fmt.Sprintf("discard-%02d.bin", i), string(data))
		_, err := runCLI(t, "add", source, "--dest", "/reclaim")
		require.NoError(t, err)
	}
	out, err := runCLI(t, "storage", "pack", "--json")
	require.NoError(t, err)
	var packed api.StoragePackReport
	require.NoError(t, json.Unmarshal([]byte(out), &packed))
	require.Equal(t, 1, packed.PacksSealed)
	require.Positive(t, packed.BlobsPacked)
	require.Zero(t, packed.BlobsMissing)
	require.Zero(t, packed.BlobsCorrupt)
	require.Zero(t, packed.BlobsDeferredOversized)
	require.False(t, packed.BudgetExhausted)
	before := recoveryStorageStatus(t)
	require.Equal(t, 1, before.Packs)
	require.Positive(t, before.PackedBlobs)
	require.Zero(t, before.LooseBlobs)
	require.Zero(t, before.DeadPackedBytes)
	_, err = runCLI(t, "rm", "/reclaim")
	require.NoError(t, err)
	out, err = runCLI(t, "trash", "empty", "--run", "--json")
	require.NoError(t, err)
	var emptied api.TrashEmptyReport
	require.NoError(t, json.Unmarshal([]byte(out), &emptied))
	require.EqualValues(t, 1, emptied.Deleted)
	require.Zero(t, emptied.RetainedRoots)
	_, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)
	dead := recoveryStorageStatus(t)
	require.Equal(t, 1, dead.Packs)
	require.Positive(t, dead.DeadPackedBytes)
	require.Positive(t, dead.PackedBlobs)
	require.Less(t, 2*dead.PackedBlobs, before.PackedBlobs, "pack must qualify for repack")
	out, err = runCLI(t, "storage", "repack", "--min-age", "1ns",
		"--min-dead-bytes", "1", "--json")
	require.NoError(t, err)
	var repacked api.StorageRepackReport
	require.NoError(t, json.Unmarshal([]byte(out), &repacked))
	require.Equal(t, 1, repacked.PacksSelected)
	require.Equal(t, 1, repacked.PacksRewritten)
	require.Equal(t, 1, repacked.PacksRemoved)
	require.Positive(t, repacked.BlobsRepacked)
	require.Positive(t, repacked.BytesRepacked)
	require.Zero(t, repacked.PacksDeferredOversized)
	require.False(t, repacked.BudgetExhausted)
	require.Zero(t, recoveryStorageStatus(t).DeadPackedBytes)
}

func recoveryStorageStatus(t *testing.T) api.StorageStatus {
	t.Helper()
	out, err := runCLI(t, "storage", "status", "--json")
	require.NoError(t, err)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	return status
}
