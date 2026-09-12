package emailmime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/home"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := home.CanonicalRoot(t.TempDir())
	require.NoError(t, err)
	return dir
}

type cancellingReader struct {
	cancel    context.CancelFunc
	remaining int
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.cancel()
	return n, nil
}

func TestDecodeIntegrityAndCancellationRemoveOwnedState(t *testing.T) {
	valid := sha256.Sum256([]byte("x"))
	for _, tc := range []struct {
		name string
		ctx  context.Context
		hash string
	}{{"mismatch", context.Background(), hex.EncodeToString(sha256.New().Sum(nil))}, {"cancelled", cancelledContext(t), hex.EncodeToString(valid[:])}} {
		t.Run(tc.name, func(t *testing.T) {
			parent := canonicalTempDir(t)
			_, err := Decode(tc.ctx, tc.hash, 1, strings.NewReader("x"), parent)
			require.Error(t, err)
			entries, readErr := os.ReadDir(parent)
			require.NoError(t, readErr)
			assert.Empty(t, entries)
		})
	}
}

func TestDecodeSourceIOErrorReturnsNilResultAndRemovesOwnedState(t *testing.T) {
	parent := canonicalTempDir(t)
	sentinel := errors.New("synthetic source transport failure")
	result, err := Decode(t.Context(), strings.Repeat("0", 64), 1, &dataThenErrorReader{err: sentinel}, parent)
	assert.Nil(t, result)
	require.ErrorIs(t, err, sentinel)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestRecoverStaleRemovesOnlyMarkedOwnedDirectories(t *testing.T) {
	parent := canonicalTempDir(t)
	owned := filepath.Join(parent, "docbank-email-owned")
	require.NoError(t, os.Mkdir(owned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(owned, spoolMarkerName), []byte(spoolMarker), 0o600))
	unmarked := filepath.Join(parent, "docbank-email-unmarked")
	require.NoError(t, os.Mkdir(unmarked, 0o700))
	unrelated := filepath.Join(parent, "other")
	require.NoError(t, os.Mkdir(unrelated, 0o700))
	link := filepath.Join(parent, "docbank-email-link")
	require.NoError(t, os.Symlink(unrelated, link))
	count, err := RecoverStale(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	_, err = os.Stat(owned)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, path := range []string{unmarked, unrelated, link} {
		_, err = os.Lstat(path)
		assert.NoError(t, err)
	}
}

func TestRecoverStaleBoundsMarkerBeforeRead(t *testing.T) {
	parent := canonicalTempDir(t)
	owned := filepath.Join(parent, "docbank-email-oversized-marker")
	require.NoError(t, os.Mkdir(owned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(owned, spoolMarkerName), []byte(spoolMarker+"extra"), 0o600))
	count, err := RecoverStale(t.Context(), parent)
	require.NoError(t, err)
	assert.Zero(t, count)
	_, err = os.Stat(owned)
	require.NoError(t, err)
	reader := &zeroReader{remaining: 1 << 20}
	matches, err := ownershipMarkerMatches(reader)
	require.NoError(t, err)
	assert.False(t, matches)
	assert.Equal(t, int64(len(spoolMarker)+1), reader.read)
}

func TestRecoverStaleRejectsSymlinkedRoot(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Symlink(root, link))
	_, err := RecoverStale(t.Context(), link)
	require.Error(t, err)
}

func TestDecodeLiveCancellationCleansOwnedDirectory(t *testing.T) {
	parent := canonicalTempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancellingReader{cancel: cancel, remaining: 64 << 10}
	_, err := Decode(ctx, strings.Repeat("0", 64), 64<<10, reader, parent)
	require.ErrorIs(t, err, context.Canceled)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestResultArtifactCatalogIsDetachedAndCloseIsIdempotent(t *testing.T) {
	result := decodeFixture(t, "\r\nbody")
	first := result.Artifacts()
	require.NotEmpty(t, first)
	first[0].PartPath = "9"
	assert.NotEqual(t, "9", result.Artifacts()[0].PartPath)
	require.NoError(t, result.Close())
	require.NoError(t, result.Close())
	_, err := result.OpenArtifact(t.Context(), "1", "decoded_payload")
	require.Error(t, err)
}

func TestResultRejectsArtifactSymlinkReplacement(t *testing.T) {
	result := decodeFixture(t, "\r\nbody")
	var filename string
	for _, item := range result.artifacts {
		if item.artifact.PartPath == "1" && item.artifact.Reference.Role == "decoded_payload" {
			filename = item.filename
			break
		}
	}
	require.NotEmpty(t, filename)
	require.NoError(t, result.spool.root.Rename(filename, filename+"-original"))
	require.NoError(t, result.spool.root.Symlink(filename+"-original", filename))
	_, err := result.OpenArtifact(t.Context(), "1", "decoded_payload")
	require.ErrorContains(t, err, "symlink")
}

type removalFailureReader struct {
	d     *decoder
	reads int
	t     *testing.T
}

type sourceCleanupFailureReader struct {
	spool *ownedSpool
	done  bool
	err   error
	t     *testing.T
}

func TestJoinedStorageAndPolicyErrorRemainsOperational(t *testing.T) {
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool, parts: []document.EmailPartV1{}, messages: []document.EmailMessageV1{}, artifacts: []storedArtifact{}, messageIndex: map[string]int{}}
	d.limits.PartBytes = 1
	err = d.parseEntity("1", nil, 1, 1, "1", &removalFailureReader{d: d, t: t}, &parsedHeaderBlock{raw: []byte{}, fields: []parsedHeader{}})
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
	assert.Nil(t, d.termination)
}

func TestSourceFailureKeepsJoinedCleanupErrorIdentity(t *testing.T) {
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	sentinel := errors.New("synthetic source read failure")
	err = copyVerifiedSource(t.Context(), &sourceCleanupFailureReader{spool: spool, err: sentinel, t: t}, spool, "source", strings.Repeat("0", 64), 2, 8)
	require.ErrorIs(t, err, sentinel)
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
}

func TestJoinedBodyPolicyAndStorageErrorIsOperational(t *testing.T) {
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool, artifacts: []storedArtifact{{}}}
	d.limits.BodyUTF8Bytes = 1
	var total int64
	_, err = d.storeStreamLimited("1", document.EmailArtifactBodyUTF8, &removalFailureReader{d: d, t: t}, 1, &total, 8, document.EmailDiagnosticBodyUTF8Limit, document.EmailDiagnosticBodyUTF8TotalLimit, document.EmailOperationCharset)
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
	var policy *policyLimitError
	require.ErrorAs(t, err, &policy)
	assert.True(t, hasOperationalFailure(err))
}

