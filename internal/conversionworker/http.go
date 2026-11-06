package conversionworker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/workerprotocol"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

// ServeHTTP implements only authenticated fixed uploaded-file worker routes.
func (w *Worker) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	out.Header().Set(workerprotocol.ProtocolHeader, protocol)
	if len(r.Header.Values("X-Api-Key")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), []byte(w.options.APIKey)) != 1 {
		http.Error(out, "worker authentication required", http.StatusUnauthorized)
		return
	}
	if len(r.Header.Values(workerprotocol.BackendHopHeader)) != 0 {
		http.Error(out, "worker gateways cannot be used as backends", http.StatusBadRequest)
		return
	}
	if !w.enter() {
		http.Error(out, "worker shutting down", http.StatusServiceUnavailable)
		return
	}
	defer w.active.Done()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopOwner := context.AfterFunc(w.lifetime, cancel)
	defer stopOwner()
	body := r.Body
	stopBody := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer stopBody()
	r = r.WithContext(ctx)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == workerprotocol.SubmitPath:
		w.accept(ctx, out, r)
	case r.Method == http.MethodGet && r.URL.Path == workerprotocol.ReadyPath:
		if w.errorCode() != nil {
			http.Error(out, "worker storage unavailable", http.StatusServiceUnavailable)
			return
		}
		status, _, err := w.backendRequest(ctx, http.MethodGet, workerprotocol.ReadyPath, nil, "")
		if err != nil || status != 200 {
			http.Error(out, "worker backend unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(ctx, out, 200, map[string]string{"protocol": protocol})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, workerprotocol.LookupPath):
		key := strings.TrimPrefix(r.URL.Path, workerprotocol.LookupPath)
		w.mu.Lock()
		id := w.keys[key]
		item, ok := w.jobs[id]
		w.mu.Unlock()
		if !isHex(key, 64) || !ok {
			http.NotFound(out, r)
			return
		}
		writeJSON(ctx, out, 200, w.task(item))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, workerprotocol.PollPath):
		w.readJob(ctx, out, r, false)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, workerprotocol.ResultPath):
		w.readJob(ctx, out, r, true)
	default:
		http.NotFound(out, r)
	}
}
func (w *Worker) readJob(ctx context.Context, out http.ResponseWriter, r *http.Request, result bool) {
	prefix := workerprotocol.PollPath
	if result {
		prefix = workerprotocol.ResultPath
	}
	id := strings.TrimPrefix(r.URL.Path, prefix)
	w.mu.Lock()
	item, ok := w.jobs[id]
	w.mu.Unlock()
	if !isHex(id, 32) || !ok {
		http.NotFound(out, r)
		return
	}
	if !result {
		writeJSON(ctx, out, 200, w.task(item))
		return
	}
	if item.State != "success" {
		http.Error(out, "worker result not ready", http.StatusConflict)
		return
	}
	if err := verifyPrivate(ctx, w.path(id, "result.json"), item.ResultBytes, item.ResultSHA256); err != nil {
		http.Error(out, "worker retained result failed verification", http.StatusServiceUnavailable)
		return
	}
	f, err := safefileio.OpenCurrentUserFile(w.path(id, "result.json"))
	if err != nil {
		http.Error(out, "worker result unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = f.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(f); err != nil {
		http.Error(out, "worker result unavailable", http.StatusServiceUnavailable)
		return
	}
	out.Header().Set("Content-Type", "application/json")
	_, _ = copyContext(ctx, out, f, item.ResultBytes)
}
func (w *Worker) accept(ctx context.Context, out http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	digest := r.Header.Get(workerprotocol.SourceSHA256Header)
	if len(r.Header.Values("Idempotency-Key")) != 1 || len(r.Header.Values(workerprotocol.SourceSHA256Header)) != 1 || !isHex(key, 64) || !isHex(digest, 64) {
		http.Error(out, "invalid worker submission identity", http.StatusBadRequest)
		return
	}
	if !w.uploadMu.TryLock() {
		out.Header().Set("Retry-After", "5")
		http.Error(out, "worker upload busy", http.StatusTooManyRequests)
		return
	}
	defer w.uploadMu.Unlock()
	w.mu.Lock()
	existingID := w.keys[key]
	existing, exists := w.jobs[existingID]
	if w.fault != nil {
		w.mu.Unlock()
		http.Error(out, "worker storage unavailable", http.StatusServiceUnavailable)
		return
	}
	if !exists && (len(w.jobs) >= w.options.MaxJobs || w.used+w.reserved+w.reservation() > w.options.MaxSpoolBytes) {
		w.mu.Unlock()
		out.Header().Set("Retry-After", "30")
		http.Error(out, "worker spool full", http.StatusTooManyRequests)
		return
	}
	if !exists {
		w.reserved += w.reservation()
	}
	w.mu.Unlock()
	if !exists {
		defer func() { w.mu.Lock(); w.reserved -= w.reservation(); w.mu.Unlock() }()
	}
	r.Body = http.MaxBytesReader(out, r.Body, w.options.MaxSourceBytes+(64<<10))
	defer func() { _ = r.Body.Close() }()
	stage := ""
	if !exists {
		var err error
		stage, err = os.MkdirTemp(w.options.Directory, ".incoming-")
		if err != nil {
			http.Error(out, "worker storage unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := safefileio.EnsurePrivateDir(stage); err != nil {
			_ = os.RemoveAll(stage)
			http.Error(out, "worker storage unavailable", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = os.RemoveAll(stage) }()
	}
	item, err := w.receive(ctx, r, stage)
	if err != nil {
		status := 400
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, errSourceTooLarge) {
			status = http.StatusRequestEntityTooLarge
		} else if errors.Is(err, errSourceStorage) {
			status = http.StatusInsufficientStorage
		}
		message := "invalid bounded worker upload"
		if status == http.StatusInsufficientStorage {
			message = "worker source storage unavailable"
		}
		http.Error(out, message, status)
		return
	}
	item.Key = key
	item.BackendOrigin = w.options.BackendURL
	if item.SHA256 != digest {
		http.Error(out, "worker source checksum mismatch", http.StatusBadRequest)
		return
	}
	if exists {
		if item.Filename != existing.Filename || item.MediaType != existing.MediaType || item.SHA256 != existing.SHA256 || item.Bytes != existing.Bytes || !reflect.DeepEqual(item.Fields, existing.Fields) {
			http.Error(out, "worker request key conflict", http.StatusConflict)
			return
		}
		writeJSON(ctx, out, 202, w.task(existing))
		return
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		http.Error(out, "worker storage unavailable", http.StatusServiceUnavailable)
		return
	}
	item.ID = hex.EncodeToString(id)
	item.State = "pending"
	manifest, err := json.Marshal(item)
	if err == nil {
		err = atomicfile.WriteFile(filepath.Join(stage, "manifest.json"), manifest, atomicfile.WithPrivate())
	}
	if err == nil {
		err = atomicfile.RenameNoReplace(stage, w.path(item.ID, ""))
	}
	if err == nil {
		err = atomicfile.SyncDir(w.options.Directory)
	}
	if err != nil {
		w.fail(err)
		http.Error(out, "worker storage commit uncertain; reopen required", http.StatusServiceUnavailable)
		return
	}
	w.mu.Lock()
	w.jobs[item.ID] = item
	w.keys[key] = item.ID
	w.used += w.reservation()
	w.mu.Unlock()
	writeJSON(ctx, out, 202, w.task(item))
}

var (
	errSourceTooLarge = errors.New("source exceeds bound")
	errSourceStorage  = errors.New("retained source storage failed")
)

type retainedSourceWriter struct {
	io.Writer

	err error
}

func (w *retainedSourceWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

func sourceStorageFailure(operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", errSourceStorage, operation, err)
}

func (w *Worker) receive(ctx context.Context, r *http.Request, stage string) (job, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return job{}, fmt.Errorf("receive worker input: %w", err)
	}
	var item job
	fileSeen := false
	for count := 0; ; count++ {
		if count > 6 {
			return job{}, errors.New("too many worker fields")
		}
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return job{}, fmt.Errorf("receive worker input: %w", err)
		}
		if len(part.Header.Values("Content-Transfer-Encoding")) != 0 {
			_ = part.Close()
			return job{}, errors.New("encoded worker parts are forbidden")
		}
		if part.FormName() == "files" {
			_, parameters, parseErr := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if parseErr != nil || parameters["filename"] != part.FileName() {
				_ = part.Close()
				return job{}, errors.New("worker filename must be a basename")
			}
			if fileSeen || !validFile(part.FileName(), part.Header.Get("Content-Type")) {
				_ = part.Close()
				return job{}, errors.New("invalid worker file")
			}
			fileSeen = true
			item.Filename = part.FileName()
			item.MediaType = part.Header.Get("Content-Type")
			h := sha256.New()
			var dst io.Writer = h
			var file *os.File
			var retained *retainedSourceWriter
			if stage != "" {
				file, err = safefileio.CreatePrivateFile(filepath.Join(stage, "source"))
				if err != nil {
					return job{}, fmt.Errorf("receive worker input: %w", sourceStorageFailure("create retained source", err))
				}
				retained = &retainedSourceWriter{Writer: file}
				dst = io.MultiWriter(h, retained)
			}
			item.Bytes, err = copyContext(ctx, dst, part, w.options.MaxSourceBytes)
			if err != nil && item.Bytes > w.options.MaxSourceBytes {
				err = errSourceTooLarge
			}
			if retained != nil && retained.err != nil {
				err = errors.Join(err, sourceStorageFailure("write retained source", retained.err))
			}
			if file != nil {
				if err == nil {
					if syncErr := file.Sync(); syncErr != nil {
						err = sourceStorageFailure("sync retained source", syncErr)
					}
				}
				if closeErr := file.Close(); closeErr != nil {
					err = errors.Join(err, sourceStorageFailure("close retained source", closeErr))
				}
			}
			if err != nil {
				return job{}, fmt.Errorf("receive worker input: %w", err)
			}
			item.SHA256 = hex.EncodeToString(h.Sum(nil))
		} else {
			if part.FileName() != "" {
				_ = part.Close()
				return job{}, errors.New("unexpected worker file")
			}
			value, err := io.ReadAll(io.LimitReader(part, 129))
			if err != nil || len(value) > 128 {
				return job{}, errors.New("worker field too large")
			}
			item.Fields = append(item.Fields, [2]string{part.FormName(), string(value)})
		}
		if err := part.Close(); err != nil {
			return job{}, fmt.Errorf("receive worker input: %w", err)
		}
	}
	if !fileSeen || item.Bytes <= 0 || !validSubmission(item.MediaType, item.Fields) {
		return job{}, errors.New("worker upload is incomplete")
	}
	return item, nil
}
func validFile(filename, mediaType string) bool {
	if filename == "" || !utf8.ValidString(filename) || len(filename) > 255 || strings.ContainsAny(filename, "/\\\x00\r\n") {
		return false
	}
	parsed, params, err := mime.ParseMediaType(mediaType)
	if err != nil || len(params) > 0 || parsed != mediaType {
		return false
	}
	return slices.Contains([]string{"application/pdf", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "image/png", "image/jpeg", "text/plain", "text/markdown", "audio/wav", "audio/mpeg"}, mediaType)
}
func validFields(fields [][2]string) bool {
	return reflect.DeepEqual(fields, [][2]string{{"to_formats", "md"}, {"to_formats", "json"}, {"target_type", "inbody"}}) || reflect.DeepEqual(fields, [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}}) || reflect.DeepEqual(fields, [][2]string{{"from_formats", "audio"}, {"to_formats", "json"}, {"target_type", "inbody"}})
}
func writeJSON(ctx context.Context, out http.ResponseWriter, status int, value any) {
	if ctx.Err() != nil {
		return
	}
	body, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		http.Error(out, "worker response encoding failed", http.StatusInternalServerError)
		return
	}
	out.Header().Set("Content-Type", "application/json")
	out.WriteHeader(status)
	_, _ = out.Write(body)
}

func validSubmission(mediaType string, fields [][2]string) bool {
	if !validFields(fields) {
		return false
	}
	audio := mediaType == "audio/wav" || mediaType == "audio/mpeg"
	return audio == (len(fields) == 3 && fields[0] == [2]string{"from_formats", "audio"})
}
