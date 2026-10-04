package processing

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type evidenceWindowFixture struct {
	publicationFixture

	service *Service
	request EvidenceWindowRequest
	text    string
}

func newEvidenceWindowFixture(t *testing.T) evidenceWindowFixture {
	t.Helper()
	f := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(f.catalog, f.blobs)
	require.NoError(t, err)
	published, err := publisher.PublishRendition(t.Context(), f.stage(t,
		publicationIDs{"evidence-build", "evidence-attachment", "evidence-generation"}, "aé界🙂z", "Synthetic evidence"))
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(), SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
	require.NoError(t, err)
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versionID)
	require.NoError(t, err)
	window, err := service.RenditionTextWindow(t.Context(), RenditionWindowRequest{VaultUID: f.catalog.VaultID(), NodeID: version.NodeID, ContentVersionID: f.versionID, AttachmentID: published.AttachmentID})
	require.NoError(t, err)
	return evidenceWindowFixture{publicationFixture: f, service: service, text: window.Text, request: EvidenceWindowRequest{
		VaultUID: f.catalog.VaultID(), NodeID: version.NodeID, ContentVersionID: f.versionID, ContentSHA256: version.BlobHash,
		RenditionAttachmentID: published.AttachmentID, BuildID: published.BuildID, RenditionSHA256: window.Checksum}}
}

func TestEvidenceWindowExactIdentity(t *testing.T) {
	t.Parallel()
	f := newEvidenceWindowFixture(t)
	got, err := f.service.ReadEvidenceWindow(t.Context(), f.request)
	require.NoError(t, err)
	assert.Equal(t, f.text, got.Text)
	assert.Contains(t, got.Text, "aé界🙂z")
	assert.Equal(t, len(f.text), got.ResponseBytes)
	assert.Equal(t, f.request.ContentSHA256, got.ContentSHA256)
	for name, change := range map[string]func(*EvidenceWindowRequest){
		"vault":          func(r *EvidenceWindowRequest) { r.VaultUID = "11111111-1111-4111-8111-111111111111" },
		"node":           func(r *EvidenceWindowRequest) { r.NodeID++ },
		"version":        func(r *EvidenceWindowRequest) { r.ContentVersionID = "22222222-2222-4222-8222-222222222222" },
		"content hash":   func(r *EvidenceWindowRequest) { r.ContentSHA256 = strings.Repeat("a", 64) },
		"attachment":     func(r *EvidenceWindowRequest) { r.RenditionAttachmentID = strings.Repeat("a", 64) },
		"build":          func(r *EvidenceWindowRequest) { r.BuildID = strings.Repeat("a", 64) },
		"rendition hash": func(r *EvidenceWindowRequest) { r.RenditionSHA256 = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			r := f.request
			change(&r)
			got, err := f.service.ReadEvidenceWindow(t.Context(), r)
			require.ErrorIs(t, err, ErrEvidenceUnavailable)
			assert.Empty(t, got.Text)
		})
	}
}

func TestEvidenceWindowAuthority(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"rename", "content replacement", "rendition replacement", "trash", "retirement", "pruning"} {
		t.Run(kind, func(t *testing.T) {
			f := newEvidenceWindowFixture(t)
			node, err := f.catalog.NodeByID(t.Context(), f.request.NodeID)
			require.NoError(t, err)
			switch kind {
			case "rename":
				_, _, err = f.catalog.Move(t.Context(), node.ID, f.catalog.RootID(), "renamed.pdf", node.Revision)
			case "content replacement", "pruning":
				_, _, err = f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision, node.BlobHash, node.Size, node.MimeType)
			case "rendition replacement":
				publisher, e := NewArtifactPublisher(f.catalog, f.blobs)
				require.NoError(t, e)
				_, err = publisher.PublishRendition(t.Context(), f.stage(t, publicationIDs{"replacement-build", "replacement-attachment", "replacement-generation"}, "new text", "Replacement evidence"))
			case "retirement":
				_, err = f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{AttachmentIDs: []string{f.request.RenditionAttachmentID}})
			case "trash":
				_, _, err = f.catalog.Trash(t.Context(), node.ID, store.UnconditionalRev)
			}
			require.NoError(t, err)
			if kind == "pruning" {
				_, err = f.catalog.PruneContentVersions(t.Context(), node.ID, store.UnconditionalRev, store.VersionPruneSelector{VersionIDs: []string{f.request.ContentVersionID}}, true)
				require.NoError(t, err)
			}
			got, err := f.service.ReadEvidenceWindow(t.Context(), f.request)
			if kind == "rename" {
				require.NoError(t, err)
				assert.Equal(t, f.text, got.Text)
			} else {
				require.ErrorIs(t, err, ErrEvidenceUnavailable)
				assert.Empty(t, got.Text)
			}
		})
	}
}

