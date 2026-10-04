package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

const familyDateLabel = "Document dated 2024-05-06."

type familyReportFixture struct {
	reportRenditionFixture

	root      string
	request   report.Request
	originals map[string]reportOriginal
	nodes     map[string]store.Node
}

type familyFixtureWriter struct {
	fixture *familyReportFixture
	catalog *store.Store
	blobs   *blob.Store
}

type familyPart struct{ key, filename, text string }

type familyMail struct {
	key, filename, text string
	parts               []familyPart
	reuse               []document.EmailDocumentReuse
	partial             bool
}

func newFamilyReportFixture(t *testing.T, scenario string) *familyReportFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	f := &familyReportFixture{root: root, reportRenditionFixture: newReportRenditionFixture(t),
		originals: make(map[string]reportOriginal), nodes: make(map[string]store.Node)}
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, blobs.Close()) }()
	require.NoError(t, os.Mkdir(filepath.Join(root, "spools"), 0o700))
	w := familyFixtureWriter{fixture: f, catalog: catalog, blobs: blobs}
	var selected []string
	switch scenario {
	case "selected":
		w.mail(t, familyMail{key: "P", filename: "parent.eml", text: "alpha. " + familyDateLabel,
			parts: []familyPart{
				{"C", "chosen.txt", "ordinary note. " + familyDateLabel},
				{"U", "omitted.txt", "beta. " + familyDateLabel},
			}})
		selected = []string{"P", "C"}
	case "shared":
		parts := []familyPart{{"X", "shared.txt", "alpha beta. " + familyDateLabel}}
		first := w.mail(t, familyMail{key: "Q", filename: "first.eml",
			text: "alpha. " + familyDateLabel, parts: parts})
		require.Len(t, first.Relations, 1)
		relation := first.Relations[0]
		child, err := catalog.NodeByID(t.Context(), relation.Child.NodeID)
		require.NoError(t, err)
		w.mail(t, familyMail{key: "R", filename: "second.eml",
			text: "beta. " + familyDateLabel, parts: parts,
			reuse: []document.EmailDocumentReuse{{PartPath: relation.PartPath,
				Child: *relation.Child, Revision: child.Revision}}})
		selected = []string{"Q", "R"}
	case "partial":
		w.mail(t, familyMail{key: "I", filename: "incomplete.eml",
			text: "alpha. " + familyDateLabel, partial: true})
		selected = []string{"I"}
	default:
		t.Fatalf("unknown family fixture %q", scenario)
	}
	f.request = report.Request{Version: 1, Profile: "archive", Timezone: "UTC",
		CoverageMode: "strict", SelectedDocuments: &report.SelectedDocuments{}}
	for _, key := range selected {
		f.request.SelectedDocuments.Documents = append(f.request.SelectedDocuments.Documents,
			f.identity(key))
	}
	for i, term := range []string{"alpha", "beta"} {
		f.request.Terms = append(f.request.Terms, report.Term{Number: i + 1,
			Expression: term, Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}})
	}
	return f
}

