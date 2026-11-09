package conversionworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pack"
	"hegel.dev/go/hegel"
)

func TestWorkerRejectsNonUTF8Identity(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Lists(hegel.Integers[byte](0, 255)))
		raw = raw[:min(len(raw), 255)]
		if utf8.Valid(raw) {
			return
		}
		o := workerOptions(t)
		w := openWorker(t, o)
		out := submitNamed(t, w, strings.Repeat("a", 64), string(raw), "application/pdf", []byte("x"), [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}})
		require.Equal(ht, 400, out.Code)
		require.NoError(ht, w.Close(t.Context()))
		w, err := Open(t.Context(), o)
		require.NoError(ht, err)
		require.NoError(ht, w.Close(t.Context()))
	})
}
func TestWorkerReconcilesAtomicCrashStaging(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers[uint](0, ^uint(0)))
		count = min(count, 8)
		o := workerOptions(t)
		w := openWorker(t, o)
		id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
		require.NoError(ht, w.Close(t.Context()))
		var paths []string
		for n := range count {
			path := filepath.Join(o.Directory, id, ".result.json.tmp-"+strings.Repeat("x", int(n)+1))
			require.NoError(ht, atomicfile.WriteFile(path, bytes.Repeat([]byte{1}, 4096), atomicfile.WithPrivate()))
			paths = append(paths, path)
		}
		w, err := Open(t.Context(), o)
		require.NoError(ht, err)
		for _, path := range paths {
			_, err := os.Stat(path)
			require.ErrorIs(ht, err, os.ErrNotExist)
		}
		require.NoError(ht, w.Close(t.Context()))
	})
}
func TestWorkerBudgetExhaustionDoesNotNeedBackend(t *testing.T) {
	o := workerOptions(t)
	var offline atomic.Bool
	var sends atomic.Int32
	o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			sends.Add(1)
			return backendResponse(r, 503, `{}`), nil
		}
		if offline.Load() {
			return nil, errors.New("synthetic offline")
		}
		return backendResponse(r, 200, `{}`), nil
	})}
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	for range 3 {
		_, err := w.RunOnce(t.Context())
		require.NoError(t, err)
	}
	offline.Store(true)
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, int32(3), sends.Load())
	require.Contains(t, getRequest(t.Context(), w, "/v1/status/poll/"+id).Body.String(), "failure")
}
func TestWorkerSpoolCreationSyncsEveryNewParent(t *testing.T) {
	o := workerOptions(t)
	parent := filepath.Dir(o.Directory)
	o.Directory = filepath.Join(parent, "level-a", "level-b", "spool")
	old := pack.SyncDir
	var synced []string
	pack.SyncDir = func(path string) error { synced = append(synced, path); return old(path) }
	t.Cleanup(func() { pack.SyncDir = old })
	w := openWorker(t, o)
	require.Contains(t, synced, parent)
	require.Contains(t, synced, filepath.Join(parent, "level-a"))
	require.Contains(t, synced, filepath.Join(parent, "level-a", "level-b"))
	require.NoError(t, w.Close(t.Context()))
}
func TestWorkerRejectsUnsafeRetainedProtocolState(t *testing.T) {
	for _, change := range []func([]byte) []byte{
		func(data []byte) []byte { return append(data, []byte("\n{}")...) },
		func(data []byte) []byte {
			var item job
			require.NoError(t, json.Unmarshal(data, &item))
			item.BackendID = "../ready"
			item.State = "started"
			item.Submissions = 1
			out, err := json.Marshal(item)
			require.NoError(t, err)
			return out
		},
	} {
		o := workerOptions(t)
		w := openWorker(t, o)
		id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
		require.NoError(t, w.Close(t.Context()))
		path := filepath.Join(o.Directory, id, "manifest.json")
		original, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, change(original), 0600))
		next, err := Open(t.Context(), o)
		if next != nil {
			require.NoError(t, next.Close(t.Context()))
		}
		require.Error(t, err)
		require.NoError(t, os.WriteFile(path, original, 0600))
		next, err = Open(t.Context(), o)
		require.NoError(t, err)
		require.NoError(t, next.Close(t.Context()))
	}
}
func TestWorkerRejectsMismatchedAudioMode(t *testing.T) {
	for _, candidate := range []struct {
		filename, mediaType string
		fields              [][2]string
	}{
		{"synthetic.pdf", "application/pdf", [][2]string{{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"}}},
		{"synthetic.wav", "audio/wav", [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}},
	} {
		w := openWorker(t, workerOptions(t))
		out := submitNamed(t, w, strings.Repeat("a", 64), candidate.filename, candidate.mediaType, []byte("x"), candidate.fields)
		require.Equal(t, 400, out.Code)
	}
}
func TestWorkerRejectsEncodedFileTransfer(t *testing.T) {
	w := openWorker(t, workerOptions(t))
	body := "--synthetic\r\nContent-Disposition: form-data; name=\"files\"; filename=\"synthetic.pdf\"\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nA\r\n--synthetic\r\nContent-Disposition: form-data; name=\"to_formats\"\r\n\r\njson\r\n--synthetic\r\nContent-Disposition: form-data; name=\"target_type\"\r\n\r\ninbody\r\n--synthetic--\r\n"
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/convert/file/async", strings.NewReader(body))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=synthetic")
	r.Header.Set("X-Api-Key", "synthetic-worker-key")
	r.Header.Set("Idempotency-Key", strings.Repeat("a", 64))
	sum := sha256.Sum256([]byte("A"))
	r.Header.Set("X-Docbank-Source-Sha256", hex.EncodeToString(sum[:]))
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	require.Equal(t, 400, out.Code)
}

type observedBody struct {
	once   sync.Once
	closed chan struct{}
}

func (b *observedBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *observedBody) Close() error             { b.once.Do(func() { close(b.closed) }); return nil }
func TestWorkerAlreadyCanceledEntryIsRaceFree(t *testing.T) {
	w := openWorker(t, workerOptions(t))
	for range 100 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		body := &observedBody{closed: make(chan struct{})}
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/convert/file/async", body)
		r.Header.Set("X-Api-Key", "synthetic-worker-key")
		r.Header.Set("Idempotency-Key", strings.Repeat("a", 64))
		r.Header.Set("X-Docbank-Source-Sha256", strings.Repeat("b", 64))
		r.Header.Set("Content-Type", "multipart/form-data; boundary=synthetic")
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		require.NotEqual(t, 202, out.Code)
		<-body.closed
	}
}

func TestWorkerRejectsAmbiguousBackendOrigins(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:5001?", "http://127.0.0.1:5001#"} {
		o := workerOptions(t)
		o.BackendURL = origin
		w, err := Open(t.Context(), o)
		if w != nil {
			require.NoError(t, w.Close(t.Context()))
		}
		require.Error(t, err, origin)
	}
}
func TestWorkerSpoolBindsBackendOrigin(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	require.NoError(t, w.Close(t.Context()))
	o.BackendURL = "http://127.0.0.1:5003"
	next, err := Open(t.Context(), o)
	if next != nil {
		require.NoError(t, next.Close(t.Context()))
	}
	require.Error(t, err)
}

