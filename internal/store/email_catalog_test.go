package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/emailmime"
	"go.kenn.io/kit/packstore"
)

const catalogEmailSource = "From: Sender <sender@example.test>\r\nSubject: Synthetic email\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=example\r\n\r\n--example\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nChosen synthetic body.\r\n--example\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=sample.bin\r\n\r\nAAECAw==\r\n--example--\r\n"

type emailFixture struct {
	s           *Store
	blobs       *packstore.LooseStore
	publication EmailPublication
	bytes       map[string][]byte
}

func newEmailFixture(t *testing.T, s *Store, name string) emailFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	layout, err := packstore.NewLayout(filepath.Join(root, "blobs"), packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	bs, err := packstore.NewLooseStore(layout)
	require.NoError(t, err)
	f := emailFixture{s: s, blobs: bs, bytes: make(map[string][]byte)}
	source := []byte(catalogEmailSource)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	decoded, err := emailmime.Decode(t.Context(), hash, int64(len(source)), bytes.NewReader(source), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, decoded.Close()) }()
	require.NoError(t, func() error {
		receipt, e := bs.Write(t.Context(), bytes.NewReader(source), packstore.WriteOptions{Durability: packstore.DurablePublication, Dedup: packstore.VerifyFullHash, MaxBytes: 128 << 20})
		if e != nil {
			return fmt.Errorf("staging synthetic email: %w", e)
		}
		enc := "raw"
		node, e := s.CreateFile(t.Context(), s.RootID(), name, hash, int64(len(source)), "message/rfc822", BlobPhysical{Encoding: enc, StoredBytes: receipt.StoredSize, Created: receipt.Created, PackEligible: true})
		if e != nil {
			return fmt.Errorf("staging synthetic email: %w", e)
		}
		f.publication.ContentVersionID = node.CurrentVersionID
		for _, a := range decoded.Artifacts() {
			r, e := decoded.OpenArtifact(t.Context(), a.PartPath, string(a.Reference.Role))
			if e != nil {
				return fmt.Errorf("staging synthetic email: %w", e)
			}
			b, e := io.ReadAll(r)
			closeErr := r.Close()
			if e != nil {
				return fmt.Errorf("staging synthetic email: %w", e)
			}
			if closeErr != nil {
				return closeErr
			}
			wr, e := bs.Write(t.Context(), bytes.NewReader(b), packstore.WriteOptions{Durability: packstore.DurablePublication, Dedup: packstore.VerifyFullHash, MaxBytes: 128 << 20})
			if e != nil {
				return fmt.Errorf("staging synthetic email: %w", e)
			}
			require.Equal(t, a.Reference.SHA256, string(wr.Hash))
			require.Equal(t, a.Reference.Size, wr.Size)
			enc := "raw"
			if e = s.RecordRenditionBlob(t.Context(), string(wr.Hash), wr.Size, BlobPhysical{Encoding: enc, StoredBytes: wr.StoredSize, Created: wr.Created, PackEligible: true}); e != nil {
				return fmt.Errorf("staging synthetic email: %w", e)
			}
			f.publication.Artifacts = append(f.publication.Artifacts, EmailPartArtifactRecord{PartPath: a.PartPath, Role: string(a.Reference.Role), BlobSHA256: string(wr.Hash), Size: wr.Size})
			f.bytes[string(wr.Hash)] = b
		}
		return nil
	}())
	f.publication.CanonicalJSON, _, err = document.MarshalEmailV1(decoded.Evidence)
	require.NoError(t, err)
	return f
}

func emailTableCounts(t *testing.T, s *Store) []int {
	t.Helper()
	var result []int
	for _, table := range []string{"email_generations", "email_part_artifacts", "email_attachments", "email_heads", "email_body_results", "rendition_blob_staging"} {
		var n int
		require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
		result = append(result, n)
	}
	return result
}

