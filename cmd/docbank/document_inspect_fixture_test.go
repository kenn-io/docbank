package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

type documentInspectionFixture struct {
	root        string
	config      config.Config
	nodes       map[string]store.Node
	attachments map[string]store.RenditionAttachmentRecord
}

func newDocumentInspectionFixture(t *testing.T) *documentInspectionFixture {
	t.Helper()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	require.NoError(t, err)
	cfg := plaintextProcessingConfig(provider.Descriptor().Fingerprint)
	cfg.ProcessingProfiles["archive"] = cfg.ProcessingProfiles["private-text"]
	delete(cfg.ProcessingProfiles, "private-text")
	other := cfg.ProcessingProfiles["archive"]
	other.MaxSegmentRunes = 1800
	cfg.ProcessingProfiles["other"] = other
	portable := newReportRenditionFixture(t)
	cfg.RenditionProfiles["primary"] = portable.config.RenditionProfiles["primary"]
	cfg.RetrievalProfiles["local"] = portable.config.RetrievalProfiles["local"]
	cfg.ProcessingProfiles["portable"] = portable.config.ProcessingProfiles["archive"]
	f := &documentInspectionFixture{root: t.TempDir(), config: cfg,
		nodes: map[string]store.Node{}, attachments: map[string]store.RenditionAttachmentRecord{}}
	catalog, err := store.Open(filepath.Join(f.root, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(f.root, "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, blobs.Close()) }()
	for _, name := range []string{"notes.txt", "missing.txt", strings.Repeat("界", 256), "portable.txt"} {
		content := []byte("synthetic original for " + name)
		receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
		require.NoError(t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(t, err)
		written, err := catalog.CreateFileWithReceipt(t.Context(), catalog.RootID(), name,
			receipt.Hash, int64(len(content)), "text/plain", store.BlobPhysical{Encoding: encoding,
				StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible,
				MD5: receipt.MD5, Created: receipt.Created})
		require.NoError(t, err)
		f.nodes[name] = written.Node
		if name == "missing.txt" {
			continue
		}
		profile := "archive"
		if name == "portable.txt" {
			profile = "portable"
		}
		f.attachments[name] = f.publish(t, catalog, blobs, written.Node, profile, "aé界🙂z")
		if name == "notes.txt" {
			f.attachments["other"] = f.publish(t, catalog, blobs, written.Node, "other", "other profile text")
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, toml.NewEncoder(&encoded).Encode(map[string]any{
		"rendition_profiles": cfg.RenditionProfiles, "retrieval_profiles": cfg.RetrievalProfiles,
		"processing_profiles": cfg.ProcessingProfiles,
	}))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "config.toml"), encoded.Bytes(), 0o600))
	return f
}

func (f *documentInspectionFixture) publish(t *testing.T, catalog *store.Store,
	blobs *blob.Store, node store.Node, name, text string,
) store.RenditionAttachmentRecord {
	t.Helper()
	resolved, err := f.config.ProcessingProfile(name)
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(resolved.Document)
	require.NoError(t, err)
	publisher := reportRenditionFixture{config: f.config, name: name, profile: fingerprints.Profile}
	return publisher.publish(t, catalog, blobs, node, document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "text", UnitKind: document.EvidenceUnitSection,
		Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: text,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorSection,
				IndexOrigin: document.EvidenceIndexOriginNone}}},
	})
}

func (f *documentInspectionFixture) assertNoProcessingJobs(t *testing.T) {
	t.Helper()
	// Call only while the synthetic vault's daemon and setup store are closed.
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(f.root, "docbank.db"),
		sqlite.OpenOptions{Access: sqlite.ReadOnlyImmutable})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var jobs int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT
		(SELECT COUNT(*) FROM rendition_jobs) + (SELECT COUNT(*) FROM embedding_jobs)`).Scan(&jobs))
	require.Zero(t, jobs, "inspection must not enqueue processing")
}
