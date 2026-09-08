package geminiembed

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestDirectUploadOwnershipRejectsUnsafeInputsAndClosesEverySourceOnce(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "owned.png", "image/png")
	client, secrets, requests := noEgressGeminiClient(t, profile)
	authorization := directGeminiAuthorization(profile, 3)

	t.Run("repeated source before trailing source", func(t *testing.T) {
		repeated := newProofUpload(data, metadata, proof)
		trailing := newProofUpload(data, metadata, proof)
		_, err := client.Embed(t.Context(), directGeminiInputs(repeated, repeated, trailing), authorization)
		require.ErrorContains(t, err, "repeated")
		assert.Equal(t, int32(1), repeated.closeCalls.Load())
		assert.Equal(t, int32(1), trailing.closeCalls.Load())
		assert.Zero(t, repeated.metadataCalls.Load())
		assert.Zero(t, trailing.metadataCalls.Load())
	})

	t.Run("nil source before trailing source", func(t *testing.T) {
		trailing := newProofUpload(data, metadata, proof)
		_, err := client.Embed(t.Context(), directGeminiInputs(nil, trailing), authorization)
		require.ErrorContains(t, err, "source is nil")
		assert.Equal(t, int32(1), trailing.closeCalls.Load())
		assert.Zero(t, trailing.metadataCalls.Load())
	})

	t.Run("unsafe source before trailing source", func(t *testing.T) {
		unsafe := newProofUpload(data, metadata, proof)
		trailing := newProofUpload(data, metadata, proof)
		value := nonComparableProofUpload{proofUpload: unsafe, identity: []byte("unsafe")}
		_, err := client.Embed(t.Context(), directGeminiInputs(value, trailing), authorization)
		require.ErrorContains(t, err, "safely comparable")
		assert.Equal(t, int32(1), unsafe.closeCalls.Load())
		assert.Equal(t, int32(1), trailing.closeCalls.Load())
		assert.Zero(t, unsafe.metadataCalls.Load())
		assert.Zero(t, trailing.metadataCalls.Load())
	})

	t.Run("source attached to query", func(t *testing.T) {
		source := newProofUpload(data, metadata, proof)
		_, err := client.Embed(t.Context(), []document.EmbeddingInput{{
			Key: "query", Role: document.EmbeddingRoleQuery,
			Kind: document.EmbeddingInputQueryText, Text: "query", Source: source,
		}}, authorization)
		require.ErrorContains(t, err, "unsupported input role or kind")
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, source.metadataCalls.Load())
	})

	t.Run("oversized auxiliary still closes attached source", func(t *testing.T) {
		source := newProofUpload(data, metadata, proof)
		_, err := client.Embed(t.Context(), []document.EmbeddingInput{{
			Key: "chunk", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputRenditionChunk, Text: "text", Source: source,
			HeadingPath: make([]string, maximumHeadingDepth+1),
		}}, authorization)
		require.ErrorContains(t, err, "auxiliary bounds")
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, source.metadataCalls.Load())
		assert.Zero(t, source.proofCalls.Load())
	})

	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestDirectUploadCloseFailureIsReturnedWithoutEgress(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "close.png", "image/png")
	closeErr := errors.New("synthetic close failure")
	source := newProofUpload(data, metadata, proof)
	source.closeErr = closeErr
	client, secrets, requests := noEgressGeminiClient(t, profile)

	_, err := client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorIs(t, err, closeErr)
	assert.Equal(t, int32(1), source.closeCalls.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestDirectUploadRejectsMissingInvalidAndUnsupportedProofBeforeEgress(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "proof.png", "image/png")
	text := []byte("synthetic text\n")
	textMetadata, textProof := issuedGeminiAuthority(t, profile, text, "proof.txt", "text/plain")
	tests := []struct {
		name   string
		source *proofUpload
	}{
		{name: "missing", source: newProofUpload(data, metadata, proof)},
		{name: "invalid", source: newProofUpload(data, metadata, document.VerifiedUploadProof{})},
		{name: "unsupported", source: newProofUpload(text, textMetadata, textProof)},
	}
	tests[0].source.proofPresent = false
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, secrets, requests := noEgressGeminiClient(t, profile)
			_, err := client.Embed(t.Context(), directGeminiInputs(test.source), directGeminiAuthorization(profile, 1))
			require.Error(t, err)
			assert.Zero(t, test.source.readPasses.Load())
			assert.Equal(t, int32(1), test.source.closeCalls.Load())
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestDirectUploadRejectsCapacityPlusOneBeforeEgress(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "bounded.png", "image/png")
	source := newProofUpload(append(bytes.Clone(data), 1), metadata, proof)
	client, secrets, requests := noEgressGeminiClient(t, profile)

	_, err := client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.ErrorContains(t, err, "read exactly")
	assert.Equal(t, int32(1), source.closeCalls.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestDirectUploadProofProfileAndDisclosureTransplantsFailInActualCoreBeforeEgress(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{name: "descriptor", mutate: func(sourceProfile *Profile) {
			sourceProfile.Descriptor.Fingerprint = strings.Repeat("3", 64)
		}},
		{name: "profile", mutate: func(sourceProfile *Profile) {
			sourceProfile.CapabilityProfileFingerprint = strings.Repeat("3", 64)
		}},
		{name: "disclosure", mutate: func(sourceProfile *Profile) {
			sourceProfile.DisclosureFingerprint = strings.Repeat("3", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sourceProfile := profile
			test.mutate(&sourceProfile)
			source := authorizeGeminiFixture(t, sourceProfile, data, "transplant.png", "image/png")
			client, secrets, requests := noEgressGeminiClient(t, profile)

			_, err := document.ExecuteEmbedding(t.Context(), client, directGeminiInputs(source), geminiAuthorization(profile.Descriptor, 1))

			require.Error(t, err)
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestDirectUploadDifferentBytesUnderRealCopiedProofFailBeforeEgress(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "copied-proof.png", "image/png")
	changed := bytes.Clone(data)
	changed[len(changed)-1] ^= 0xff
	source := newProofUpload(changed, metadata, proof)
	client, secrets, requests := noEgressGeminiClient(t, profile)

	_, err := document.ExecuteEmbedding(t.Context(), client, directGeminiInputs(source), geminiAuthorization(profile.Descriptor, 1))

	require.ErrorContains(t, err, "checksum changed")
	assert.Equal(t, int32(1), source.closeCalls.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestDirectUploadRejectsMetadataAndProofChangesBeforeOrDuringRead(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "stable.png", "image/png")
	otherData := mediatest.PNG(3, 2, nil)
	_, otherProof := issuedGeminiAuthority(t, profile, otherData, "other.png", "image/png")
	changedMetadata := metadata
	changedMetadata.ProviderMetadataChecksum = strings.Repeat("f", 64)
	tests := []struct {
		name     string
		metadata []document.AuthorizedUploadMetadata
		proofs   []document.VerifiedUploadProof
	}{
		{name: "metadata before read", metadata: []document.AuthorizedUploadMetadata{metadata, changedMetadata}, proofs: []document.VerifiedUploadProof{proof}},
		{name: "metadata during read", metadata: []document.AuthorizedUploadMetadata{metadata, metadata, changedMetadata}, proofs: []document.VerifiedUploadProof{proof}},
		{name: "proof before read", metadata: []document.AuthorizedUploadMetadata{metadata}, proofs: []document.VerifiedUploadProof{proof, otherProof}},
		{name: "proof during read", metadata: []document.AuthorizedUploadMetadata{metadata}, proofs: []document.VerifiedUploadProof{proof, proof, otherProof}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newProofUpload(data, metadata, proof)
			source.metadata = test.metadata
			source.proofs = test.proofs
			client, secrets, requests := noEgressGeminiClient(t, profile)

			_, err := client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

			require.Error(t, err)
			assert.Equal(t, int32(1), source.closeCalls.Load())
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestDirectUploadCancellationInterruptsCoreSealedBlockedReadAndJoins(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.RequestTimeout = time.Second
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "blocked.png", "image/png")
	source := newProofUpload(data, metadata, proof)
	source.blockRead = true
	client, secrets, requests := noEgressGeminiClient(t, profile)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := document.ExecuteEmbedding(ctx, client, directGeminiInputs(source), geminiAuthorization(profile.Descriptor, 1))
		done <- err
	}()

	awaitDirectSignal(t, source.readStarted)
	cancel()
	err := awaitDirectError(t, done)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), source.closeCalls.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestDirectUploadClearsEarlierPreparedBytesWhenLaterItemFails(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "clear.png", "image/png")
	earlier := newProofUpload(data, metadata, proof)
	earlier.captureReadBuffer = true
	later := newProofUpload(data, metadata, proof)
	changedMetadata := metadata
	changedMetadata.ProviderMetadataChecksum = strings.Repeat("f", 64)
	later.metadata = []document.AuthorizedUploadMetadata{metadata, changedMetadata}
	client, secrets, requests := noEgressGeminiClient(t, profile)

	_, err := client.Embed(t.Context(), directGeminiInputs(earlier, later), directGeminiAuthorization(profile, 2))

	require.Error(t, err)
	require.NotEmpty(t, earlier.capturedReadBuffer)
	assert.Equal(t, make([]byte, len(earlier.capturedReadBuffer)), earlier.capturedReadBuffer)
	assert.Zero(t, later.readPasses.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestDirectUploadSnapshotsCallerInputsBeforeMetadataCallback(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "snapshot.png", "image/png")
	source := newProofUpload(data, metadata, proof)
	inputs := []document.EmbeddingInput{
		{Key: "image", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: source},
		{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "stable chunk", HeadingPath: []string{"stable"}, SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 6}}},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "stable query"},
	}
	source.onMetadata = func() {
		inputs[0].Key = "changed-image"
		inputs[1].Key = "changed-chunk"
		inputs[1].Text = "changed chunk"
		inputs[1].HeadingPath[0] = "changed"
		inputs[1].SourceSpans[0].CharEnd = 0
		inputs[2].Key = "changed-query"
		inputs[2].Text = "changed query"
	}
	var requests [][]byte
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		requests = append(requests, body)
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
	}))

	result, err := client.Embed(t.Context(), inputs, directGeminiAuthorization(profile, 3))

	require.NoError(t, err)
	assert.Equal(t, []string{"image", "chunk", "query"}, []string{result.Vectors[0].Key, result.Vectors[1].Key, result.Vectors[2].Key})
	require.Len(t, requests, 3)
	assert.Contains(t, string(requests[1]), "stable chunk")
	assert.NotContains(t, string(requests[1]), "changed chunk")
	assert.Contains(t, string(requests[2]), "stable query")
	assert.NotContains(t, string(requests[2]), "changed query")
}

func TestDirectUploadSnapshotsCallerInputsBeforeProofCallback(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "proof-snapshot.png", "image/png")
	source := newProofUpload(data, metadata, proof)
	inputs := []document.EmbeddingInput{
		{Key: "image", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: source},
		{Key: "chunk", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "proof-stable", HeadingPath: []string{"stable"}, SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 6}}},
	}
	source.onProof = func() {
		inputs[0].Key = "changed-image"
		inputs[1].Key = "changed-chunk"
		inputs[1].Text = "proof-changed"
		inputs[1].HeadingPath[0] = "changed"
		inputs[1].SourceSpans[0].CharEnd = 0
	}
	var requests [][]byte
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		requests = append(requests, body)
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
	}))

	result, err := client.Embed(t.Context(), inputs, directGeminiAuthorization(profile, 2))

	require.NoError(t, err)
	assert.Equal(t, []string{"image", "chunk"}, []string{result.Vectors[0].Key, result.Vectors[1].Key})
	require.Len(t, requests, 2)
	assert.Contains(t, string(requests[1]), "proof-stable")
	assert.NotContains(t, string(requests[1]), "proof-changed")
}

