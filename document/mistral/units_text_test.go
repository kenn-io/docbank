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
	message := []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nbody\r\n")
	nestedMessage := []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic multipart\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=docbank\r\n\r\n--docbank\r\nContent-Type: message/rfc822\r\n\r\nFrom: nested@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\n\r\nnested\r\n--docbank--\r\n")
	textCases := []struct {
		name    string
		counter localUnitCounter
		content []byte
		want    int
	}{
		{name: "txt lines", counter: countTextLines, content: []byte("one\ntwo\r\nthree"), want: 3},
		{name: "long text line", counter: countTextLines, content: []byte(strings.Repeat("x", 100_000)), want: 1},
		{name: "markdown lines", counter: countTextLines, content: []byte("# One\n\nbody\n"), want: 3},
		{name: "csv records", counter: countCSVRecords, content: []byte("name,value\nalpha,1\nbeta,2\n"), want: 3},
		{name: "csv variable width", counter: countCSVRecords, content: []byte("a,b\nvalue\n1,2,3\n"), want: 3},
		{name: "csv quoted newline", counter: countCSVRecords, content: []byte("name,value\n\"alpha\nbeta\",1\n"), want: 2},
		{name: "json value", counter: countJSONValues, content: []byte("{\n  \"value\": [1, 2]\n}\n"), want: 1},
		{name: "json array", counter: countJSONValues, content: []byte("[1, 2, 3]"), want: 1},
		{name: "json string", counter: countJSONValues, content: []byte("\"synthetic\""), want: 1},
		{name: "json number", counter: countJSONValues, content: []byte("7319"), want: 1},
		{name: "json boolean", counter: countJSONValues, content: []byte("true"), want: 1},
		{name: "json null", counter: countJSONValues, content: []byte("null"), want: 1},
		{name: "jsonl records", counter: countJSONLines, content: []byte("{\"a\":1}\n\n[2]\ntrue"), want: 3},
		{name: "yaml documents", counter: countYAMLDocuments, content: []byte("---\na: 1\n---\nb: 2\n"), want: 2},
		{name: "go lines", counter: countTextLines, content: []byte("package probe\n\nconst value = 1\n"), want: 3},
		{name: "python lines", counter: countTextLines, content: []byte("value = 1\n# comment\n"), want: 2},
		{name: "javascript lines", counter: countTextLines, content: []byte("const value = 1;\nvalue++;\n"), want: 2},
		{name: "rst lines", counter: countTextLines, content: []byte("Heading\n=======\n\nbody\n"), want: 4},
		{name: "latex lines", counter: countTextLines, content: []byte("\\documentclass{article}\n\\begin{document}\nbody\n\\end{document}"), want: 4},
		{name: "xml document", counter: countXMLDocument, content: []byte("<?xml version=\"1.0\"?>\n<!-- synthetic -->\n<root><item/></root>\n"), want: 1},
		{name: "xml document with BOM", counter: countXMLDocument, content: append([]byte{0xef, 0xbb, 0xbf}, []byte("<root/>")...), want: 1},
		{name: "eml message", counter: countMailMessages, content: message, want: 1},
		{name: "eml nested message", counter: countMailMessages, content: nestedMessage, want: 1},
		{name: "msg message", counter: countMSGMessages, content: compoundDocument(t, "__properties_version1.0"), want: 1},
	}
	for _, test := range textCases {
		t.Run(test.name, func(t *testing.T) {
			assertCounter(t, test.counter, test.content, test.want)
		})
	}

	for _, test := range []struct {
		name    string
		counter localUnitCounter
		content []byte
	}{
		{name: "csv malformed quoting", counter: countCSVRecords, content: []byte("name,value\n\"unterminated,1\n")},
		{name: "json empty", counter: countJSONValues, content: []byte("   ")},
		{name: "json malformed", counter: countJSONValues, content: []byte("{")},
		{name: "json trailing value", counter: countJSONValues, content: []byte("1 2")},
		{name: "jsonl malformed record", counter: countJSONLines, content: []byte("{\"a\":1}\nnot-json\n")},
		{name: "jsonl multiple values", counter: countJSONLines, content: []byte("1 2\n")},
		{name: "yaml malformed", counter: countYAMLDocuments, content: []byte("---\nkey: [unterminated\n")},
		{name: "xml multiple roots", counter: countXMLDocument, content: []byte("<one/><two/>")},
		{name: "xml malformed", counter: countXMLDocument, content: []byte("<one>")},
		{name: "mail malformed", counter: countMailMessages, content: []byte("not a mail message")},
	} {
		t.Run(test.name, func(t *testing.T) { assertCounterError(t, test.counter, test.content) })
	}

	assertCounter(t, countTextLines, []byte(strings.Repeat("x\n", MaxUnits+1)), MaxUnits+1)
}