func TestEmailPublicationSharesGenerationAndRetiresStaging(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "first.eml")
	first, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	require.Equal(t, "pending", first.BodySearch.State)
	for hash := range f.bytes {
		var n int
		require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM rendition_blob_staging WHERE blob_hash=?", hash).Scan(&n))
		require.Zero(t, n)
	}
	node, err := s.CreateFile(t.Context(), s.RootID(), "second.eml", first.Version.BlobHash, first.Version.Size, "message/rfc822")
	require.NoError(t, err)
	p := f.publication
	p.ContentVersionID = node.CurrentVersionID
	second, err := s.PublishEmailGeneration(t.Context(), p)
	require.NoError(t, err)
	require.Equal(t, first.Generation.ID, second.Generation.ID)
	require.NotEqual(t, first.Attachment.ID, second.Attachment.ID)
	require.Equal(t, first.Version.ID, first.Attachment.ContentVersionID)
	require.Equal(t, second.Version.ID, second.Attachment.ContentVersionID)
	again, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	require.Equal(t, first.Attachment.AttachedAt, again.PublishedAt)
	unrelated, err := s.CreateFile(t.Context(), s.RootID(), "other.eml", fakeHash("a4"), 4, "message/rfc822")
	require.NoError(t, err)
	_, err = s.EmailMetadataGeneration(t.Context(), unrelated.CurrentVersionID, first.Generation.ID)
	require.ErrorIs(t, err, ErrNotFound)
	for _, a := range f.publication.Artifacts {
		r, err := s.EmailPart(t.Context(), first.Version.ID, first.Generation.ID, a.PartPath, a.Role)
		require.NoError(t, err)
		require.Equal(t, a.BlobSHA256, r.BlobSHA256)
		if a.PartPath == "1.2" {
			require.Equal(t, "sample.bin", r.Filename)
		}
	}
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestEmailPublicationRejectsInvalidAuthorityAtomically(t *testing.T) {
	for _, tc := range []string{"canonical", "omitted", "extra", "role", "size", "source", "missing_blob"} {
		t.Run(tc, func(t *testing.T) {
			s := newTestStore(t)
			f := newEmailFixture(t, s, "source.eml")
			first, err := s.PublishEmailGeneration(t.Context(), f.publication)
			require.NoError(t, err)
			p := f.publication
			p.Artifacts = slices.Clone(p.Artifacts)
			p.CanonicalJSON = bytes.Clone(p.CanonicalJSON)
			switch tc {
			case "canonical":
				p.CanonicalJSON = append(p.CanonicalJSON, ' ')
			case "omitted":
				p.Artifacts = p.Artifacts[1:]
			case "extra":
				p.Artifacts = append(p.Artifacts, p.Artifacts[0])
			case "role":
				p.Artifacts[0].Role = "unknown"
			case "size":
				p.Artifacts[0].Size++
			case "source":
				n, e := s.CreateFile(t.Context(), s.RootID(), "wrong.eml", fakeHash("a5"), 5, "message/rfc822")
				require.NoError(t, e)
				p.ContentVersionID = n.CurrentVersionID
			case "missing_blob":
				_, err = s.db.Exec("PRAGMA foreign_keys=OFF")
				require.NoError(t, err)
				_, err = s.db.Exec("DELETE FROM blobs WHERE hash=?", p.Artifacts[0].BlobSHA256)
				require.NoError(t, err)
				_, err = s.db.Exec("PRAGMA foreign_keys=ON")
				require.NoError(t, err)
			}
			before := emailTableCounts(t, s)
			_, err = s.PublishEmailGeneration(t.Context(), p)
			require.Error(t, err)
			require.Equal(t, before, emailTableCounts(t, s))
			var selected string
			require.NoError(t, s.db.QueryRow("SELECT attachment_id FROM email_heads WHERE content_version_id=?", first.Version.ID).Scan(&selected))
			require.Equal(t, first.Attachment.ID, selected)
		})
	}
}

func TestEmailPublicationRollbackPreservesNewStagingAndOldHead(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	first, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	changed := first.Evidence
	changed.Recipe.GoVersion = "go1.27.1"
	p := f.publication
	p.CanonicalJSON, _, err = document.MarshalEmailV1(changed)
	require.NoError(t, err)
	for _, a := range p.Artifacts {
		require.NoError(t, s.RecordRenditionBlob(t.Context(), a.BlobSHA256, a.Size, BlobPhysical{Encoding: "raw", StoredBytes: a.Size, PackEligible: true, Created: true}))
	}
	before := emailTableCounts(t, s)
	_, err = s.db.Exec(`CREATE TRIGGER fail_email_head BEFORE UPDATE ON email_heads BEGIN SELECT RAISE(ABORT,'synthetic email head failure'); END`)
	require.NoError(t, err)
	_, err = s.PublishEmailGeneration(t.Context(), p)
	require.ErrorContains(t, err, "synthetic email head failure")
	require.Equal(t, before, emailTableCounts(t, s))
	got, err := s.EmailMetadata(t.Context(), first.Version.ID)
	require.NoError(t, err)
	require.Equal(t, first.Generation.ID, got.Generation.ID)
	_, err = s.db.Exec(`DROP TRIGGER fail_email_head`)
	require.NoError(t, err)
	_, err = s.PublishEmailGeneration(t.Context(), p)
	require.NoError(t, err)
	require.Zero(t, emailTableCounts(t, s)[5])
}

func TestEmailMissingStatesAndDeterministicRefusal(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "pending.eml")
	v, err := s.EmailMetadata(t.Context(), f.publication.ContentVersionID)
	require.ErrorIs(t, err, ErrEmailPending)
	require.Equal(t, f.publication.ContentVersionID, v.Version.ID)
	n, err := s.CreateFile(t.Context(), s.RootID(), "unsupported.eml", fakeHash("a6"), 6, "application/octet-stream")
	require.NoError(t, err)
	v, err = s.EmailMetadata(t.Context(), n.CurrentVersionID)
	require.ErrorIs(t, err, ErrEmailNotSupported)
	require.Equal(t, n.CurrentVersionID, v.Version.ID)
	v, err = s.EmailMetadata(t.Context(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, v.Version.ID)
	n, err = s.CreateFile(t.Context(), s.RootID(), "oversize.eml", fakeHash("a7"), 128<<20+1, "message/rfc822")
	require.NoError(t, err)
	decoded, err := emailmime.Decode(t.Context(), n.BlobHash, n.Size, bytes.NewReader(nil), t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, decoded.Close()) }()
	encoded, _, err := document.MarshalEmailV1(decoded.Evidence)
	require.NoError(t, err)
	v, err = s.PublishEmailGeneration(t.Context(), EmailPublication{ContentVersionID: n.CurrentVersionID, CanonicalJSON: encoded})
	require.NoError(t, err)
	require.Empty(t, v.Generation.Artifacts)
	require.Equal(t, document.EmailVerification("catalog_only"), v.Evidence.Source.Verification)
	_, err = s.EmailPart(t.Context(), n.CurrentVersionID, v.Generation.ID, "1", "raw_headers")
	require.ErrorIs(t, err, ErrEmailPartUnavailable)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}