func TestDirectTransportRequestCapacityFailsBeforeReadSecretOrHTTP(t *testing.T) {
	data := mediatest.PNG(2, 2, nil)
	t.Run("inline envelope", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		profile.MaxRequestBytes = 1
		profile.Descriptor = geminiDescriptorFor(t, profile)
		metadata, proof := issuedGeminiAuthority(t, profile, data, "preflight.png", "image/png")
		source := newProofUpload(data, metadata, proof)
		client, secrets, requests := noEgressGeminiClient(t, profile)

		_, err := client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

		require.ErrorContains(t, err, "request")
		assert.Zero(t, source.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})

	t.Run("Files API raw upload", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		profile.Transport = TransportFilesAPI
		profile.Descriptor = geminiDescriptorFor(t, profile)
		metadata, proof := issuedGeminiAuthority(t, profile, data, "files.png", "image/png")
		source := newProofUpload(data, metadata, proof)
		client, secrets, requests := noEgressGeminiClient(t, profile)
		client.profile.MaxRequestBytes = 1

		_, err := client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

		require.ErrorContains(t, err, "raw file upload exceeds request byte capacity")
		assert.Zero(t, source.readPasses.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})
}

func TestInlineDirectPreflightAccountsForBase64AndProviderPDFEnvelope(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.MaxInputBytes = maximumInputBytes
	profile.MaxRequestBytes = maximumRequest
	profile.Descriptor = geminiDescriptorFor(t, profile)
	client, _, _ := noEgressGeminiClient(t, profile)
	// This raw length base64-encodes to exactly 50 MiB. The fixed JSON
	// envelope therefore puts a PDF over its conservative encoded ceiling.
	rawBytes := int64(50<<20) * 3 / 4

	err := client.preflightInlineRequest("application/pdf", rawBytes)
	require.ErrorContains(t, err, "provider byte capacity")
	require.NoError(t, client.preflightInlineRequest("image/png", rawBytes))
}