func TestTextCountersRejectEmptyShortAndUnreadableInput(t *testing.T) {
	message := compoundDocument(t, "__properties_version1.0")
	cases := []struct {
		name    string
		counter localUnitCounter
		content []byte
	}{
		{name: "txt", counter: countTextLines, content: []byte("x\n")},
		{name: "markdown", counter: countTextLines, content: []byte("x\n")},
		{name: "csv", counter: countCSVRecords, content: []byte("x,y\n")},
		{name: "json", counter: countJSONValues, content: []byte(`{"x":1}`)},
		{name: "jsonl", counter: countJSONLines, content: []byte(`{"x":1}` + "\n")},
		{name: "yaml", counter: countYAMLDocuments, content: []byte("---\nx: 1\n")},
		{name: "go", counter: countTextLines, content: []byte("package x\n")},
		{name: "python", counter: countTextLines, content: []byte("x = 1\n")},
		{name: "javascript", counter: countTextLines, content: []byte("const x = 1;\n")},
		{name: "rst", counter: countTextLines, content: []byte("x\n")},
		{name: "latex", counter: countTextLines, content: []byte("x\n")},
		{name: "xml", counter: countXMLDocument, content: []byte("<x/>\n")},
		{name: "eml", counter: countMailMessages, content: []byte("From: a@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\n\r\nbody\r\n")},
		{name: "msg", counter: countMSGMessages, content: message},
	}
	for _, test := range cases {
		t.Run(test.name+" empty", func(t *testing.T) {
			_, err := test.counter(bytes.NewReader(nil), 0)
			require.Error(t, err)
		})
		t.Run(test.name+" short", func(t *testing.T) {
			_, err := test.counter(bytes.NewReader(test.content), int64(len(test.content)+1))
			require.Error(t, err)
		})
		t.Run(test.name+" unreadable", func(t *testing.T) {
			failAt := min(int64(2), int64(len(test.content)))
			_, err := test.counter(&failingReaderAt{data: test.content, failAt: failAt}, int64(len(test.content)))
			require.Error(t, err)
		})
	}
}

func TestTextLocalCountedAuthorization(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	formatCases := []struct {
		format  string
		content []byte
	}{
		{format: "txt", content: []byte("one\ntwo\n")},
		{format: "markdown", content: []byte("# One\n\nbody\n")},
		{format: "csv", content: []byte("name,value\nalpha,1\n")},
		{format: "json", content: []byte(`{"value":"synthetic"}`)},
		{format: "jsonl", content: []byte(`{"value":"synthetic"}` + "\n")},
		{format: "yaml", content: []byte("---\nvalue: synthetic\n")},
		{format: "go", content: []byte("package synthetic\n\nconst value = 1\n")},
		{format: "python", content: []byte("value = \"synthetic\"\n")},
		{format: "javascript", content: []byte("const value = \"synthetic\";\n")},
		{format: "rst", content: []byte("Synthetic\n========\n\nbody\n")},
		{format: "latex", content: []byte(`\documentclass{article}\begin{document}synthetic\end{document}`)},
		{format: "xml", content: append([]byte{0xef, 0xbb, 0xbf}, []byte("<synthetic/>\n")...)},
		{format: "eml", content: []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nbody\r\n")},
		{format: "msg", content: compoundDocument(t, "__properties_version1.0")},
	}
	manifest := textAuthorityManifest(t, policy, textFormatIDs()...)
	for _, test := range formatCases {
		t.Run(test.format, func(t *testing.T) {
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
					encoded := strings.TrimPrefix(envelope.Document.URL, "data:"+candidate.MediaType+";base64,")
					uploaded, err = base64.StdEncoding.DecodeString(encoded)
					if err != nil {
						return nil, fmt.Errorf("decode synthetic upload: %w", err)
					}
					return &http.Response{
						StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(testSuccessfulResponse)), Request: request,
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
	manifest := textAuthorityManifest(t, policy, "txt")
	authorization, err := policy.Authorize(manifest, "txt")
	require.NoError(t, err)
	candidate, found := CandidateFormatByID("txt")
	require.True(t, found)
	prepared := prepareTextDocument(t, policy, candidate, []byte("synthetic\n"))
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

func TestTextLocalCountedAllowsProviderPageDifference(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := textAuthorityManifest(t, policy, "txt")
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == "txt" {
			manifest.Results[index].LocalUnits = 2
		}
	}
	require.NoError(t, manifest.ValidateComplete())
	authorization, err := policy.Authorize(manifest, "txt")
	require.NoError(t, err)
	candidate, found := CandidateFormatByID("txt")
	require.True(t, found)
	prepared := prepareTextDocument(t, policy, candidate, []byte("one\ntwo\n"))
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key", MaxRetries: 1,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, request.Body)
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(testSuccessfulResponse)), Request: request,
			}, nil
		})},
	})
	require.NoError(t, err)
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	assert.Equal(t, 2, prepared.localUnits)
	assert.Equal(t, 1, result.UnitsProcessed)
}

func TestTextLocalCountedRejectsProviderOverflow(t *testing.T) {
	policy := testPolicy(t, 1<<20, MaxUnits)
	snapshot := preparedSnapshot{size: 1, localUnits: 1}
	page := wirePage{indexPresent: true}
	t.Run("pages", func(t *testing.T) {
		pages := make([]wirePage, MaxUnits+1)
		for index := range pages {
			pages[index] = page
			pages[index].Index = index
		}
		err := validateWireResult(wireResult{Model: defaultModel, Pages: pages, UsageInfo: &wireUsage{
			PagesProcessed: MaxUnits + 1, pagesProcessedPresent: true,
		}}, defaultModel, snapshot, UnitBoundLocalCounted, policy.values.MaxUnits)
		require.ErrorIs(t, err, ErrCapabilityContract)
	})
	t.Run("usage", func(t *testing.T) {
		err := validateWireResult(wireResult{Model: defaultModel, Pages: []wirePage{page}, UsageInfo: &wireUsage{
			PagesProcessed: MaxUnits + 1, pagesProcessedPresent: true,
		}}, defaultModel, snapshot, UnitBoundLocalCounted, policy.values.MaxUnits)
		require.ErrorIs(t, err, ErrCapabilityContract)
	})
}

func textFormatIDs() []string {
	return []string{"txt", "markdown", "csv", "json", "jsonl", "yaml", "go", "python", "javascript", "rst", "latex", "xml", "eml", "msg"}
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
			manifest.Results[index].UnitBoundMethod = UnitBoundLocalCounted
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