func TestWorkerDoesNotServeCorruptedRetainedResult(t *testing.T) {
	o := workerOptions(t)
	w := openWorker(t, o)
	id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
	_, err := w.RunOnce(t.Context())
	require.NoError(t, err)
	path := filepath.Join(o.Directory, id, "result.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[0] = 'x'
	require.NoError(t, os.WriteFile(path, data, 0600))
	require.Equal(t, 503, getRequest(t.Context(), w, "/v1/result/"+id).Code)
}

func TestWorkerNumericBoundsHoldAcrossConfiguration(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		field := hegel.Draw(ht, hegel.Integers(0, 3))
		o := workerOptions(t)
		var maximum, minimum int64
		switch field {
		case 0:
			o.MaxSourceBytes = value
			minimum = 1
			maximum = 100 << 20
		case 1:
			o.MaxResultBytes = value
			minimum = 1
			maximum = 128 << 20
		case 2:
			o.MaxSpoolBytes = value
			minimum = 4096
			maximum = 40 << 30
		case 3:
			o.PollInterval = time.Duration(value)
			minimum = 1
			maximum = int64(time.Minute)
		}
		w, err := Open(t.Context(), o)
		valid := value == 0 || value >= minimum && value <= maximum
		if !valid {
			if w != nil {
				require.NoError(ht, w.Close(t.Context()))
			}
			require.Error(ht, err)
			_, statErr := os.Stat(o.Directory)
			require.ErrorIs(ht, statErr, os.ErrNotExist)
			return
		}
		require.NoError(ht, err)
		require.NoError(ht, w.Close(t.Context()))
	})
}
func TestWorkerJobLimitConfiguration(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		limit := hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt))
		o := workerOptions(t)
		o.MaxJobs = limit
		w, err := Open(t.Context(), o)
		if limit < 0 || limit > 128 {
			if w != nil {
				require.NoError(ht, w.Close(t.Context()))
			}
			require.Error(ht, err)
			return
		}
		require.NoError(ht, err)
		require.NoError(ht, w.Close(t.Context()))
	})
}
func TestWorkerBackendReceivesExactGeneratedOriginalAfterRestart(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		source := hegel.Draw(ht, hegel.Lists(hegel.Integers[byte](0, 255)))
		source = source[:min(len(source), 512)]
		kind := hegel.Draw(ht, hegel.Integers(0, 8))
		mediaTypes := []string{"application/pdf", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "image/png", "image/jpeg", "text/plain", "text/markdown", "audio/wav", "audio/mpeg"}
		names := []string{"synthetic.pdf", "synthetic.pptx", "synthetic.xlsx", "synthetic.png", "synthetic.jpg", "synthetic.txt", "synthetic.md", "synthetic.wav", "synthetic.mp3"}
		fields := [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}
		if kind >= 7 {
			fields = [][2]string{{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"}}
		}
		o := workerOptions(t)
		normal := o.Client.Transport
		var received []byte
		var filename, mediaType string
		o.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil {
					return nil, fmt.Errorf("read synthetic backend input: %w", err)
				}
				reader := multipart.NewReader(r.Body, params["boundary"])
				part, err := reader.NextRawPart()
				if err != nil {
					return nil, fmt.Errorf("read synthetic backend input: %w", err)
				}
				filename = part.FileName()
				mediaType = part.Header.Get("Content-Type")
				received, err = io.ReadAll(part)
				if err != nil {
					return nil, fmt.Errorf("read synthetic backend input: %w", err)
				}
				for {
					part, err = reader.NextRawPart()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return nil, fmt.Errorf("read synthetic backend input: %w", err)
					}
					_, _ = io.Copy(io.Discard, part)
				}
				return backendResponse(r, 202, `{"task_id":"backend-1","task_status":"pending"}`), nil
			}
			return normal.RoundTrip(r)
		})}
		w := openWorker(t, o)
		out := submitNamed(t, w, strings.Repeat("a", 64), names[kind], mediaTypes[kind], source, fields)
		if len(source) == 0 {
			require.Equal(ht, 400, out.Code)
			require.NoError(ht, w.Close(t.Context()))
			return
		}
		require.Equal(ht, 202, out.Code)
		require.NoError(ht, w.Close(t.Context()))
		w, err := Open(t.Context(), o)
		require.NoError(ht, err)
		defer func() { require.NoError(t, w.Close(context.WithoutCancel(t.Context()))) }()
		_, err = w.RunOnce(t.Context())
		require.NoError(ht, err)
		require.Equal(ht, source, received)
		require.Equal(ht, names[kind], filename)
		require.Equal(ht, mediaTypes[kind], mediaType)
	})
}