func issuedGeminiAuthority(t *testing.T, profile Profile, data []byte, filename, mediaType string) (document.AuthorizedUploadMetadata, document.VerifiedUploadProof) {
	t.Helper()
	source := authorizeGeminiFixture(t, profile, data, filename, mediaType)
	metadata := source.Metadata()
	carrier, ok := source.(document.VerifiedUploadProofCarrier)
	require.True(t, ok)
	proof, present := carrier.VerifiedUploadProof()
	require.True(t, present)
	require.True(t, proof.Valid())
	require.NoError(t, source.Close())
	return metadata, proof
}

func directGeminiInputs(sources ...document.AuthorizedUpload) []document.EmbeddingInput {
	inputs := make([]document.EmbeddingInput, len(sources))
	for index, source := range sources {
		inputs[index] = document.EmbeddingInput{
			Key: "direct-" + string(rune('a'+index)), Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: source,
		}
	}
	return inputs
}

func directGeminiAuthorization(profile Profile, items int) document.EmbeddingAuthorization {
	authorization := geminiAuthorization(profile.Descriptor, items)
	authorization.MaxInputBytes = profile.MaxInputBytes
	authorization.DiscloseFilename = true
	return authorization
}

func noEgressGeminiClient(t *testing.T, profile Profile) (*Client, *countingSecrets, *atomic.Int32) {
	t.Helper()
	secrets := &countingSecrets{value: "synthetic-key"}
	requests := new(atomic.Int32)
	client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected synthetic egress")
	}))
	return client, secrets, requests
}

