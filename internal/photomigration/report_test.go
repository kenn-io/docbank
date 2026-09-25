package photomigration

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

func TestReportCanonicalRoundTrip(t *testing.T) {
	raw, err := EncodeReport(testReport())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	other, err := EncodeReport(decoded)
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
	raw, err := EncodeReport(report)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Capacity != report.Capacity {
		t.Fatalf("retained-version capacity changed: got %#v, want %#v", decoded.Capacity, report.Capacity)
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

func TestOwnerMapTemplate(t *testing.T) {
	report := testReport()
	template, err := NewOwnerMapTemplate(report, []MapEntry{
		{SourceHub: "hub", SourceUserID: "b", StorageKey: "key-b"},
		{SourceHub: "hub", SourceUserID: "a", StorageKey: "key-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if template.Entries[0].SourceUserID != "a" || template.Entries[0].DocbankOwnerID != "" {
		t.Fatalf("owner map was not sorted or blank: %#v", template.Entries)
	}
	path := filepath.Join(t.TempDir(), "owner-map.json")
	if err := WriteOwnerMapTemplate(path, template, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("owner map is not private: %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOwnerMapTemplate(raw); err != nil {
		t.Fatal(err)
	}
	if err := WriteOwnerMapTemplate(path, template); err == nil {
		t.Fatal("owner map creation was not exclusive")
	}
}

func TestOwnerMapTemplateRejectsSourceAlias(t *testing.T) {
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
	output := filepath.Join(aliasRoot, "nested", "owner-map.json")
	err = WriteOwnerMapTemplate(output, template, sourceRoot)
	if err == nil || !strings.Contains(err.Error(), "overlaps a source tree") {
		t.Fatalf("expected alias to source tree to be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "nested", "owner-map.json")); !os.IsNotExist(err) {
		t.Fatalf("source tree contains owner-map output: %v", err)
	}
}
