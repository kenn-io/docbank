package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestResolvePassageReturnsExactVerifiedHistoricalWindow(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	resolution, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
	require.NoError(t, err)
	assert.Equal(t, "available", resolution.Availability)
	assert.Equal(t, "historical", resolution.Freshness)
	assert.Equal(t, "😀 evidence", resolution.Text)
	assert.Equal(t, []string{"Synthetic heading"}, resolution.SectionPath)
	require.NotNil(t, resolution.SourceLocator)
	assert.Equal(t, document.EvidenceLocatorPage, resolution.SourceLocator.Kind)
	assert.Equal(t, "/renamed/source.pdf", resolution.SourcePath)
	assert.NotEmpty(t, resolution.PassageID)
	assert.Equal(t, 1, fixture.catalog.calls)
	assert.Equal(t, 1, fixture.blobs.calls)
}

func TestResolvePassagePreservesOriginRefWhileCheckingAdoptedLocalBuild(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	originRef := fixture.ref
	originID, err := document.PassageIdentityV1(originRef)
	require.NoError(t, err)
	localBuildID := passageProcessingHash("adopted local build")
	fixture.catalog.vaultID = "99999999-9999-4999-8999-999999999999"
	fixture.catalog.authority.Build.ID = localBuildID
	fixture.blobs.payload = bytes.Replace(fixture.blobs.payload,
		[]byte(originRef.RenditionBuildID), []byte(localBuildID), 1)
	artifactHash := passageProcessingHashBytes(fixture.blobs.payload)
	fixture.catalog.authority.Artifact.BlobHash = artifactHash
	fixture.catalog.authority.Artifact.Checksum = artifactHash
	fixture.catalog.authority.Artifact.Size = int64(len(fixture.blobs.payload))
	resolution, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: originRef, MaxBytes: 64})
	require.NoError(t, err)
	assert.Equal(t, "😀 evidence", resolution.Text)
	assert.Equal(t, originRef, resolution.Ref)
	assert.Equal(t, originID, resolution.PassageID)
	for name, mutate := range map[string]func(*document.PassageRefV1){
		"origin body hash": func(ref *document.PassageRefV1) {
			ref.BodySHA256 = passageProcessingHash("wrong adopted body")
		},
		"origin quote hash": func(ref *document.PassageRefV1) {
			ref.QuoteSHA256 = passageProcessingHash("wrong adopted quote")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := originRef
			mutate(&changed)
			_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
				PassageResolveRequest{Ref: changed, MaxBytes: 64})
			require.ErrorIs(t, err, ErrPassageCorrupt)
		})
	}
}

func TestResolvePassageFailsClosedWithoutOpeningUnauthorizedOrUnavailableContent(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	foreign := fixture.ref
	foreign.VaultUID = "99999999-9999-4999-8999-999999999999"
	fixture.catalog.err = store.ErrPassageAuthorityUnavailable
	_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: foreign, MaxBytes: 64})
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Equal(t, 1, fixture.catalog.calls)
	assert.Zero(t, fixture.blobs.calls)

	fixture = newPassageResolutionFixture(t)
	fixture.catalog.err = store.ErrPassageAuthorityUnavailable
	fixture.ref.QuoteSHA256 = passageProcessingHash("tampered but undisclosed")
	_, err = resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Equal(t, 1, fixture.catalog.calls)
	assert.Zero(t, fixture.blobs.calls, "revoked or pruned authority must be rejected before content access")
}

func TestResolvePassageRejectsTamperedHashesBuildsAndBudgets(t *testing.T) {
	for name, mutate := range map[string]func(*passageResolutionFixture){
		"quote hash": func(f *passageResolutionFixture) {
			f.ref.QuoteSHA256 = passageProcessingHash("wrong quote")
		},
		"body hash": func(f *passageResolutionFixture) {
			f.ref.BodySHA256 = passageProcessingHash("wrong body")
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPassageResolutionFixture(t)
			mutate(&fixture)
			_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
				PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
			require.ErrorIs(t, err, ErrPassageCorrupt)
		})
	}
	fixture := newPassageResolutionFixture(t)
	_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: len("😀 evidence") - 1})
	require.ErrorIs(t, err, ErrPassageInvalid)
	assert.Zero(t, fixture.catalog.calls)
	assert.Zero(t, fixture.blobs.calls)
}

func TestResolvePassageRejectsBlobPayloadThatDoesNotMatchCatalogDigest(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.blobs.payload[len(fixture.blobs.payload)-2] ^= 1
	_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
	require.ErrorIs(t, err, ErrPassageCorrupt)
}