func TestEvidenceWindowBoundsAndCancellation(t *testing.T) {
	t.Parallel()
	f := newEvidenceWindowFixture(t)
	for _, offset := range []int{0, 1, len([]rune(f.text)), len([]rune(f.text)) + 1} {
		r := f.request
		r.Offset = offset
		r.MaxChars = 3
		got, err := f.service.ReadEvidenceWindow(t.Context(), r)
		if offset > len([]rune(f.text)) {
			require.ErrorIs(t, err, ErrInvalidRenditionWindow)
			assert.Empty(t, got.Text)
			continue
		}
		require.NoError(t, err)
		end := min(offset+3, len([]rune(f.text)))
		assert.Equal(t, string([]rune(f.text)[offset:end]), got.Text)
		assert.Equal(t, end, got.NextOffset)
		assert.Equal(t, end == len([]rune(f.text)), got.EOF)
	}
	r := f.request
	r.MaxChars = 16000
	_, err := f.service.ReadEvidenceWindow(t.Context(), r)
	require.NoError(t, err)
	r.MaxChars = 16001
	_, err = f.service.ReadEvidenceWindow(t.Context(), r)
	require.ErrorIs(t, err, ErrInvalidEvidenceRequest)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := f.service.ReadEvidenceWindow(ctx, f.request)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, got.Text)
}

func FuzzEvidenceWindowRequestBounds(f *testing.F) {
	f.Add(0, 0)
	f.Add(-1, 1)
	f.Add(1, 16000)
	f.Add(0, 16001)
	f.Fuzz(func(t *testing.T, offset, maxChars int) {
		r := EvidenceWindowRequest{VaultUID: "11111111-1111-4111-8111-111111111111", NodeID: 1, ContentVersionID: "22222222-2222-4222-8222-222222222222", ContentSHA256: strings.Repeat("a", 64), BuildID: strings.Repeat("b", 64), RenditionAttachmentID: strings.Repeat("c", 64), RenditionSHA256: strings.Repeat("d", 64), Offset: offset, MaxChars: maxChars}
		err := validateEvidenceWindowRequest(r)
		if offset < 0 || maxChars < 0 || maxChars > 16000 {
			assert.ErrorIs(t, err, ErrInvalidEvidenceRequest)
		} else {
			assert.NoError(t, err)
		}
	})
}

func TestEvidenceWindowRejectsMalformedReferences(t *testing.T) {
	t.Parallel()
	f := newEvidenceWindowFixture(t)
	for name, change := range map[string]func(*EvidenceWindowRequest){
		"missing vault":      func(r *EvidenceWindowRequest) { r.VaultUID = "" },
		"malformed version":  func(r *EvidenceWindowRequest) { r.ContentVersionID = "bad" },
		"negative node":      func(r *EvidenceWindowRequest) { r.NodeID = -1 },
		"missing hash":       func(r *EvidenceWindowRequest) { r.ContentSHA256 = "" },
		"uppercase hash":     func(r *EvidenceWindowRequest) { r.RenditionSHA256 = strings.Repeat("A", 64) },
		"invalid attachment": func(r *EvidenceWindowRequest) { r.RenditionAttachmentID = "bad" },
		"invalid build":      func(r *EvidenceWindowRequest) { r.BuildID = "bad" },
		"negative offset":    func(r *EvidenceWindowRequest) { r.Offset = -1 },
		"negative limit":     func(r *EvidenceWindowRequest) { r.MaxChars = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := f.request
			change(&r)
			got, err := f.service.ReadEvidenceWindow(t.Context(), r)
			require.ErrorIs(t, err, ErrInvalidEvidenceRequest)
			assert.Empty(t, got.Text)
		})
	}
}

func TestEvidenceWindowUnavailablePhysicalBytes(t *testing.T) {
	t.Parallel()
	f := newEvidenceWindowFixture(t)
	require.NoError(t, f.blobs.Remove(f.request.RenditionSHA256))
	got, err := f.service.ReadEvidenceWindow(t.Context(), f.request)
	require.ErrorIs(t, err, packstore.ErrPhysicalMissing)
	assert.Empty(t, got.Text)
}