type proofUpload struct {
	reader *bytes.Reader

	metadata     []document.AuthorizedUploadMetadata
	proofs       []document.VerifiedUploadProof
	proofPresent bool
	onMetadata   func()
	onProof      func()

	metadataCalls atomic.Int32
	proofCalls    atomic.Int32
	readPasses    atomic.Int32
	closeCalls    atomic.Int32
	closeErr      error

	blockRead          bool
	captureReadBuffer  bool
	capturedReadBuffer []byte
	readStarted        chan struct{}
	released           chan struct{}
	startOnce          sync.Once
	releaseOnce        sync.Once
}

func newProofUpload(data []byte, metadata document.AuthorizedUploadMetadata, proof document.VerifiedUploadProof) *proofUpload {
	return &proofUpload{
		reader: bytes.NewReader(data), metadata: []document.AuthorizedUploadMetadata{metadata},
		proofs: []document.VerifiedUploadProof{proof}, proofPresent: true,
		readStarted: make(chan struct{}), released: make(chan struct{}),
	}
}

func (upload *proofUpload) Read(buffer []byte) (int, error) {
	upload.startOnce.Do(func() {
		upload.readPasses.Add(1)
		close(upload.readStarted)
	})
	if upload.blockRead {
		<-upload.released
		return 0, errors.New("synthetic interrupted read")
	}
	count, err := upload.reader.Read(buffer)
	if upload.captureReadBuffer && count > 0 && upload.capturedReadBuffer == nil {
		upload.capturedReadBuffer = buffer[:count]
	}
	return count, err //nolint:wrapcheck // Preserve the reader contract.
}

func (upload *proofUpload) Close() error {
	upload.closeCalls.Add(1)
	upload.releaseOnce.Do(func() { close(upload.released) })
	return upload.closeErr
}

func (upload *proofUpload) Metadata() document.AuthorizedUploadMetadata {
	call := int(upload.metadataCalls.Add(1)) - 1
	if upload.onMetadata != nil {
		upload.onMetadata()
	}
	return upload.metadata[min(call, len(upload.metadata)-1)]
}

func (upload *proofUpload) VerifiedUploadProof() (document.VerifiedUploadProof, bool) {
	call := int(upload.proofCalls.Add(1)) - 1
	if upload.onProof != nil {
		upload.onProof()
	}
	return upload.proofs[min(call, len(upload.proofs)-1)], upload.proofPresent
}

type nonComparableProofUpload struct {
	*proofUpload

	identity []byte
}

func awaitDirectSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for direct upload read")
	}
}

func awaitDirectError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for direct upload result")
		return nil
	}
}
