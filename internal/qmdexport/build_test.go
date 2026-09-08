package qmdexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/packstore"
)

var (
	errSyntheticOpen   = errors.New("synthetic open failure")
	errSyntheticRead   = errors.New("synthetic read failure")
	errSyntheticVerify = errors.New("synthetic verify failure")
	errSyntheticClose  = errors.New("synthetic close failure")
)

func TestBuildProducesLiteralDeterministicGeneration(t *testing.T) {
	// Mutation caught: retaining the claimed envelope, changing its body, or
	// deriving URI identity from caller order changes these literal results.
	markdown := []byte("alpha\n\n---\n\nbeta\n")
	source := syntheticSource(7, "00000000-0000-4000-8000-000000000007", markdown)
	reader := syntheticReader{source.BlobSHA256: markdown}

	generation, err := Build(t.Context(), "synthetic", []Source{source}, reader, Options{})
	require.NoError(t, err)
	require.Len(t, generation.Manifest.Entries, 1)
	entry := generation.Manifest.Entries[0]
	require.Equal(t, "qmd://synthetic/"+entry.RelativePath, entry.URI)
	require.Equal(t, markdown, generation.documents[entry.RelativePath])
	require.Equal(t, syntheticDigest(markdown), entry.ExportedMarkdownSHA256)
	require.Equal(t, "25e2070176afa8762f95c2c5826fea61dd4cba1b3c3977e813b8c393efc5d748", entry.RelativePath[13:77])
	require.Equal(t, "7db1c88d5f885d4a3ee497ad390bb47147ddd8d880744ac5f71198d468caefa3", generation.ID)
}

func TestBuildPreservesDistinctSameNodeSourceIdentityAcrossPermutation(t *testing.T) {
	body := []byte("same retained markdown\n")
	first := syntheticSource(7, "00000000-0000-4000-8000-000000000007", body)
	second := first
	second.ProcessingProfileFingerprint = strings.Repeat("c", 64)
	second.AttachmentID = "attachment-second-profile"
	reader := syntheticReader{first.BlobSHA256: body}
	forward, err := Build(t.Context(), "synthetic", []Source{first, second}, reader, Options{})
	require.NoError(t, err)
	reversed, err := Build(t.Context(), "synthetic", []Source{second, first}, reader, Options{})
	require.NoError(t, err)
	require.Equal(t, forward.ID, reversed.ID)
	require.Len(t, forward.Manifest.Entries, 2)
	require.Len(t, reversed.Manifest.Entries, 2)
	require.Equal(t, forward.Manifest.Entries, reversed.Manifest.Entries)
	require.NotEqual(t, forward.Manifest.Entries[0].URI, forward.Manifest.Entries[1].URI)
	require.Equal(t, forward.Manifest.Entries[0].NodeID, forward.Manifest.Entries[1].NodeID)
}

func TestBuildRejectsAllInvalidInputBeforeOpeningSources(t *testing.T) {
	// Mutation caught: sorting or opening before validating the complete owned
	// snapshot lets malformed identities reach the blob store.
	valid := syntheticSource(1, "00000000-0000-4000-8000-000000000001", []byte("one"))
	invalid := syntheticSource(2, "00000000-0000-4000-8000-000000000002", []byte("two"))
	invalid.ArtifactID = "bad\x00identity"
	reader := &countingSyntheticReader{contents: syntheticReader{valid.BlobSHA256: []byte("one")}}

	tests := []struct {
		name       string
		ctx        context.Context
		collection string
		sources    []Source
		options    Options
	}{
		{name: "nil context", collection: "synthetic"},
		{name: "canceled context", ctx: canceledContext(), collection: "synthetic"},
		{name: "collection", ctx: t.Context(), collection: "../synthetic"},
		{name: "options", ctx: t.Context(), collection: "synthetic", options: Options{MaxDocuments: -1}},
		{name: "membership", ctx: t.Context(), collection: "synthetic", sources: []Source{valid, valid}, options: Options{MaxDocuments: 1}},
		{name: "later invalid identity", ctx: t.Context(), collection: "synthetic", sources: []Source{valid, invalid}},
		{name: "aggregate bytes", ctx: t.Context(), collection: "synthetic", sources: []Source{valid}, options: Options{MaxTotalBytes: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader.opens = 0
			_, err := Build(test.ctx, test.collection, test.sources, reader, test.options)
			require.Error(t, err)
			require.Zero(t, reader.opens)
		})
	}
	_, err := Build(t.Context(), "synthetic", nil, nil, Options{})
	require.Error(t, err)
}

