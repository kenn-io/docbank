package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTextUnitCounters(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		for _, text := range []string{
			`{"name":"synthetic"}`,
			`[1,{"nested":true},null]`,
			`"scalar"`,
			"  42  \n",
			"{\n  \"items\": [1, 2, 3]\n}\n",
		} {
			assertCounter(t, countJSONValues, []byte(text), 1)
		}
		for _, text := range []string{"", "   ", `{`, `{"a":1} {"b":2}`, `true false`} {
			assertCounterError(t, countJSONValues, []byte(text))
		}
	})

	t.Run("mail", func(t *testing.T) {
		message := []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nbody\r\n")
		assertCounter(t, countMailMessages, message, 1)
		assertCounterError(t, countMailMessages, []byte("not a mail message"))
	})
}

func TestTextCountersRejectEmptyShortAndUnreadableInput(t *testing.T) {
	for _, test := range []struct {
		name    string
		counter localUnitCounter
	}{
		{name: "json", counter: countJSONValues},
		{name: "eml", counter: countMailMessages},
	} {
		t.Run(test.name+" empty", func(t *testing.T) {
			_, err := test.counter(bytes.NewReader(nil), 0)
			require.Error(t, err)
		})
		t.Run(test.name+" short", func(t *testing.T) {
			_, err := test.counter(bytes.NewReader([]byte("x\n")), 100)
			require.Error(t, err)
		})
	}
	failing := &failingReaderAt{data: []byte(`{"a":1}`), failAt: 2}
	_, err := countJSONValues(failing, int64(len(failing.data)))
	require.Error(t, err)
}

func TestTextLocalExactAuthorization(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := textAuthorityManifest(t, policy, "json", "eml")

	for _, test := range []struct {
		name    string
		format  string
		content []byte
	}{
		{name: "json", format: "json", content: []byte(`{"value":"synthetic"}`)},
		{name: "eml", format: "eml", content: []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nbody\r\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, found := CandidateFormatByID(test.format)
			require.True(t, found)
			authorization, err := policy.Authorize(manifest, test.format)
			require.NoError(t, err)
			prepared := prepareTextDocument(t, policy, candidate, test.content)
			var requests atomic.Int64
			var uploaded []byte
			client, err := NewClient(policy, ClientConfig{
				APIKey: "synthetic-key", MaxRetries: 1,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					requests.Add(1)
					body, readErr := io.ReadAll(request.Body)
					if readErr != nil {
						return nil, readErr
					}
					var envelope struct {
						Document struct {
							URL string `json:"document_url"`
						} `json:"document"`
					}
					if unmarshalErr := json.Unmarshal(body, &envelope); unmarshalErr != nil {
						return nil, unmarshalErr
					}
					encoded := strings.TrimPrefix(envelope.Document.URL,
						"data:"+candidate.MediaType+";base64,")
					var decodeErr error
					uploaded, decodeErr = base64.StdEncoding.DecodeString(encoded)
					if decodeErr != nil {
						return nil, fmt.Errorf("decode synthetic upload: %w", decodeErr)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(testSuccessfulResponse)), Request: request,
					}, nil
				})},
			})
			require.NoError(t, err)
			result, err := client.Process(t.Context(), prepared, authorization)
			require.NoError(t, err)
			assert.Equal(t, 1, result.UnitsProcessed)
			assert.Equal(t, int64(1), requests.Load())
			assert.Equal(t, test.content, uploaded)
		})
	}
}

func TestTextLocalUnitsRejectOverLimitBeforeHTTP(t *testing.T) {
	policy := testPolicy(t, 1<<20, MaxUnits)
	manifest := textAuthorityManifest(t, policy, "json")
	authorization, err := policy.Authorize(manifest, "json")
	require.NoError(t, err)
	candidate, found := CandidateFormatByID("json")
	require.True(t, found)
	prepared := prepareTextDocument(t, policy, candidate, []byte(`{"value":"synthetic"}`))
	prepared.localUnits = policy.values.MaxUnits + 1
	var requests atomic.Int64
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key", MaxRetries: 1,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("unexpected provider request")
		})},
	})
	require.NoError(t, err)
	snapshot, err := prepared.snapshot()
	require.NoError(t, err)
	snapshot.localUnits = MaxUnits
	require.NoError(t, client.validatePreparedSnapshot(snapshot, authorization))
	snapshot.localUnits = MaxUnits + 1
	require.ErrorIs(t, client.validatePreparedSnapshot(snapshot, authorization), ErrCapabilityContract)
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
	assert.Zero(t, requests.Load())
}

func TestTextLocalUnitsRejectProviderMismatch(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := textAuthorityManifest(t, policy, "json")
	authorization, err := policy.Authorize(manifest, "json")
	require.NoError(t, err)
	candidate, found := CandidateFormatByID("json")
	require.True(t, found)
	prepared := prepareTextDocument(t, policy, candidate, []byte(`{"value":"synthetic"}`))
	var requests atomic.Int64
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key", MaxRetries: 1,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			_, _ = io.Copy(io.Discard, request.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"model":"mistral-ocr-4-0","pages":[{"index":0},{"index":1}],"usage_info":{"pages_processed":2}}`)),
				Request:    request,
			}, nil
		})},
	})
	require.NoError(t, err)
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorIs(t, err, ErrCapabilityContract)
	assert.Equal(t, int64(1), requests.Load())
}

func assertCounter(t *testing.T, counter localUnitCounter, content []byte, want int) {
	t.Helper()
	got, err := counter(bytes.NewReader(content), int64(len(content)))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func assertCounterError(t *testing.T, counter localUnitCounter, content []byte) {
	t.Helper()
	_, err := counter(bytes.NewReader(content), int64(len(content)))
	require.Error(t, err)
}

func prepareTextDocument(t *testing.T, policy Policy, candidate CandidateFormat, content []byte) *PreparedDocument {
	t.Helper()
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: candidate.MediaType,
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	return prepared
}

func textAuthorityManifest(t *testing.T, policy Policy, formatIDs ...string) CapabilityManifest {
	t.Helper()
	manifest := syntheticManifest(t, policy, true)
	for index := range manifest.Results {
		if slices.Contains(formatIDs, manifest.Results[index].FormatID) {
			manifest.Results[index].ReasonCode = ""
			manifest.Results[index].UnitBoundMethod = UnitBoundLocalExact
			manifest.Results[index].LocalUnits = 1
		}
	}
	require.NoError(t, manifest.ValidateComplete())
	return manifest
}

type failingReaderAt struct {
	data   []byte
	failAt int64
}

func (reader *failingReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset >= reader.failAt {
		return 0, errors.New("synthetic reader failure")
	}
	if offset < 0 || offset >= int64(len(reader.data)) {
		return 0, io.EOF
	}
	end := min(int64(len(reader.data)), reader.failAt)
	read := copy(buffer, reader.data[offset:end])
	if offset+int64(read) >= reader.failAt {
		return read, errors.New("synthetic reader failure")
	}
	if read < len(buffer) {
		return read, io.EOF
	}
	return read, nil
}
