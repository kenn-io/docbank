package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

func TestParseDescription(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw string
		want      document.PhotoDescription
		invalid   bool
	}{
		{name: "normalization", raw: "```json\n{\"caption\":\" A dog on a beach. \",\"tags\":[\" DOG! \",\"dog\",\"ＢＥＡＣＨ\",\"🐕\"],\"visible_text\":\"Welcome\"}\n```", want: document.PhotoDescription{Caption: "A dog on a beach.", Tags: []string{"dog", "beach"}, VisibleText: "Welcome"}},
		{name: "missing visible text", raw: `{"caption":"ok","tags":[]}`, invalid: true},
		{name: "null tags", raw: `{"caption":"ok","tags":null,"visible_text":""}`, invalid: true},
		{name: "numeric tag", raw: `{"caption":"ok","tags":[1],"visible_text":""}`, invalid: true},
		{name: "oversized tag rejects complete result", raw: `{"caption":"A coast.","tags":["coast","` + strings.Repeat("é", 128) + `"],"visible_text":"QUAYSIDE"}`, invalid: true},
		{name: "long caption", raw: `{"caption":"` + strings.Repeat("x", 301) + `","tags":[],"visible_text":""}`, invalid: true},
		{name: "empty", raw: `{"caption":"","tags":[],"visible_text":""}`, want: document.PhotoDescription{Tags: []string{}}},
		{name: "tag punctuation", raw: `{"caption":"Programming books.","tags":["C++","C#"],"visible_text":""}`, want: document.PhotoDescription{Caption: "Programming books.", Tags: []string{"c++", "c#"}}},
		{name: "canonical duplicate after format removal", raw: `{"caption":"Cafés.","tags":["cafés","cafe\u200d\u0301s"],"visible_text":""}`, want: document.PhotoDescription{Caption: "Cafés.", Tags: []string{"cafés"}}},
		{name: "caption pipes", raw: `{"caption":"A sign reads red | green | blue.","tags":[],"visible_text":""}`, want: document.PhotoDescription{Caption: "A sign reads red | green | blue.", Tags: []string{}}},
		{name: "filename underscores", raw: `{"caption":"A print labeled IMG_1234_final.","tags":[],"visible_text":""}`, want: document.PhotoDescription{Caption: "A print labeled IMG_1234_final.", Tags: []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := Parse([]byte(tc.raw))
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		})
	}
}
func TestRenderSendsImageBytesAndPublishesAllFields(t *testing.T) {
	t.Parallel()
	for _, malformed := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("malformed_%d", malformed), func(t *testing.T) {
			var calls atomic.Int32
			var imageData bytes.Buffer
			require.NoError(t, jpeg.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 512, 1)), nil))
			data := imageData.Bytes()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/chat/completions", r.URL.Path)
				assert.Equal(t, "POST", r.Method)
				wire, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					return
				}
				var fields map[string]jsontext.Value
				if !assert.NoError(t, json.Unmarshal(wire, &fields)) {
					return
				}
				assert.NotContains(t, fields, "temperature")
				var body request
				if !assert.NoError(t, json.Unmarshal(wire, &body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, "synthetic-vision", body.Model)
				encoded := strings.TrimPrefix(body.Messages[0].Content[1].ImageURL.URL, "data:image/jpeg;base64,")
				actual, err := base64.StdEncoding.DecodeString(encoded)
				assert.NoError(t, err)
				assert.Equal(t, data, actual)
				w.Header().Set("Content-Type", "application/json")
				content := `{"caption":"A dog.","tags":["dog"],"visible_text":"HELLO"}`
				if int(calls.Add(1)) <= malformed {
					content = `{"caption":"incomplete"}`
				}

				_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}]}`, content)
			}))
			defer server.Close()
			p, err := New(Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
			require.NoError(t, err)
			descriptor := p.Descriptor()
			now := time.Now().UTC()
			metadata := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputDerivedUpload, ByteLength: int64(len(data)), SHA256: providerutil.SHA256Hex(data), CapabilityRecordChecksum: strings.Repeat("a", 64), ProviderMetadataChecksum: strings.Repeat("b", 64)}
			auth := document.RenditionAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("c", 64), SourceSHA256: strings.Repeat("d", 64), SourceBytes: 8192, CapabilityRecordChecksum: metadata.CapabilityRecordChecksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum, MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputDerivedUpload, AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, MaxArtifactBytes: 1 << 20, MaxArtifacts: 1, MaxTotalResultBytes: 1 << 20, AuthorizedAt: now.Format("2006-01-02T15:04:05.000000000Z"), ExpiresAt: now.Add(time.Minute).Format("2006-01-02T15:04:05.000000000Z")}
			result, err := document.RenderRendition(context.Background(), p, &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data)), metadata: metadata}, auth)
			require.Equal(t, int32(min(malformed+1, 2)), calls.Load())
			if malformed == 2 {
				classified, ok := errors.AsType[*document.RenditionProviderError](err)
				require.True(t, ok)
				require.Equal(t, document.RenditionErrorMalformedEvidence, classified.Code())
				require.Empty(t, result.Artifacts)
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Artifacts, 1)
			require.JSONEq(t, `{"caption":"A dog.","tags":["dog"],"visible_text":"HELLO"}`, string(result.Artifacts[0].Payload))
			require.Equal(t, auth.SourceSHA256, result.Receipt.SourceSHA256)
			require.Equal(t, metadata.SHA256, result.Receipt.UploadSHA256)
		})
	}
}

type testUpload struct {
	io.ReadCloser

	metadata document.AuthorizedUploadMetadata
}

func (u *testUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }

func TestProbeRequiresRecognitionOfSyntheticImage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		answer string
		ok     bool
	}{
		{answer: "red,blue", ok: true},
		{answer: "red, blue", ok: true},
		{answer: "Red, blue.", ok: true},
		{answer: "ok"},
		{answer: "blue,red"},
		{answer: "Red, blue. The left side is red."},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body request
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
					return
				}
				if assert.NotNil(t, body.Temperature) {
					assert.Zero(t, *body.Temperature)
				}
				encoded := strings.TrimPrefix(body.Messages[0].Content[1].ImageURL.URL, "data:image/jpeg;base64,")
				data, err := base64.StdEncoding.DecodeString(encoded)
				if !assert.NoError(t, err) {
					return
				}
				img, err := jpeg.Decode(bytes.NewReader(data))
				if !assert.NoError(t, err) {
					return
				}
				if tc.answer == "red,blue" {
					assert.NotContains(t, body.Messages[0].Content[0].Text, "red")
					assert.NotContains(t, body.Messages[0].Content[0].Text, "blue")
					assert.Equal(t, image.Rect(0, 0, 64, 32), img.Bounds())
					red, _, blue, _ := img.At(16, 16).RGBA()
					assert.Greater(t, red, blue)
					red, _, blue, _ = img.At(48, 16).RGBA()
					assert.Greater(t, blue, red)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": tc.answer}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			p, err := New(Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
			require.NoError(t, err)
			err = p.Probe(t.Context())
			if tc.ok {
				require.NoError(t, err)
			} else {
				classified, ok := errors.AsType[*document.RenditionProviderError](err)
				require.True(t, ok)
				require.Equal(t, document.RenditionErrorMalformedEvidence, classified.Code())
			}
		})
	}
}

func TestRenderRejectsPreviewAbove1024PixelsBeforeHTTP(t *testing.T) {
	t.Parallel()
	for _, dimensions := range []image.Point{{X: 1025, Y: 1}, {X: 1, Y: 1025}} {
		t.Run(fmt.Sprintf("%dx%d", dimensions.X, dimensions.Y), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			p, err := New(Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
			require.NoError(t, err)
			var data bytes.Buffer
			require.NoError(t, jpeg.Encode(&data, image.NewRGBA(image.Rectangle{Max: dimensions}), nil))
			metadata := document.AuthorizedUploadMetadata{InputKind: document.RenditionInputDerivedUpload, ByteLength: int64(data.Len()), SHA256: providerutil.SHA256Hex(data.Bytes())}
			_, err = p.Render(t.Context(), &testUpload{ReadCloser: io.NopCloser(bytes.NewReader(data.Bytes())), metadata: metadata}, document.RenditionAuthorization{})
			require.Error(t, err)
			classified, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			require.Equal(t, document.RenditionErrorUnsupportedInput, classified.Code())
			require.Zero(t, calls.Load())
		})
	}
}