type dataThenErrorReader struct {
	data []byte
	err  error
}

func TestPayloadInfrastructureReadAndBodySpoolErrorsPropagate(t *testing.T) {
	sentinel := errors.New("synthetic payload read failure")
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool, artifacts: []storedArtifact{}, parts: []document.EmailPartV1{}, messages: []document.EmailMessageV1{}, messageIndex: map[string]int{}}
	err = d.parseEntity("1", nil, 1, 1, "1", &dataThenErrorReader{data: []byte("prefix"), err: sentinel}, &parsedHeaderBlock{raw: []byte{}, fields: []parsedHeader{}})
	require.ErrorIs(t, err, sentinel)
	assert.Nil(t, d.parts[0].Payload)
	assert.Len(t, d.artifacts, 1, "only the completed raw-header artifact remains")

	ref, err := d.storeBytes("1", document.EmailArtifactDecodedPayload, []byte("body"))
	require.NoError(t, err)
	name := d.artifactFilename("1", document.EmailArtifactDecodedPayload)
	require.NoError(t, spool.remove(name))
	bodyRef, state, diagnostics, err := d.deriveBody("1", "text/plain", "utf-8", name)
	require.Error(t, err)
	assert.Nil(t, bodyRef)
	assert.Equal(t, document.EmailDisplayFailed, state)
	assert.Empty(t, diagnostics)
	assert.Equal(t, document.EmailArtifactDecodedPayload, ref.Role)

	spool2, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool2.cleanup()) })
	d2 := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool2, artifacts: []storedArtifact{}}
	payload, err := d2.storeBytes("1", document.EmailArtifactDecodedPayload, []byte("body"))
	require.NoError(t, err)
	require.NoError(t, spool2.root.Mkdir("artifact-000001", 0o700))
	bodyRef, state, diagnostics, err = d2.deriveBody("1", "text/plain", "utf-8", d2.artifactFilename("1", document.EmailArtifactDecodedPayload))
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
	assert.Nil(t, bodyRef)
	assert.Equal(t, document.EmailDisplayFailed, state)
	assert.Empty(t, diagnostics)
	assert.Equal(t, document.EmailArtifactDecodedPayload, payload.Role)
}

func TestSpoolRootRejectsSymlinkedAncestor(t *testing.T) {
	owner := t.TempDir()
	actual := filepath.Join(owner, "actual")
	require.NoError(t, os.MkdirAll(filepath.Join(actual, "spools"), 0o700))
	alias := filepath.Join(owner, "alias")
	require.NoError(t, os.Symlink(actual, alias))
	_, err := createSpool(filepath.Join(alias, "spools"))
	require.Error(t, err)
	_, err = RecoverStale(t.Context(), filepath.Join(alias, "spools"))
	require.Error(t, err)
}

func (r *dataThenErrorReader) Read(value []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(value, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *removalFailureReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 2 {
		require.NoError(r.t, r.d.spool.root.Rename("artifact-000001", "saved-payload"))
		require.NoError(r.t, r.d.spool.root.Mkdir("artifact-000001", 0o700))
		require.NoError(r.t, r.d.spool.root.WriteFile("artifact-000001/child", []byte("synthetic"), 0o600))
	}
	if r.reads > 2 {
		return 0, io.EOF
	}
	p[0] = 'x'
	return 1, nil
}

func (r *sourceCleanupFailureReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	require.NoError(r.t, r.spool.root.Rename("source", "saved-source"))
	require.NoError(r.t, r.spool.root.Mkdir("source", 0o700))
	require.NoError(r.t, r.spool.root.WriteFile("source/child", []byte("synthetic"), 0o600))
	p[0] = 'x'
	return 1, nil
}
