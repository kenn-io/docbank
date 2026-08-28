package cohereembed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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
)

func TestEmbedClosesAllOriginalUploadsWhenAuthorizationIsInvalid(t *testing.T) {
	first := newLifecycleUpload(t, tinyPNG(t), imageMetadata(t, tinyPNG(t)))
	second := newLifecycleUpload(t, tinyPNG(t), imageMetadata(t, tinyPNG(t)))
	secrets := &countingSecrets{value: "synthetic-key"}
	var requests atomic.Int32
	client := testClient(t, testProfile(t, 256), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("request must not run")
	}))
	permission := authorization(client.Descriptor(), 2)
	permission.ProviderID = "wrong-provider"

	_, err := client.Embed(context.Background(), imageInputs(first, second), permission)
	require.Error(t, err)
	assert.Equal(t, int32(1), first.closeCalls.Load())
	assert.Equal(t, int32(1), second.closeCalls.Load())
	assert.Equal(t, int32(1), first.metadataCalls.Load())
	assert.Equal(t, int32(1), second.metadataCalls.Load())
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestEmbedClosesAllUploadsWithoutEgressWhenFrozenMetadataIsInvalidOrDrifts(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata func(t *testing.T, data []byte) []document.AuthorizedUploadMetadata
		calls    int32
	}{
		{name: "invalid snapshot", calls: 1, metadata: func(t *testing.T, data []byte) []document.AuthorizedUploadMetadata {
			t.Helper()
			value := imageMetadata(t, data)
			value.ByteLength = -1
			return []document.AuthorizedUploadMetadata{value}
		}},
		{name: "live drift", calls: 2, metadata: func(t *testing.T, data []byte) []document.AuthorizedUploadMetadata {
			t.Helper()
			value := imageMetadata(t, data)
			changed := value
			changed.ProviderMetadataChecksum = strings.Repeat("c", 64)
			return []document.AuthorizedUploadMetadata{value, changed}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tinyPNG(t)
			first := newLifecycleUpload(t, data, test.metadata(t, data)...)
			second := newLifecycleUpload(t, data, imageMetadata(t, data))
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := testClient(t, testProfile(t, 256), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("request must not run")
			}))

			_, err := client.Embed(context.Background(), imageInputs(first, second), authorization(client.Descriptor(), 2))
			require.Error(t, err)
			assert.Equal(t, int32(1), first.closeCalls.Load())
			assert.Equal(t, int32(1), second.closeCalls.Load())
			assert.Equal(t, test.calls, first.metadataCalls.Load())
			assert.Equal(t, int32(1), second.metadataCalls.Load())
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestEmbedUsesOneFrozenAuthoritySnapshotWithLiveDriftComparisons(t *testing.T) {
	data := tinyPNG(t)
	source := newLifecycleUpload(t, data, imageMetadata(t, data))
	client := testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, imageSuccessTransport(t))

	_, err := client.Embed(context.Background(), imageInputs(source), authorization(client.Descriptor(), 1))
	require.NoError(t, err)
	assert.Equal(t, int32(3), source.metadataCalls.Load())
	assert.Equal(t, int32(1), source.closeCalls.Load())
}

func TestEmbedCancellationOrTimeoutClosesBlockedImageAndEveryEnrolledUploadOnce(t *testing.T) {
	for _, test := range []struct {
		name       string
		blockIndex int
		timeout    bool
	}{
		{name: "cancel first", blockIndex: 0},
		{name: "timeout middle", blockIndex: 1, timeout: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tinyPNG(t)
			sources := []*lifecycleUpload{
				newLifecycleUpload(t, data, imageMetadata(t, data)),
				newLifecycleUpload(t, data, imageMetadata(t, data)),
				newLifecycleUpload(t, data, imageMetadata(t, data)),
			}
			blocked := sources[test.blockIndex]
			blocked.block = true
			defer blocked.releaseRead()
			profile := testProfile(t, 256)
			if test.timeout {
				profile.RequestTimeout = 25 * time.Millisecond
				profile.Descriptor = descriptorFor(t, profile)
			}
			secrets := &countingSecrets{value: "synthetic-key"}
			client := testClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("request must not run")
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.Embed(ctx, imageInputs(sources...), authorization(client.Descriptor(), len(sources)))
				result <- err
			}()
			select {
			case <-blocked.readStarted:
			case <-time.After(time.Second):
				t.Fatal("blocked image read did not start")
			}
			if !test.timeout {
				cancel()
			}
			var err error
			select {
			case err = <-result:
			case <-time.After(time.Second):
				blocked.releaseRead()
				t.Fatal("embedding did not return after cancellation")
			}
			if test.timeout {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			for _, source := range sources {
				assert.Equal(t, int32(1), source.closeCalls.Load())
			}
			assert.Zero(t, secrets.calls.Load())
		})
	}
}

func imageInputs(sources ...*lifecycleUpload) []document.EmbeddingInput {
	inputs := make([]document.EmbeddingInput, len(sources))
	for index, source := range sources {
		inputs[index] = document.EmbeddingInput{Key: "image-" + string(rune('a'+index)), Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputOriginalFile, Source: source}
	}
	return inputs
}

func imageMetadata(t *testing.T, data []byte) document.AuthorizedUploadMetadata {
	t.Helper()
	digest := sha256.Sum256(data)
	return document.AuthorizedUploadMetadata{Filename: "synthetic.png", MediaFamily: "image", MediaType: "image/png",
		ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("a", 64), ProviderMetadataChecksum: strings.Repeat("b", 64),
		InputKind: document.RenditionInputOriginalFile}
}

func imageSuccessTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		vector := make([]float32, 256)
		body, err := json.Marshal(map[string]any{"id": "synthetic", "embeddings": map[string]any{"float": [][]float32{vector}},
			"images": []map[string]any{{"width": 1, "height": 1, "format": "png", "bit_depth": 8}}})
		require.NoError(t, err)
		return jsonResponse(request, http.StatusOK, body), nil
	})
}

type lifecycleUpload struct {
	data *bytes.Reader

	metadata      []document.AuthorizedUploadMetadata
	metadataCalls atomic.Int32
	closeCalls    atomic.Int32
	block         bool
	readStarted   chan struct{}
	released      chan struct{}
	startOnce     sync.Once
	releaseOnce   sync.Once
}

func newLifecycleUpload(t *testing.T, data []byte, metadata ...document.AuthorizedUploadMetadata) *lifecycleUpload {
	t.Helper()
	require.NotEmpty(t, metadata)
	return &lifecycleUpload{data: bytes.NewReader(data), metadata: metadata,
		readStarted: make(chan struct{}), released: make(chan struct{})}
}

func (upload *lifecycleUpload) Read(value []byte) (int, error) {
	if upload.block {
		upload.startOnce.Do(func() { close(upload.readStarted) })
		<-upload.released
		return 0, errors.New("synthetic source closed")
	}
	//nolint:wrapcheck // The synthetic reader must preserve io.EOF for io.ReadAll.
	return upload.data.Read(value)
}

func (upload *lifecycleUpload) Close() error {
	upload.closeCalls.Add(1)
	upload.releaseRead()
	return nil
}

func (upload *lifecycleUpload) Metadata() document.AuthorizedUploadMetadata {
	call := int(upload.metadataCalls.Add(1)) - 1
	return upload.metadata[min(call, len(upload.metadata)-1)]
}

func (upload *lifecycleUpload) releaseRead() {
	upload.releaseOnce.Do(func() { close(upload.released) })
}

var _ document.AuthorizedUpload = (*lifecycleUpload)(nil)
var _ io.Reader = (*lifecycleUpload)(nil)
