package photomigration

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testReport() Report {
	return Report{
		Source:    Source{Kind: SourceInstall, Identity: "catalog-sha256"},
		Schema:    Schema{CatalogVersion: 1, CatalogFingerprint: "fingerprint", EmbeddedDocbankVersion: 16},
		Counts:    Counts{Owners: 1, Assets: 2, Files: 3, Bytes: 10, Albums: 1, Shares: 1, Checkouts: 1, AIResults: 1, HiddenSetup: 1},
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
