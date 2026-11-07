package conversionworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document/workerprotocol"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

// Run performs one bounded conversion pass at a time until canceled.
func (w *Worker) Run(ctx context.Context) error {
	if !w.enter() {
		return errors.New("worker closed")
	}
	defer w.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopOwner := context.AfterFunc(w.lifetime, cancel)
	defer stopOwner()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := w.RunOnce(ctx)
		if err != nil {
			return err
		}
		timer := time.NewTimer(w.options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOnce advances one retained job. The caller controls retry timing.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if !w.enter() {
		return false, errors.New("worker closed")
	}
	defer w.active.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopOwner := context.AfterFunc(w.lifetime, cancel)
	defer stopOwner()
	if !w.runMu.TryLock() {
		return false, errors.New("worker runner already active")
	}
	defer w.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := w.errorCode(); err != nil {
		return false, err
	}
	w.mu.Lock()
	ids := make([]string, 0, len(w.jobs))
	for id := range w.jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var item job
	found := false
	// Keep polling and retaining known work before admitting another backend
	// submission. Random issued IDs must not let new uploads displace it.
	for _, id := range ids {
		j := w.jobs[id]
		if j.BackendID != "" && (j.State == "pending" || j.State == "started") {
			item, found = j, true
			break
		}
	}
	if !found {
		for _, id := range ids {
			j := w.jobs[id]
			if j.State == "pending" || j.State == "started" {
				item = j
				found = true
				break
			}
		}
	}
	w.mu.Unlock()
	if !found {
		return false, nil
	}
	if item.BackendID == "" {
		if item.Submissions >= 3 {
			item.State = "failure"
			return true, w.save(ctx, item)
		}
		status, _, err := w.backendRequest(ctx, http.MethodGet, workerprotocol.ReadyPath, nil, "")
		if err != nil || status != 200 {
			return true, nil
		}
		item.Submissions++
		if err := w.save(ctx, item); err != nil {
			return true, err
		}
		status, body, err := w.submitBackend(ctx, item)
		if status == http.StatusTooManyRequests {
			if errors.Is(err, errBackendSourceRead) {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				return true, nil
			}
			item.Submissions--
			// Persist the confirmed refusal's refund even if cancellation raced
			// with reading its response body, then propagate cancellation.
			refundCtx, cancelRefund := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			refundErr := w.save(refundCtx, item)
			cancelRefund()
			if refundErr != nil {
				return true, errors.Join(ctx.Err(), refundErr)
			}
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return true, nil
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if err != nil || status >= 500 || status == 408 {
			return true, nil
		}
		if status != 200 && status != 202 {
			item.State = "failure"
			return true, w.save(ctx, item)
		}
		var task struct {
			ID    string `json:"task_id"`
			State string `json:"task_status"`
		}
		if json.Unmarshal(body, &task) != nil || !validBackendID(task.ID) {
			return true, nil
		}
		item.BackendID = task.ID
		item.State = "started"
		if err := w.save(ctx, item); err != nil {
			return true, err
		}
	}
	status, body, err := w.backendRequest(ctx, http.MethodGet, workerprotocol.PollPath+item.BackendID, nil, "")
	if err != nil {
		if errors.Is(err, errBackendOutput) {
			item.State = "failure"
			return true, w.save(ctx, item)
		}
		return true, nil
	}
	if status == 404 {
		item.BackendID = ""
		item.State = "pending"
		return true, w.save(ctx, item)
	}
	if status != 200 {
		return true, nil
	}
	var task struct {
		ID    string `json:"task_id"`
		State string `json:"task_status"`
	}
	if json.Unmarshal(body, &task) != nil || task.ID != item.BackendID {
		item.State = "failure"
		return true, w.save(ctx, item)
	}
	if task.State == "pending" || task.State == "started" {
		return true, nil
	}
	if task.State != "success" && task.State != "partial_success" {
		item.State = "failure"
		return true, w.save(ctx, item)
	}
	status, body, err = w.backendRequest(ctx, http.MethodGet, workerprotocol.ResultPath+item.BackendID, nil, "")
	if err != nil {
		if errors.Is(err, errBackendOutput) {
			item.State = "failure"
			return true, w.save(ctx, item)
		}
		return true, nil
	}
	if status == 404 {
		item.BackendID = ""
		item.State = "pending"
		return true, w.save(ctx, item)
	}
	if status != 200 {
		return true, nil
	}
	if len(body) == 0 || !jsontext.Value(body).IsValid() {
		item.State = "failure"
		return true, w.save(ctx, item)
	}
	if err := atomicfile.WriteFile(w.path(item.ID, "result.json"), body, atomicfile.WithPrivate()); err != nil {
		w.fail(err)
		return true, err
	}
	digest := sha256.Sum256(body)
	item.ResultBytes = int64(len(body))
	item.ResultSHA256 = hex.EncodeToString(digest[:])
	item.BackendTaskStatus = task.State
	item.State = "success"
	return true, w.save(ctx, item)
}

var (
	errBackendOutput     = errors.New("worker backend output violates bounds")
	errBackendSourceRead = errors.New("retained worker source read failed")
)

func (w *Worker) backendRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.options.BackendURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Api-Key", w.options.BackendKey)
	req.Header.Set(workerprotocol.BackendHopHeader, "1")
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := w.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	limit := w.options.MaxResultBytes
	if path == workerprotocol.ReadyPath || method == http.MethodPost || strings.HasPrefix(path, workerprotocol.PollPath) {
		limit = min(limit, 16<<10)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	if int64(len(data)) > limit {
		return response.StatusCode, nil, errBackendOutput
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		parsed, params, err := mimeType(response.Header.Get("Content-Type"))
		if err != nil || parsed != "application/json" || len(params) > 1 {
			return response.StatusCode, nil, errBackendOutput
		}
	}
	return response.StatusCode, data, nil
}
func (w *Worker) submitBackend(ctx context.Context, item job) (int, []byte, error) {
	if err := verifyPrivate(ctx, w.path(item.ID, "source"), item.Bytes, item.SHA256); err != nil {
		return 0, nil, fmt.Errorf("verify retained worker source: %w", err)
	}
	f, err := safefileio.OpenCurrentUserFile(w.path(item.ID, "source"))
	if err != nil {
		return 0, nil, fmt.Errorf("open retained worker source: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(f); err != nil {
		return 0, nil, fmt.Errorf("open retained worker source: %w", err)
	}
	return w.submitBackendStream(ctx, item, f)
}

func (w *Worker) submitBackendStream(ctx context.Context, item job, source io.Reader) (int, []byte, error) {
	// Source is bounded at admission; multipart framing is streamed from the retained file.
	input := &backendSourceReader{Reader: source}
	reader, writer := io.Pipe()
	multi := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", multipart.FileContentDisposition("files", item.Filename))
		header.Set("Content-Type", item.MediaType)
		part, err := multi.CreatePart(header)
		if err == nil {
			_, err = copyContext(ctx, part, input, item.Bytes)
		}
		for _, field := range item.Fields {
			if err == nil {
				err = multi.WriteField(field[0], field[1])
			}
		}
		if err == nil {
			err = multi.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	status, body, requestErr := w.backendRequest(ctx, http.MethodPost, workerprotocol.SubmitPath, reader, multi.FormDataContentType())
	_ = reader.Close()
	writeErr := <-done
	if requestErr == nil && status >= 200 && status < 300 {
		requestErr = writeErr
	}
	// An early definitive refusal may close the pipe without consuming it.
	// Preserve that refusal, but never mask a genuine retained-file read fault.
	var sourceErr error
	if input.readErr != nil {
		sourceErr = fmt.Errorf("%w: %w", errBackendSourceRead, input.readErr)
	}
	return status, body, errors.Join(requestErr, sourceErr)
}

type backendSourceReader struct {
	io.Reader

	readErr error
}

func (r *backendSourceReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.readErr = err
	}
	return n, err
}
func validBackendID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// Keep MIME parsing at the backend response boundary.
var mimeType = mime.ParseMediaType