func TestResolvePassageRejectsTruncatedOrUnverifiedArtifact(t *testing.T) {
	for name, mutate := range map[string]func(*passageResolutionFixture){
		"truncated": func(f *passageResolutionFixture) {
			f.blobs.reportedSize = int64(len(f.blobs.payload))
			f.blobs.payload = f.blobs.payload[:len(f.blobs.payload)-1]
		},
		"blob verification failed": func(f *passageResolutionFixture) {
			f.blobs.verifyErr = errors.New("synthetic verification failure")
		},
		"artifact digest differs": func(f *passageResolutionFixture) {
			f.catalog.authority.Artifact.BlobHash = passageProcessingHash("other artifact")
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPassageResolutionFixture(t)
			mutate(&fixture)
			_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
				PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
			require.ErrorIs(t, err, ErrPassageCorrupt)
		})
	}
}

func TestResolvePassageAllowsEnvelopeAboveMaximumBodySize(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.authority.Artifact.Size = 64<<20 + 1
	_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
	require.ErrorIs(t, err, ErrPassageCorrupt,
		"a retained artifact above the body cap must reach blob verification")
	assert.Equal(t, 1, fixture.blobs.calls)

	fixture = newPassageResolutionFixture(t)
	fixture.catalog.authority.Artifact.Size = 1<<30 + 256<<10 + 1
	_, err = resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Zero(t, fixture.blobs.calls, "oversize authority must fail before opening content")
}

