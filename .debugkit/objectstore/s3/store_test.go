package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

type fixture struct {
	mu                                                                          sync.Mutex
	objects                                                                     map[string][]byte
	parts                                                                       map[int][]byte
	sha, crc, scope                                                             string
	revision                                                                    string
	afterComplete                                                               func()
	omitCompletionRevision                                                      bool
	lossCompleteHeaders                                                         bool
	retryDenied                                                                 bool
	lossPut, lossComplete, denyHead, truncate, failPart, omitPutChecksum        bool
	omitChecksums, omitRevision, denyGet, wrongLength, wrongSHA, dropOnConflict bool
	putError                                                                    int
	afterPut                                                                    func()
	puts, heads, aborts, deletes                                                int
	requests                                                                    int
	missingBucket                                                               bool
}

func newFixture(t *testing.T) (*fixture, *Store) {
	t.Helper()
	f := &fixture{objects: map[string][]byte{}, parts: map[int][]byte{}, scope: "FULL_OBJECT", revision: "\"revision\""}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	store, err := New(t.Context(), Config{Bucket: "bucket", Prefix: "namespace", Region: "us-east-1", Endpoint: server.URL, ForcePathStyle: true, AllowInsecureTransport: true, Credentials: credentials.NewStaticCredentialsProvider("test", "test-secret", ""), PartBytes: 5 << 20, PageSize: 1, AllowReadback: true})
	require.NoError(t, err)
	return f, store
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	q := r.URL.Query()
	fail := func(code int, name string) {
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", name)
	}
	if f.missingBucket {
		fail(http.StatusNotFound, "NoSuchBucket")
		return
	}
	lose := func() {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		keys := []string{}
		for key := range f.objects {
			if strings.HasPrefix(key, q.Get("prefix")) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		start, _ := strconv.Atoi(strings.TrimPrefix(q.Get("continuation-token"), "page/%2B+"))
		end := min(start+1, len(keys))
		_, _ = io.WriteString(w, "<ListBucketResult><EncodingType>url</EncodingType>")
		for _, k := range keys[start:end] {
			wireKey := k
			if q.Get("encoding-type") == "url" {
				wireKey = url.QueryEscape(k)
			}
			_, _ = fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><ETag>&quot;revision&quot;</ETag></Contents>", wireKey, len(f.objects[k]))
		}
		if end < len(keys) {
			_, _ = fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextContinuationToken>page/%%2B+%d</NextContinuationToken>", end)
		}
		_, _ = io.WriteString(w, "</ListBucketResult>")
	case r.Method == http.MethodPost && q.Has("uploads"):
		if r.Header.Get("X-Amz-Checksum-Algorithm") != "CRC32C" || r.Header.Get("X-Amz-Checksum-Type") != "FULL_OBJECT" {
			fail(400, "InvalidRequest")
			return
		}
		f.parts = map[int][]byte{}
		_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>")
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		if f.failPart {
			fail(400, "InvalidPart")
			return
		}
		data, _ := io.ReadAll(r.Body)
		part, _ := strconv.Atoi(q.Get("partNumber"))
		f.parts[part] = data
		w.Header().Set("ETag", fmt.Sprintf("\"part-%d\"", part))
		w.Header().Set("X-Amz-Checksum-Crc32c", crcValue(data))
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		if f.putError != 0 {
			fail(f.putError, "AccessDenied")
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			fail(400, "InvalidRequest")
			return
		}
		if _, ok := f.objects[key]; ok {
			if f.dropOnConflict {
				delete(f.objects, key)
			}
			fail(412, "PreconditionFailed")
			return
		}
		var doc struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				CRC    string `xml:"ChecksumCRC32C"`
			} `xml:"Part"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&doc)
		data := []byte{}
		for _, p := range doc.Parts {
			if p.CRC != crcValue(f.parts[p.Number]) {
				fail(400, "BadDigest")
				return
			}
			data = append(data, f.parts[p.Number]...)
		}
		if r.Header.Get("X-Amz-Checksum-Crc32c") != crcValue(data) {
			fail(400, "BadDigest")
			return
		}
		f.objects[key] = data
		f.crc = crcValue(data)
		if f.lossCompleteHeaders {
			f.lossCompleteHeaders = false
			if f.retryDenied {
				f.putError = http.StatusForbidden
			}
			w.Header().Set("Content-Length", "1024")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<CompleteMultipartUploadResult>")
			w.(http.Flusher).Flush()
			lose()
			return
		}
		if f.lossComplete {
			f.lossComplete = false
			lose()
			return
		}
		revision := f.revision
		if f.omitCompletionRevision {
			revision = ""
		}
		_, _ = fmt.Fprintf(w, "<CompleteMultipartUploadResult><ETag>%s</ETag><ChecksumCRC32C>%s</ChecksumCRC32C><ChecksumType>%s</ChecksumType></CompleteMultipartUploadResult>", revision, f.crc, f.scope)
		if f.afterComplete != nil {
			f.afterComplete()
		}
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		f.aborts++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.puts++
		if f.putError != 0 {
			fail(f.putError, "AccessDenied")
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			fail(400, "InvalidRequest")
			return
		}
		if _, ok := f.objects[key]; ok {
			if f.dropOnConflict {
				delete(f.objects, key)
			}
			fail(412, "PreconditionFailed")
			return
		}
		data, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Amz-Checksum-Sha256") != shaValue(data) {
			fail(400, "BadDigest")
			return
		}
		f.objects[key] = data
		f.sha = shaValue(data)
		if f.afterPut != nil {
			f.afterPut()
		}
		if f.lossPut {
			f.lossPut = false
			if f.retryDenied {
				f.putError = http.StatusForbidden
			}
			lose()
			return
		}
		w.Header().Set("ETag", f.revision)
		if !f.omitChecksums && !f.omitPutChecksum {
			w.Header().Set("X-Amz-Checksum-Sha256", f.sha)
		}
	case r.Method == http.MethodHead:
		f.heads++
		if f.denyHead {
			fail(403, "AccessDenied")
			return
		}
		data, ok := f.objects[key]
		if !ok {
			fail(404, "NotFound")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if f.wrongLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)+1))
		}
		if !f.omitRevision {
			w.Header().Set("ETag", f.revision)
		}
		w.Header().Set("X-Amz-Checksum-Type", f.scope)
		if f.sha != "" && !f.omitChecksums {
			w.Header().Set("X-Amz-Checksum-Sha256", f.sha)
			if f.wrongSHA {
				w.Header().Set("X-Amz-Checksum-Sha256", shaValue([]byte("wrong")))
			}
		}
		if f.crc != "" && !f.omitChecksums {
			w.Header().Set("X-Amz-Checksum-Crc32c", f.crc)
		}
	case r.Method == http.MethodGet:
		if f.denyGet {
			fail(403, "AccessDenied")
			return
		}
		data, ok := f.objects[key]
		if !ok {
			fail(404, "NoSuchKey")
			return
		}
		if rev := r.Header.Get("If-Match"); rev != "" && rev != f.revision {
			fail(412, "PreconditionFailed")
			return
		}
		w.Header().Set("ETag", f.revision)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if rng := r.Header.Get("Range"); rng != "" {
			var start, end int
			_, _ = fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			if start < 0 || end >= len(data) {
				fail(416, "InvalidRange")
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			data = data[start : end+1]
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("ETag", f.revision)
			w.WriteHeader(http.StatusPartialContent)
		}
		if f.truncate && len(data) > 0 {
			data = data[:len(data)-1]
		}
		_, _ = w.Write(data)
	case r.Method == http.MethodDelete:
		f.deletes++
		if rev := r.Header.Get("If-Match"); rev != "" && rev != f.revision {
			fail(412, "PreconditionFailed")
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(400, "InvalidRequest")
	}
}

func shaValue(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func crcValue(data []byte) string {
	sum := binary.BigEndian.AppendUint32(nil, crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	return base64.StdEncoding.EncodeToString(sum)
}

func TestConditionalPublication(t *testing.T) {
	f, s := newFixture(t)
	data := []byte("original")
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, "provider-sha256", receipt.Verification)
	receipt, err = s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, sum, receipt.SHA256)
	different := []byte("different")
	receipt, err = s.Publish(t.Context(), "key", bytes.NewReader(different), int64(len(different)), sha256.Sum256(different))
	require.ErrorIs(t, err, objectstore.ErrExists)
	assert.Zero(t, receipt)
	assert.Equal(t, sum, sha256.Sum256(f.objects["namespace/key"]))
	before := f.puts
	receipt, err = s.Publish(t.Context(), "invalid", bytes.NewReader(data), int64(len(data)), sha256.Sum256(different))
	require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
	assert.Zero(t, receipt)
	assert.Equal(t, before, f.puts)
}

func TestRetry412AfterStoredPut(t *testing.T) {
	f, s := newFixture(t)
	f.lossPut = true
	data := []byte("stored before lost response")
	sum := sha256.Sum256(data)
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sum)
	require.NoError(t, err)
	assert.Equal(t, sum, receipt.SHA256)
	assert.GreaterOrEqual(t, f.puts, 2)
	assert.Positive(t, f.heads)
}

func TestUncertainInspection(t *testing.T) {
	f, s := newFixture(t)
	f.lossPut = true
	f.denyHead = true
	data := []byte("stored")
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	assert.Contains(t, err.Error(), "403")
	assert.Equal(t, data, f.objects["namespace/key"])
}

func TestRangeTruncated(t *testing.T) {
	f, s := newFixture(t)
	f.objects["namespace/key"] = []byte("abcdef")
	f.truncate = true
	body, err := s.OpenRange(t.Context(), "key", "\"revision\"", 1, 4)
	require.NoError(t, err)
	_, err = io.ReadAll(body)
	require.Error(t, err)
	require.NoError(t, body.Close())
}

func TestInventoryForeignKeys(t *testing.T) {
	f, s := newFixture(t)
	f.objects["namespace/foreign-object"] = []byte("foreign")
	f.objects["namespace/../foreign"] = nil
	page, err := s.ListPage(t.Context(), "", "", 1)
	require.NoError(t, err)
	require.Len(t, page.Objects, 1)
	require.NotEmpty(t, page.NextToken)
	next, err := s.ListPage(t.Context(), "", page.NextToken, 1)
	require.NoError(t, err)
	assert.Contains(t, []string{page.Objects[0].Key, next.Objects[0].Key}, "foreign-object")
	assert.Zero(t, f.deletes)
}

func TestInventoryEncodedKeysAcrossPages(t *testing.T) {
	f, s := newFixture(t)
	want := []string{"foreign/\x01+entry", "foreign/literal%2F+name/nested", "foreign/slash/name"}
	for _, key := range want {
		_, err := s.Publish(t.Context(), key, bytes.NewReader([]byte("stored")), 6, sha256.Sum256([]byte("stored")))
		require.NoError(t, err)
	}
	var got []string
	token := ""
	for i := range want {
		page, err := s.ListPage(t.Context(), "foreign/", token, 1)
		require.NoError(t, err)
		require.Len(t, page.Objects, 1)
		got = append(got, page.Objects[0].Key)
		if i+1 < len(want) {
			assert.Equal(t, fmt.Sprintf("page/%%2B+%d", i+1), page.NextToken)
		} else {
			assert.Empty(t, page.NextToken)
		}
		token = page.NextToken
	}
	assert.Equal(t, want, got)
	assert.Zero(t, f.deletes)
}

func TestRangeAndDeleteRevision(t *testing.T) {
	f, s := newFixture(t)
	f.objects["namespace/key"] = []byte("abcdef")
	body, err := s.OpenRange(t.Context(), "key", "\"revision\"", 1, 3)
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "bcd", string(data))
	require.NoError(t, body.Close())
	_, err = s.OpenRange(t.Context(), "key", "stale", 0, 1)
	require.ErrorIs(t, err, objectstore.ErrRevisionChanged)
	require.ErrorIs(t, s.Delete(t.Context(), "key", "stale"), objectstore.ErrRevisionChanged)
	assert.Contains(t, f.objects, "namespace/key")
	require.NoError(t, s.Delete(t.Context(), "key", "\"revision\""))
	require.NoError(t, s.Delete(t.Context(), "key", ""))
	_, err = s.Inspect(t.Context(), "key")
	require.ErrorIs(t, err, objectstore.ErrNotFound)
}

func TestCanceledBeforeDispatch(t *testing.T) {
	f, s := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, err := s.Publish(ctx, "key", bytes.NewReader(nil), 0, sha256.Sum256(nil))
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	assert.Zero(t, f.puts)
}

func TestInvalidKeysAndConfiguration(t *testing.T) {
	f, s := newFixture(t)
	for _, key := range []string{"../key", "/absolute", "a/../key", "a//key", "a\\key", "a:key"} {
		t.Run(key, func(t *testing.T) {
			_, err := s.Publish(t.Context(), key, bytes.NewReader(nil), 0, sha256.Sum256(nil))
			require.Error(t, err)
			_, err = s.Inspect(t.Context(), key)
			require.Error(t, err)
			_, err = s.OpenRange(t.Context(), key, "", 0, 1)
			require.Error(t, err)
			require.Error(t, s.Delete(t.Context(), key, ""))
			_, err = s.ListPage(t.Context(), key, "", 1)
			require.Error(t, err)
		})
	}
	assert.Zero(t, f.puts)
	assert.Zero(t, f.heads)
	assert.Zero(t, f.deletes)
	for _, cfg := range []Config{
		{Bucket: "bucket"},
		{Bucket: "bucket", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), Endpoint: "http://example.com"},
		{Bucket: "bucket", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), PartBytes: 1},
		{Bucket: "bucket", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), PageSize: 1001},
	} {
		_, err := New(t.Context(), cfg)
		require.Error(t, err)
	}
}

func TestRangeCancellation(t *testing.T) {
	f, s := newFixture(t)
	f.objects["namespace/key"] = []byte("abcdef")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body, err := s.OpenRange(ctx, "key", "\"revision\"", 0, 6)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(body)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, body.Close())
}

func TestUnsupportedConditionalOperation(t *testing.T) {
	f, s := newFixture(t)
	f.putError = http.StatusNotImplemented
	data := []byte("bytes")
	receipt, err := s.Publish(t.Context(), "key", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.Error(t, err)
	require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	assert.Empty(t, f.objects)
	assert.Zero(t, f.heads)
}
