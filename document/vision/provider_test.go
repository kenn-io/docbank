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
	"go.kenn.io/docbank/document/documenttest"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/vision/visiontest"
)

func TestParseDescription(t *testing.T) {
	t.Parallel()
	tags := make([]string, 65)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag-%02d", i)
	}
	validTags, err := json.Marshal(document.PhotoDescription{Caption: "A coast.", Tags: tags})
	require.NoError(t, err)

	for _, tc := range []struct {
		name, raw string
		want      document.PhotoDescription
		invalid   bool
	}{
		{name: "65 tags retain the first ten", raw: string(validTags), want: document.PhotoDescription{Caption: "A coast.", Tags: []string{"tag-00", "tag-01", "tag-02", "tag-03", "tag-04", "tag-05", "tag-06", "tag-07", "tag-08", "tag-09"}}},
		{name: "normalization", raw: "```json\n{\"caption\":\" A dog on a beach. \",\"tags\":[\" DOG! \",\"dog\",\"ＢＥＡＣＨ\",\"🐕\"],\"visible_text\":\"Welcome\"}\n```", want: document.PhotoDescription{Caption: "A dog on a beach.", Tags: []string{"DOG", "dog", "ＢＥＡＣＨ"}, VisibleText: "Welcome"}},
		{name: "caption line break", raw: `{"caption":"A coast.\n A harbor.","tags":[],"visible_text":""}`, want: document.PhotoDescription{Caption: "A coast. A harbor.", Tags: []string{}}},
		{name: "caption list marker", raw: `{"caption":"- Harbor at dusk","tags":["Coast","coast"],"visible_text":"QUAY"}`, want: document.PhotoDescription{Caption: "Harbor at dusk", Tags: []string{"Coast", "coast"}, VisibleText: "QUAY"}},
		{name: "caption emphasis", raw: `{"caption":"**Harbor** at dusk","tags":[],"visible_text":""}`, want: document.PhotoDescription{Caption: "Harbor at dusk", Tags: []string{}}},
		{name: "numbered caption with emphasis", raw: `{"caption":"1. _Harbor_ at dusk","tags":[],"visible_text":""}`, want: document.PhotoDescription{Caption: "Harbor at dusk", Tags: []string{}}},
		{name: "caption link drops only the caption", raw: `{"caption":"[Harbor](https://example.com)","tags":["coast"],"visible_text":"QUAY"}`, want: document.PhotoDescription{Tags: []string{"coast"}, VisibleText: "QUAY"}},
		{name: "missing visible text keeps caption and tags", raw: `{"caption":"ok","tags":["coast"]}`, want: document.PhotoDescription{Caption: "ok", Tags: []string{"coast"}}},
		{name: "null tags drop alone", raw: `{"caption":"ok","tags":null,"visible_text":"QUAY"}`, want: document.PhotoDescription{Caption: "ok", Tags: []string{}, VisibleText: "QUAY"}},
		{name: "tags that are not an array drop alone", raw: `{"caption":"ok","tags":"coast","visible_text":"QUAY"}`, want: document.PhotoDescription{Caption: "ok", Tags: []string{}, VisibleText: "QUAY"}},
		{name: "numeric tag drops alone", raw: `{"caption":"ok","tags":[1,"coast",null],"visible_text":"QUAY"}`, want: document.PhotoDescription{Caption: "ok", Tags: []string{"coast"}, VisibleText: "QUAY"}},
		{name: "numeric visible text drops alone", raw: `{"caption":"ok","tags":["coast"],"visible_text":42}`, want: document.PhotoDescription{Caption: "ok", Tags: []string{"coast"}}},
		{name: "numeric caption drops alone", raw: `{"caption":7,"tags":["coast"],"visible_text":"QUAY"}`, want: document.PhotoDescription{Tags: []string{"coast"}, VisibleText: "QUAY"}},
		{name: "missing caption and tags keep visible text", raw: `{"visible_text":"QUAY"}`, want: document.PhotoDescription{Tags: []string{}, VisibleText: "QUAY"}},
		{name: "object with no description field", raw: `{"description":"A harbor."}`, invalid: true},
		{name: "array reply", raw: `["coast"]`, invalid: true},
		{name: "oversized tag is dropped alone", raw: `{"caption":"A coast.","tags":["coast","` + strings.Repeat("é", 128) + `"],"visible_text":"QUAYSIDE"}`, want: document.PhotoDescription{Caption: "A coast.", Tags: []string{"coast"}, VisibleText: "QUAYSIDE"}},
		{name: "tag that normalizes past the byte limit is dropped alone", raw: `{"caption":"A coast.","tags":["coast","x` + strings.Repeat("\u0344", 127) + `"],"visible_text":"QUAYSIDE"}`, want: document.PhotoDescription{Caption: "A coast.", Tags: []string{"coast"}, VisibleText: "QUAYSIDE"}},
		{name: "long visible text is clipped", raw: `{"caption":"A sign.","tags":["sign"],"visible_text":"` + strings.Repeat("é", document.MaxPhotoTextBytes/2+10) + `"}`, want: document.PhotoDescription{Caption: "A sign.", Tags: []string{"sign"}, VisibleText: strings.Repeat("é", document.MaxPhotoTextBytes/2)}},
		{name: "reply that is not JSON", raw: `A harbor at dusk.`, invalid: true},
		{name: "long caption drops only the caption", raw: `{"caption":"` + strings.Repeat("x", 301) + `","tags":["coast"],"visible_text":"QUAY"}`, want: document.PhotoDescription{Tags: []string{"coast"}, VisibleText: "QUAY"}},
		{name: "empty", raw: `{"caption":"","tags":[],"visible_text":""}`, want: document.PhotoDescription{Tags: []string{}}},
		{name: "tag punctuation", raw: `{"caption":"Programming books.","tags":["C++","C#"],"visible_text":""}`, want: document.PhotoDescription{Caption: "Programming books.", Tags: []string{"C++", "C#"}}},
		{name: "trailing combining mark", raw: `{"caption":"A cafe.","tags":["cafe\u0301!"],"visible_text":""}`, want: document.PhotoDescription{Caption: "A cafe.", Tags: []string{"cafe\u0301"}}},
		{name: "format removal leaves normalization to the store", raw: `{"caption":"Cafés.","tags":["cafés","cafe\u200d\u0301s"],"visible_text":""}`, want: document.PhotoDescription{Caption: "Cafés.", Tags: []string{"cafés", "cafe\u0301s"}}},
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

func TestRenderSendsImageBytesOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, reply, want string }{
		{"valid reply", `{"caption":"A dog.","tags":["dog"],"visible_text":"HELLO"}`, `{"caption":"A dog.","tags":["dog"],"visible_text":"HELLO"}`},
		{"reply that is not JSON", `A dog.`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				if _, exists := fields["temperature"]; exists {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, jsontext.Value("2048"), fields["max_completion_tokens"])
				assert.NotContains(t, fields, "max_tokens")
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
				calls.Add(1)
				_, _ = w.Write(visiontest.Completion(tc.reply))
			}))
			defer server.Close()
			p, err := New(Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
			require.NoError(t, err)
			descriptor := p.Descriptor()
			now := time.Now().UTC()
			metadata := document.AuthorizedUploadMetadata{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputDerivedUpload, ByteLength: int64(len(data)), SHA256: providerutil.SHA256Hex(data), CapabilityRecordChecksum: strings.Repeat("a", 64), ProviderMetadataChecksum: strings.Repeat("b", 64)}
			auth := document.RenditionAuthorization{ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint, RenditionRequestFingerprint: strings.Repeat("c", 64), SourceSHA256: strings.Repeat("d", 64), SourceBytes: 8192, CapabilityRecordChecksum: metadata.CapabilityRecordChecksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum, MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputDerivedUpload, AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, MaxArtifactBytes: 1 << 20, MaxArtifacts: 1, MaxTotalResultBytes: 1 << 20, AuthorizedAt: now.Format("2006-01-02T15:04:05.000000000Z"), ExpiresAt: now.Add(time.Minute).Format("2006-01-02T15:04:05.000000000Z")}
			result, err := document.RenderRendition(context.Background(), p, &documenttest.Upload{ReadCloser: io.NopCloser(bytes.NewReader(data)), Meta: metadata}, auth)
			require.Equal(t, int32(1), calls.Load())
			if tc.want == "" {
				classified, ok := errors.AsType[*document.RenditionProviderError](err)
				require.True(t, ok)
				require.Equal(t, document.RenditionErrorMalformedEvidence, classified.Code())
				require.Empty(t, result.Artifacts)
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Artifacts, 1)
			require.JSONEq(t, tc.want, string(result.Artifacts[0].Payload))
			require.Equal(t, auth.SourceSHA256, result.Receipt.SourceSHA256)
			require.Equal(t, metadata.SHA256, result.Receipt.UploadSHA256)
		})
	}
}

func TestProbeSendsSyntheticImageWithoutNamingColors(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body request
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
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
		assert.NotContains(t, body.Messages[0].Content[0].Text, "red")
		assert.NotContains(t, body.Messages[0].Content[0].Text, "blue")
		assert.Equal(t, image.Rect(0, 0, 64, 32), img.Bounds())
		red, _, blue, _ := img.At(16, 16).RGBA()
		assert.Greater(t, red, blue)
		red, _, blue, _ = img.At(48, 16).RGBA()
		assert.Greater(t, blue, red)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion("red,blue"))
	}))
	defer server.Close()
	p, err := New(Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	require.NoError(t, p.Probe(t.Context()))
}

func TestProbeRequiresRecognitionOfSyntheticImage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		answer string
		ok     bool
	}{
		{answer: "red, blue", ok: true},
		{answer: "Red, blue.", ok: true},
		{answer: "left: red, right: blue", ok: true},
		{answer: "ok"},
		{answer: "blue,red"},
		{answer: "left: blue, right: red"},
		{answer: "right: red, left: blue"},
		{answer: "Red, blue. The left side is red."},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(visiontest.Completion(tc.answer))
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