func TestResolvePassageStreamsValidArtifactBeyondLegacyCap(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	original := fixture.blobs.payload
	boundary := bytes.Index(original, []byte("\n---\n")) + len("\n---\n")
	require.Greater(t, boundary, len("\n---\n"))
	oldBody := original[boundary:]
	padLength := int64(64<<20+256<<10+1) - int64(len(original))
	pad := []byte(strings.Repeat("x", 32<<10))
	bodyHash := sha256.New()
	_, _ = bodyHash.Write(oldBody)
	for remaining := padLength; remaining > 0; {
		n := min(remaining, int64(len(pad)))
		_, _ = bodyHash.Write(pad[:n])
		remaining -= n
	}
	newBodyHash := hex.EncodeToString(bodyHash.Sum(nil))
	header := bytes.Replace(original[:boundary], []byte(fixture.ref.BodySHA256), []byte(newBodyHash), 1)
	require.Len(t, header, boundary)
	artifactHash := sha256.New()
	_, _ = artifactHash.Write(header)
	_, _ = artifactHash.Write(oldBody)
	for remaining := padLength; remaining > 0; {
		n := min(remaining, int64(len(pad)))
		_, _ = artifactHash.Write(pad[:n])
		remaining -= n
	}
	fixture.ref.BodySHA256 = newBodyHash
	fixture.catalog.authority.Artifact.BlobHash = hex.EncodeToString(artifactHash.Sum(nil))
	fixture.catalog.authority.Artifact.Size = int64(len(header)+len(oldBody)) + padLength
	blobs := &passageGeneratedBlob{header: header, prefix: oldBody, padLength: padLength}
	resolution, err := resolvePassage(t.Context(), fixture.catalog, blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
	require.NoError(t, err)
	assert.Equal(t, "😀 evidence", resolution.Text)
	assert.LessOrEqual(t, blobs.maxRead, 64<<10)
}

type passageGeneratedBlob struct {
	header, prefix []byte
	padLength      int64
	maxRead        int
}

func (blob *passageGeneratedBlob) OpenStreamContext(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
	reader := io.MultiReader(bytes.NewReader(blob.header), bytes.NewReader(blob.prefix),
		io.LimitReader(passageRepeatReader{}, blob.padLength))
	return &passageGeneratedStream{Reader: reader, blob: blob},
		int64(len(blob.header)+len(blob.prefix)) + blob.padLength, nil
}

type passageRepeatReader struct{}

func (passageRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type passageGeneratedStream struct {
	io.Reader

	blob     *passageGeneratedBlob
	verified bool
}

func (stream *passageGeneratedStream) Read(p []byte) (int, error) {
	stream.blob.maxRead = max(stream.blob.maxRead, len(p))
	n, err := stream.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		stream.verified = true
	}
	return n, err
}
func (*passageGeneratedStream) Close() error          { return nil }
func (stream *passageGeneratedStream) Verified() bool { return stream.verified }
func (*passageGeneratedStream) Verify() error         { return nil }

func TestResolvePassageDeniesRevokedMediaInputBeforeOpeningBlob(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.inputBinding = passageProcessingHash("supplied input")
	fixture.catalog.inputVisible = false
	_, err := resolvePassage(t.Context(), fixture.catalog, fixture.blobs,
		PassageResolveRequest{Ref: fixture.ref, MaxBytes: 64})
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Zero(t, fixture.blobs.calls)
}

type passageResolutionFixture struct {
	ref     document.PassageRefV1
	catalog *passageCatalogStub
	blobs   *passageBlobStub
}

func newPassageResolutionFixture(t *testing.T) passageResolutionFixture {
	t.Helper()
	body := []byte("# Synthetic heading\r\n\r\n😀 evidence and e\u0301\r\n")
	buildID := passageProcessingHash("passage build")
	sourceHash := passageProcessingHash("passage source")
	rendered, frontmatter, err := document.EnvelopeRenditionV1(document.RenditionV1{
		ContractVersion: document.RenditionContractV1, Completeness: document.EvidenceComplete,
		EvidenceChecksum: passageProcessingHash("evidence"), Markdown: body,
		MarkdownChecksum: passageProcessingHashBytes(body),
		Units: []document.NormalizedUnitV1{{EvidenceUnitID: "page:000000", Order: 0,
			Text: string(body), HeadingPath: []string{"Synthetic heading"},
			Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
	}, document.RenditionEnvelopeV1{BuildID: buildID, SourceSHA256: sourceHash,
		SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: passageProcessingHash("request"),
		EvidenceLexicalFingerprint:  passageProcessingHash("lexical"),
		NormalizedEvidenceContract:  document.NormalizedEvidenceContractV1,
		UnitKind:                    document.EvidenceUnitPage})
	require.NoError(t, err)
	start := bytes.Index(body, []byte("😀 evidence"))
	end := start + len("😀 evidence")
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     sourceHash, RenditionBuildID: buildID,
		AttachmentID: passageProcessingHash("attachment"),
	}, body, start, end)
	require.NoError(t, err)
	unit := store.RenditionUnitRecord{EvidenceUnitID: frontmatter.Navigation.Entries[0].Key,
		HeadingPath: []string{"Synthetic heading"}, Locator: document.EvidenceLocatorV1{
			Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
			Start: 1, End: 1}}
	artifactHash := passageProcessingHashBytes(rendered.Markdown)
	authority := store.PassageAuthority{Path: "/renamed/source.pdf", Fresh: false,
		Build: store.RenditionBuildRecord{ID: buildID, SourceSHA256: sourceHash,
			Units: []store.RenditionUnitRecord{unit}},
		Artifact: store.RenditionArtifactRecord{ID: "artifact", Role: "sanitized_markdown",
			BlobHash: artifactHash, Checksum: artifactHash, Size: int64(len(rendered.Markdown)),
			State: store.RenditionArtifactVerified}}
	return passageResolutionFixture{ref: ref,
		catalog: &passageCatalogStub{vaultID: ref.VaultUID, authority: authority},
		blobs:   &passageBlobStub{payload: rendered.Markdown}}
}

type passageCatalogStub struct {
	vaultID      string
	authority    store.PassageAuthority
	err          error
	calls        int
	inputBinding string
	inputVisible bool
}

func (stub *passageCatalogStub) VaultID() string { return stub.vaultID }
func (stub *passageCatalogStub) ResolvePassageAuthority(
	context.Context, document.PassageRefV1,
) (store.PassageAuthority, error) {
	stub.calls++
	return stub.authority, stub.err
}

func (stub *passageCatalogStub) RenditionInputBinding(context.Context, string) (string, error) {
	return stub.inputBinding, nil
}

func (stub *passageCatalogStub) MediaSourceBindingForContentVersion(
	context.Context, string, string,
) (string, string, error) {
	return "synthetic-source", "synthetic-version", nil
}

func (stub *passageCatalogStub) MediaInputBindingVisible(
	context.Context, string, string, string, string,
) (bool, error) {
	return stub.inputVisible, nil
}

type passageBlobStub struct {
	payload      []byte
	err          error
	calls        int
	reportedSize int64
	verifyErr    error
}

func (stub *passageBlobStub) OpenStreamContext(
	context.Context, string,
) (packstore.VerifiedReadCloser, int64, error) {
	stub.calls++
	if stub.err != nil {
		return nil, 0, stub.err
	}
	size := int64(len(stub.payload))
	if stub.reportedSize != 0 {
		size = stub.reportedSize
	}
	return &passageVerifiedReader{Reader: bytes.NewReader(stub.payload), verifyErr: stub.verifyErr}, size, nil
}

type passageVerifiedReader struct {
	*bytes.Reader

	verified  bool
	verifyErr error
}

func (reader *passageVerifiedReader) Read(value []byte) (int, error) {
	n, err := reader.Reader.Read(value)
	if errors.Is(err, io.EOF) {
		reader.verified = true
	}
	return n, err //nolint:wrapcheck // Preserve io.EOF for the io.Reader contract.
}
func (reader *passageVerifiedReader) Close() error   { return nil }
func (reader *passageVerifiedReader) Verified() bool { return reader.verified }
func (reader *passageVerifiedReader) Verify() error  { return reader.verifyErr }

func passageProcessingHash(value string) string { return passageProcessingHashBytes([]byte(value)) }
func passageProcessingHashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