func TestBuildPreservesDonorEmptyGenerationRepresentation(t *testing.T) {
	// Mutation caught: forcing a non-nil public entry slice changes the donor
	// value representation even though canonical JSON remains an empty array.
	generation, err := Build(t.Context(), "synthetic", nil, syntheticReader{}, Options{})
	require.NoError(t, err)
	require.Nil(t, generation.Manifest.Entries)
	encoded, err := encodeManifest(generation.Manifest, maxManifestBytes)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"entries":[]`)
}

func TestBuildRejectsDuplicateAndNonFiniteSourceIdentities(t *testing.T) {
	// Mutation caught: hashing before finite-string validation can admit NUL,
	// invalid UTF-8, or duplicate opaque identities into path authority.
	body := []byte("body")
	valid := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
	for name, mutate := range map[string]func(*Source){
		"NUL":           func(source *Source) { source.ArtifactID = "artifact\x00markdown" },
		"invalid UTF-8": func(source *Source) { source.VaultUID = string([]byte{0xff}) },
		"oversized":     func(source *Source) { source.AttachmentID = strings.Repeat("x", 1025) },
	} {
		t.Run(name, func(t *testing.T) {
			source := valid
			mutate(&source)
			reader := &countingSyntheticReader{contents: syntheticReader{valid.BlobSHA256: body}}
			_, err := Build(t.Context(), "synthetic", []Source{source}, reader, Options{})
			require.Error(t, err)
			require.Zero(t, reader.opens)
		})
	}
	reader := &countingSyntheticReader{contents: syntheticReader{valid.BlobSHA256: body}}
	_, err := Build(t.Context(), "synthetic", []Source{valid, valid}, reader, Options{})
	require.ErrorContains(t, err, "duplicate")
	require.Zero(t, reader.opens)
}

func TestBuildUsesOwnedSourceSnapshotAcrossReaderMutation(t *testing.T) {
	// Mutation caught: retaining the caller slice would let a callback replace
	// a later source's requested hash and manifest authority mid-build.
	firstBody, secondBody := []byte("first\n"), []byte("second\n")
	first := syntheticSource(1, "00000000-0000-4000-8000-000000000001", firstBody)
	second := syntheticSource(2, "00000000-0000-4000-8000-000000000002", secondBody)
	sources := []Source{first, second}
	reader := &mutatingSyntheticReader{contents: syntheticReader{
		first.BlobSHA256: firstBody, second.BlobSHA256: secondBody,
	}, mutate: func() { sources[1] = first }}

	generation, err := Build(t.Context(), "synthetic", sources, reader, Options{})
	require.NoError(t, err)
	require.Len(t, generation.Manifest.Entries, 2)
	require.ElementsMatch(t, []string{first.BlobSHA256, second.BlobSHA256}, reader.hashes)
	require.ElementsMatch(t, []int64{first.NodeID, second.NodeID}, []int64{
		generation.Manifest.Entries[0].NodeID, generation.Manifest.Entries[1].NodeID,
	})
}

func TestBuildReturnsNoGenerationForSourceFailures(t *testing.T) {
	// Mutation caught: losing an underlying stream error or returning partial
	// documents after a managed-read failure violates fail-closed generation.
	body := []byte("synthetic body")
	source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
	tests := []struct {
		name   string
		stream *faultStream
		size   int64
		open   error
		want   error
	}{
		{name: "open", open: errSyntheticOpen, want: errSyntheticOpen},
		{name: "reported size", stream: newFaultStream(body), size: int64(len(body) + 1)},
		{name: "short bytes", stream: newFaultStream(body[:len(body)-1]), size: int64(len(body))},
		{name: "extra bytes", stream: newFaultStream(append(append([]byte(nil), body...), 'x')), size: int64(len(body))},
		{name: "read", stream: &faultStream{readErr: errSyntheticRead}, size: int64(len(body)), want: errSyntheticRead},
		{name: "verify", stream: &faultStream{Reader: bytes.NewReader(body), verifyErr: errSyntheticVerify}, size: int64(len(body)), want: errSyntheticVerify},
		{name: "close", stream: &faultStream{Reader: bytes.NewReader(body), closeErr: errSyntheticClose}, size: int64(len(body)), want: errSyntheticClose},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &faultBlobReader{stream: test.stream, size: test.size, err: test.open}
			generation, err := Build(t.Context(), "synthetic", []Source{source}, reader, Options{})
			require.Error(t, err)
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
			}
			require.Zero(t, generation)
			if test.stream != nil {
				require.Equal(t, 1, test.stream.closes)
			}
		})
	}
	t.Run("joined read and close", func(t *testing.T) {
		stream := &faultStream{readErr: errSyntheticRead, closeErr: errSyntheticClose}
		generation, err := Build(t.Context(), "synthetic", []Source{source},
			&faultBlobReader{stream: stream, size: int64(len(body))}, Options{})
		require.ErrorIs(t, err, errSyntheticRead)
		require.ErrorIs(t, err, errSyntheticClose)
		require.Zero(t, generation)
	})
}

func TestBuildRejectsChecksumUTF8AndLateCancellation(t *testing.T) {
	// Mutation caught: omitting any post-open integrity/cancellation check can
	// return bytes that are corrupt or no longer authorized by the caller.
	body := []byte("synthetic body")
	source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
	wrong := source
	wrong.BlobSHA256 = strings.Repeat("c", 64)
	wrong.MarkdownChecksum = wrong.BlobSHA256
	wrong.ArtifactChecksum = wrong.BlobSHA256
	invalidUTF8 := []byte{0xff, 0xfe}
	invalidSource := syntheticSource(1, source.ContentVersionID, invalidUTF8)

	_, err := Build(t.Context(), "synthetic", []Source{wrong}, syntheticReader{wrong.BlobSHA256: body}, Options{})
	require.ErrorContains(t, err, "checksum")
	_, err = Build(t.Context(), "synthetic", []Source{invalidSource}, syntheticReader{invalidSource.BlobSHA256: invalidUTF8}, Options{})
	require.ErrorContains(t, err, "UTF-8")

	ctx, cancel := context.WithCancel(t.Context())
	stream := newFaultStream(body)
	stream.afterVerify = cancel
	generation, err := Build(ctx, "synthetic", []Source{source}, &faultBlobReader{stream: stream, size: int64(len(body))}, Options{})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, generation)
}

func TestBuildRejectsCancellationAtEveryManagedReadBoundary(t *testing.T) {
	// Mutation caught: checking cancellation only before opening allows a late
	// cancel after Open, Read, Verify, or Close to publish a generation.
	body := []byte("synthetic body")
	source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
	for _, phase := range []string{"open", "read", "verify", "close"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream := newFaultStream(body)
			reader := &faultBlobReader{stream: stream, size: int64(len(body))}
			switch phase {
			case "open":
				reader.afterOpen = cancel
			case "read":
				stream.afterRead = cancel
			case "verify":
				stream.afterVerify = cancel
			case "close":
				stream.afterClose = cancel
			}
			generation, err := Build(ctx, "synthetic", []Source{source}, reader, Options{})
			require.ErrorIs(t, err, context.Canceled)
			require.Zero(t, generation)
			require.Equal(t, 1, stream.closes)
		})
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type syntheticReader map[string][]byte

func (reader syntheticReader) OpenStreamContext(
	_ context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	content := reader[hash]
	return &syntheticVerifiedReader{Reader: bytes.NewReader(content)}, int64(len(content)), nil
}

type countingSyntheticReader struct {
	contents syntheticReader
	opens    int
}

func (reader *countingSyntheticReader) OpenStreamContext(
	ctx context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	reader.opens++
	return reader.contents.OpenStreamContext(ctx, hash)
}

type mutatingSyntheticReader struct {
	contents syntheticReader
	mutate   func()
	hashes   []string
}

func (reader *mutatingSyntheticReader) OpenStreamContext(
	ctx context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	reader.hashes = append(reader.hashes, hash)
	if len(reader.hashes) == 1 {
		reader.mutate()
	}
	return reader.contents.OpenStreamContext(ctx, hash)
}

type faultBlobReader struct {
	stream    *faultStream
	size      int64
	err       error
	afterOpen func()
}

func (reader *faultBlobReader) OpenStreamContext(
	context.Context, string,
) (packstore.VerifiedReadCloser, int64, error) {
	if reader.afterOpen != nil {
		reader.afterOpen()
	}
	return reader.stream, reader.size, reader.err
}

type faultStream struct {
	*bytes.Reader

	readErr     error
	verifyErr   error
	closeErr    error
	afterRead   func()
	afterVerify func()
	afterClose  func()
	closes      int
}

func newFaultStream(content []byte) *faultStream {
	return &faultStream{Reader: bytes.NewReader(content)}
}

func (reader *faultStream) Read(p []byte) (int, error) {
	if reader.readErr != nil {
		return 0, reader.readErr
	}
	if reader.Len() == 0 {
		return 0, io.EOF
	}
	n, _ := reader.Reader.Read(p)
	if reader.afterRead != nil {
		reader.afterRead()
		reader.afterRead = nil
	}
	return n, nil
}

func (reader *faultStream) Close() error {
	reader.closes++
	if reader.afterClose != nil {
		reader.afterClose()
	}
	return reader.closeErr
}

func (reader *faultStream) Verify() error {
	if reader.afterVerify != nil {
		reader.afterVerify()
	}
	return reader.verifyErr
}

func (reader *faultStream) Verified() bool { return reader.verifyErr == nil }

type syntheticVerifiedReader struct {
	*bytes.Reader

	verified bool
}

func (reader *syntheticVerifiedReader) Close() error   { return nil }
func (reader *syntheticVerifiedReader) Verify() error  { reader.verified = true; return nil }
func (reader *syntheticVerifiedReader) Verified() bool { return reader.verified }

func syntheticSource(nodeID int64, versionID string, markdown []byte) Source {
	checksum := syntheticDigest(markdown)
	return Source{
		VaultUID: "00000000-0000-4000-8000-000000000001", NodeID: nodeID,
		ContentVersionID: versionID, ProcessingProfileFingerprint: strings.Repeat("a", 64),
		AttachmentID: "attachment-" + versionID, BuildID: strings.Repeat("b", 64),
		ArtifactID: "artifact-markdown", BlobSHA256: checksum, BlobSize: int64(len(markdown)),
		ArtifactChecksum: checksum, MarkdownChecksum: checksum,
	}
}

func syntheticDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

var _ io.ReadCloser = (*faultStream)(nil)
