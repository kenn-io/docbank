package conversionworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
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

func TestWorkerRejectsUnsafeBackendOrigins(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:5001?", "http://127.0.0.1:5001#", "http://203.0.113.1:5001"} {
		o := workerOptions(t)
		o.BackendURL = origin
		w, err := Open(t.Context(), o)
		if w != nil {
			require.NoError(t, w.Close(t.Context()))
		}
		require.Error(t, err, origin)
		if origin == "http://203.0.113.1:5001" {
			require.ErrorContains(t, err, "worker backend must be loopback")
		}
	}
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
		field := hegel.Draw(ht, hegel.Integers(0, 4))
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
		case 4:
			o.MaxJobs = int(value)
			minimum = 1
			maximum = 128
		}
		w, err := Open(t.Context(), o)
		valid := value >= minimum && value <= maximum || field == 3 && value == 0
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
func TestWorkerBackendReceivesExactGeneratedOriginalAfterRestart(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		source := hegel.Draw(ht, hegel.Lists(hegel.Integers[byte](0, 255)))
		source = source[:min(len(source), 512)]
		kind := hegel.Draw(ht, hegel.Integers(0, 4))
		mediaTypes := []string{"application/pdf", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "image/png", "image/jpeg"}
		names := []string{"synthetic.pdf", "synthetic.pptx", "synthetic.xlsx", "synthetic.png", "synthetic.jpg"}
		fields := [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}
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
				return backendResponse(r, 202, `{"task_id":"backend-1","task_status":"pending","task_type":"convert"}`), nil
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
		key := strings.Repeat("a", 64)
		id := taskID(t, out)
		found := getRequest(t.Context(), w, "/v1/jobs/by-key/"+key)
		require.Equal(ht, http.StatusOK, found.Code)
		require.Contains(ht, found.Body.String(), id)
		changed := append([]byte(nil), source...)
		changed[0] ^= 1
		require.Equal(ht, http.StatusConflict, submitNamed(t, w, key, names[kind], mediaTypes[kind], changed, fields).Code)
		require.Equal(ht, out.Body.String(), submitNamed(t, w, strings.Repeat("a", 64), names[kind], mediaTypes[kind], source, fields).Body.String())
		_, err = w.RunOnce(t.Context())
		require.NoError(ht, err)
		require.Equal(ht, source, received)
		require.Equal(ht, names[kind], filename)
		require.Equal(ht, mediaTypes[kind], mediaType)
	})
}

func TestWorkerRejectsInvalidUploads(t *testing.T) {
	o := workerOptions(t)
	o.MaxSourceBytes, o.MaxSpoolBytes = 32<<20, 128<<20
	w := openWorker(t, o)
	wav := append([]byte("RIFF\x64\x1f\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00\x40\x1f\x00\x00\x01\x00\x08\x00data\x40\x1f\x00\x00"), make([]byte, 8000)...)
	for _, candidate := range []struct {
		filename, mediaType string
		fields              [][2]string
	}{
		{"synthetic.txt", "text/plain", [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}}},
		{"synthetic.pdf", "application/pdf", [][2]string{{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"}}},
		{"synthetic.wav", "audio/wav", [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}},
	} {
		source := []byte("x")
		if candidate.mediaType == "audio/wav" {
			source = wav
		}
		out := submitNamed(t, w, strings.Repeat("a", 64), candidate.filename, candidate.mediaType, source, candidate.fields)
		require.Equal(t, 400, out.Code)
	}
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
	fields := [][2]string{{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"}}
	for i, test := range []struct{ seconds, status int }{{3600, 202}, {3601, 400}} {
		data := make([]byte, 44+8000*test.seconds)
		copy(data, "RIFF")
		binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
		copy(data[8:], "WAVEfmt ")
		binary.LittleEndian.PutUint32(data[16:20], 16)
		binary.LittleEndian.PutUint16(data[20:22], 1)
		binary.LittleEndian.PutUint16(data[22:24], 1)
		binary.LittleEndian.PutUint32(data[24:28], 8000)
		binary.LittleEndian.PutUint32(data[28:32], 8000)
		binary.LittleEndian.PutUint16(data[32:34], 1)
		binary.LittleEndian.PutUint16(data[34:36], 8)
		copy(data[36:], "data")
		binary.LittleEndian.PutUint32(data[40:44], uint32(len(data)-44))
		response := submitNamed(t, w, strings.Repeat(string(rune('b'+i)), 64), "audio.wav", "audio/wav", data, fields)
		require.Equal(t, test.status, response.Code, response.Body.String())
	}
}

func TestWorkerRejectsInvalidRetainedState(t *testing.T) {
	for _, damage := range []string{"version", "source", "extra JSON", "unsafe task", "backend origin"} {
		t.Run(damage, func(t *testing.T) {
			o := workerOptions(t)
			w := openWorker(t, o)
			id := taskID(t, submitRequest(t, w, strings.Repeat("a", 64), []byte("source")))
			require.NoError(t, w.Close(t.Context()))
			manifest := filepath.Join(o.Directory, id, "manifest.json")
			original, err := os.ReadFile(manifest)
			require.NoError(t, err)
			switch damage {
			case "source":
				require.NoError(t, os.WriteFile(filepath.Join(o.Directory, id, "source"), []byte("tampered"), 0600))
			case "backend origin":
				o.BackendURL = "http://127.0.0.1:5003"
			case "extra JSON":
				require.NoError(t, os.WriteFile(manifest, append(original, []byte("\n{}")...), 0600))
			default:
				var item job
				require.NoError(t, json.Unmarshal(original, &item))
				if damage == "version" {
					item.Version = 2
				} else {
					item.BackendID, item.State, item.Submissions = "../ready", "started", 1
				}
				data, err := json.Marshal(item)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(manifest, data, 0600))
			}
			next, err := Open(t.Context(), o)
			require.Error(t, err)
			require.Nil(t, next)
			if damage == "extra JSON" || damage == "unsafe task" {
				require.NoError(t, os.WriteFile(manifest, original, 0600))
				next, err = Open(t.Context(), o)
				require.NoError(t, err)
				require.NoError(t, next.Close(t.Context()))
			}
		})
	}
}