func (w familyFixtureWriter) mail(
	t *testing.T, source familyMail,
) document.EmailDocumentPublicationReceipt {
	t.Helper()
	content := "From: sender@example.test\r\nTo: reader@example.test\r\n" +
		"Subject: Synthetic message\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=m\r\n\r\n" +
		"--m\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + source.text + "\r\n"
	var attachments strings.Builder
	for _, part := range source.parts {
		attachments.WriteString("--m\r\nContent-Type: text/plain; charset=utf-8\r\n" +
			"Content-Disposition: attachment; filename=" + part.filename + "\r\n\r\n" +
			part.text + "\r\n")
	}
	content += attachments.String()
	wantState := "partial"
	if !source.partial {
		content += "--m--\r\n"
		wantState = "complete"
	}
	receipt, err := w.blobs.WriteDetailedContext(t.Context(), strings.NewReader(content))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	written, err := w.catalog.CreateFileWithReceipt(t.Context(), w.catalog.RootID(),
		source.filename, receipt.Hash, receipt.Size, "message/rfc822", store.BlobPhysical{
			Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible,
			MD5: receipt.MD5, Created: receipt.Created,
		})
	require.NoError(t, err)
	parent := written.Node
	version, err := w.catalog.ContentVersionByID(t.Context(), parent.CurrentVersionID)
	require.NoError(t, err)
	view, err := processing.EnsureEmailTarget(t.Context(), w.catalog, w.blobs,
		filepath.Join(w.fixture.root, "spools"), store.EmailTarget{Version: version})
	require.NoError(t, err)
	dir, err := w.catalog.NodeByID(t.Context(), w.catalog.RootID())
	require.NoError(t, err)
	published, err := processing.PublishEmailDocuments(t.Context(), w.catalog, w.blobs,
		document.EmailDocumentPublicationRequest{OperationID: "family-" + source.key,
			Parent: document.EmailDocumentIdentity{NodeID: parent.ID, VersionID: version.ID,
				SHA256: receipt.Hash, Size: receipt.Size},
			GenerationID: view.Generation.ID, AttachmentID: view.Attachment.ID,
			DestinationID: dir.ID, DestinationRevision: dir.Revision, Reuse: source.reuse})
	require.NoError(t, err)
	require.Equal(t, wantState, published.InventoryState)
	require.Len(t, published.Relations, len(source.parts))
	w.retain(t, source.key, parent, []byte(content), source.text)
	for i, part := range source.parts {
		relation := published.Relations[i]
		require.Equal(t, "decoded", relation.Outcome)
		require.Equal(t, parent.CurrentVersionID, relation.Parent.VersionID)
		require.NotNil(t, relation.Child)
		child, err := w.catalog.NodeByID(t.Context(), relation.Child.NodeID)
		require.NoError(t, err)
		require.Equal(t, relation.Child.VersionID, child.CurrentVersionID)
		if retained, ok := w.fixture.originals[part.key]; ok {
			require.Equal(t, retained.member.NodeID, child.ID)
			require.Equal(t, retained.member.VersionID, child.CurrentVersionID)
			require.Equal(t, retained.bytes, []byte(part.text))
			continue
		}
		w.retain(t, part.key, child, []byte(part.text), part.text)
	}
	return published
}

func (w familyFixtureWriter) retain(
	t *testing.T, key string, node store.Node, content []byte, text string,
) {
	t.Helper()
	for _, term := range []string{"alpha", "beta"} {
		require.NotContains(t, strings.ToLower(node.Name), term)
	}
	require.Equal(t, sha256HexBytes(content), node.BlobHash)
	stream, size, err := w.blobs.OpenStreamContext(t.Context(), node.BlobHash)
	require.NoError(t, err)
	actual, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, content, actual)
	require.EqualValues(t, len(content), size)
	w.fixture.originals[key] = reportOriginal{bytes: content, member: bundle.Member{
		NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: size}}
	w.fixture.nodes[key] = node
	family, unit, locator := "text", document.EvidenceUnitSection, document.EvidenceLocatorSection
	if strings.HasSuffix(node.Name, ".eml") {
		family, unit, locator = "mail", document.EvidenceUnitMessage, document.EvidenceLocatorMessage
	}
	w.fixture.publish(t, w.catalog, w.blobs, node, document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: family, UnitKind: unit, Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: text,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: locator, IndexOrigin: document.EvidenceIndexOriginNone}}},
	})
	active, err := w.catalog.ActiveRendition(t.Context(), node.CurrentVersionID, w.fixture.profile)
	require.NoError(t, err)
	require.Equal(t, node.CurrentVersionID, active.Attachment.ContentVersionID)
	require.Equal(t, w.fixture.profile, active.Head.ProcessingProfileFingerprint)
	require.Equal(t, sha256Hex("build:"+node.CurrentVersionID), active.Build.ID)
	stream, _, err = w.blobs.OpenStreamContext(t.Context(), active.Build.MarkdownChecksum)
	require.NoError(t, err)
	markdown, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	// The retained rendition escapes punctuation; compare the fixture's exact body.
	require.Equal(t, text, strings.TrimSpace(strings.ReplaceAll(string(markdown), `\`, "")))
}

func (f *familyReportFixture) identity(key string) report.Identity {
	member := f.originals[key].member
	return report.Identity{NodeID: member.NodeID, VersionID: member.VersionID, SHA256: member.SHA256}
}

func (f *familyReportFixture) start(t *testing.T) func() {
	t.Helper()
	var content bytes.Buffer
	require.NoError(t, toml.NewEncoder(&content).Encode(map[string]any{
		"rendition_profiles":  f.config.RenditionProfiles,
		"retrieval_profiles":  f.config.RetrievalProfiles,
		"processing_profiles": f.config.ProcessingProfiles,
	}))
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "config.toml"), content.Bytes(), 0o600))
	loaded, err := config.Load(f.root)
	require.NoError(t, err)
	profile, err := loaded.ProcessingProfile("archive")
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(profile.Document)
	require.NoError(t, err)
	require.Equal(t, f.profile, fingerprints.Profile)
	t.Setenv("DOCBANK_HOME", f.root)
	stop := startServe(t)
	waitForDaemon(t, f.root)
	return stop
}
