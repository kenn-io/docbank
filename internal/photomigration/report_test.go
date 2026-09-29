package photomigration

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/kit/safefileio"
)

func testReport() Report {
	return Report{
		Source:    Source{Kind: SourceInstall, Identity: "catalog-sha256"},
		Schema:    Schema{CatalogVersion: 1, CatalogFingerprint: "fingerprint", EmbeddedDocbankVersion: 16},
		Counts:    Counts{Owners: 1, Assets: 2, Files: 3, Bytes: 10, Albums: 1, AlbumMemberships: 2, Shares: 1, Checkouts: 1, CheckoutEntries: 3, AIResults: 1, HiddenSetup: 1},
		Vectors:   []VectorGeneration{{ID: 1, Fingerprint: "vectors", State: "active", Rebuildable: true}},
		Capacity:  Capacity{SourceBytes: 10, UniqueBlobBytes: 8, MinimumContentBytes: 8},
		CreatedAt: "2026-09-25T00:00:00Z",
	}
}

func TestReportCanonicalEncodingIsStable(t *testing.T) {
	raw, err := EncodeReport(testReport())
	if err != nil {
		t.Fatal(err)
	}
	other, err := EncodeReport(testReport())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, other) || bytes.Contains(raw, []byte("/private")) {
		t.Fatalf("report is not stable or contains a host path: %s", raw)
	}
}

func TestReportAllowsRetainedVersionsToExceedCurrentFileBytes(t *testing.T) {
	report := testReport()
	report.Capacity = Capacity{SourceBytes: 10, UniqueBlobBytes: 20, MinimumContentBytes: 20}
	if err := ValidateReport(report); err != nil {
		t.Fatalf("rejected retained-version capacity: %v", err)
	}
}

func TestReportRejectsNegativeRelationshipCounts(t *testing.T) {
	for _, mutate := range []func(*Report){
		func(report *Report) { report.Counts.AlbumMemberships = -1 },
		func(report *Report) { report.Counts.CheckoutEntries = -1 },
	} {
		report := testReport()
		mutate(&report)
		if err := ValidateReport(report); err == nil {
			t.Fatalf("accepted negative relationship count: %#v", report.Counts)
		}
	}
}

func TestOwnerMapTemplateSortsEntries(t *testing.T) {
	template, err := NewOwnerMapTemplate(testReport(), []MapEntry{
		{SourceHub: "hub", SourceUserID: "b", StorageKey: "key-b"},
		{SourceHub: "hub", SourceUserID: "a", StorageKey: "key-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if template.Entries[0].SourceUserID != "a" || template.Entries[0].DocbankOwnerID != "" {
		t.Fatalf("owner map was not sorted or blank: %#v", template.Entries)
	}
}

func TestWriteInventoryFiles(t *testing.T) {
	report := testReport()
	template, err := NewOwnerMapTemplate(report, []MapEntry{{SourceHub: "hub", SourceUserID: "a", StorageKey: "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "inventory")
	reportPath, ownerMapPath, err := WriteInventoryFiles(dir, report, template, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if reportPath != filepath.Join(dir, ReportFileName) || ownerMapPath != filepath.Join(dir, OwnerMapFileName) {
		t.Fatalf("unexpected output paths %q %q", reportPath, ownerMapPath)
	}
	first := map[string][]byte{}
	for _, path := range []string{reportPath, ownerMapPath} {
		privateFile, err := safefileio.OpenCurrentUserFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := safefileio.ValidatePrivateCurrentUserFile(privateFile); err != nil {
			_ = privateFile.Close()
			t.Fatalf("%s is not private: %v", path, err)
		}
		if err := privateFile.Close(); err != nil {
			t.Fatal(err)
		}
		if first[path], err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	changed := report
	changed.Counts.Owners = 9
	if _, _, err := WriteInventoryFiles(dir, changed, template); err == nil {
		t.Fatal("second write into the same directory was not refused")
	}
	for path, want := range first {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed after refused second write: %v", path, err)
		}
	}

	badDir := filepath.Join(t.TempDir(), "bad")
	badReport := report
	badReport.CreatedAt = ""
	if _, _, err := WriteInventoryFiles(badDir, badReport, template); err == nil {
		t.Fatal("accepted an invalid report")
	}
	if _, err := os.Stat(filepath.Join(badDir, OwnerMapFileName)); !os.IsNotExist(err) {
		t.Fatalf("report encode failure left an owner map: %v", err)
	}
}

func TestWriteInventoryFilesRejectsSourceAlias(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(sourceRoot, aliasRoot); err != nil {
		t.Skipf("create directory symlink: %v", err)
	}
	template, err := NewOwnerMapTemplate(testReport(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = WriteInventoryFiles(filepath.Join(aliasRoot, "nested"), testReport(), template, sourceRoot)
	if err == nil || !strings.Contains(err.Error(), "overlaps a source tree") {
		t.Fatalf("expected alias to source tree to be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "nested")); !os.IsNotExist(err) {
		t.Fatalf("source tree contains inventory output: %v", err)
	}
}
