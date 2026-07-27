package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/kit/safefileio"
)

const (
	webDownloadPreparePath = "/api/daemon/web-download"
	webDownloadFilePath    = "/api/daemon/web-download/file"
	webDownloadTicketTTL   = 2 * time.Minute
)

type webDownloadRegistry struct {
	mu       sync.Mutex
	dir      string
	initOnce sync.Once
	initErr  error
	tickets  map[[sha256.Size]byte]webDownloadTicket
}

type webDownloadTicket struct {
	path      string
	name      string
	mediaType string
	versionID string
	blobHash  string
	size      int64
	expiresAt time.Time
	timer     *time.Timer
}

type webDownloadRequest struct {
	NodeID    int64  `json:"node_id"`
	Revision  int64  `json:"revision"`
	VersionID string `json:"version_id"`
	BlobHash  string `json:"blob_hash"`
	Size      int64  `json:"size"`
}

type webDownloadEvent struct {
	Phase     string `json:"phase"`
	Received  int64  `json:"received"`
	Total     int64  `json:"total"`
	URL       string `json:"url,omitempty"`
	Name      string `json:"name,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	BlobHash  string `json:"blob_hash,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

func newWebDownloadRegistry(vaultRoot string) *webDownloadRegistry {
	return &webDownloadRegistry{
		dir:     filepath.Join(vaultRoot, "web-downloads"),
		tickets: make(map[[sha256.Size]byte]webDownloadTicket),
	}
}

func (r *webDownloadRegistry) createStagingFile() (*os.File, string, error) {
	r.initOnce.Do(func() {
		if err := os.RemoveAll(r.dir); err != nil {
			r.initErr = fmt.Errorf("removing abandoned web downloads: %w", err)
			return
		}
		if err := safefileio.EnsurePrivateDir(r.dir); err != nil {
			r.initErr = fmt.Errorf("securing web download staging: %w", err)
		}
	})
	if r.initErr != nil {
		return nil, "", r.initErr
	}
	file, err := os.CreateTemp(r.dir, ".download-*")
	if err != nil {
		return nil, "", fmt.Errorf("creating web download staging file: %w", err)
	}
	return file, file.Name(), nil
}

func (r *webDownloadRegistry) issue(ticket webDownloadTicket) (string, error) {
	for range 10 {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generating web download ticket: %w", err)
		}
		token := base64.RawURLEncoding.EncodeToString(raw[:])
		key := sha256.Sum256([]byte(token))

		r.mu.Lock()
		if _, exists := r.tickets[key]; exists {
			r.mu.Unlock()
			continue
		}
		ticket.expiresAt = time.Now().Add(webDownloadTicketTTL)
		ticket.timer = time.AfterFunc(webDownloadTicketTTL, func() {
			r.expire(key)
		})
		r.tickets[key] = ticket
		r.mu.Unlock()
		return token, nil
	}
	return "", errors.New("generating web download ticket: repeated collisions")
}

func (r *webDownloadRegistry) consume(token string) (webDownloadTicket, bool) {
	key := sha256.Sum256([]byte(token))
	r.mu.Lock()
	ticket, ok := r.tickets[key]
	if ok {
		delete(r.tickets, key)
	}
	r.mu.Unlock()
	if !ok {
		return webDownloadTicket{}, false
	}
	ticket.timer.Stop()
	if time.Now().After(ticket.expiresAt) {
		_ = os.Remove(ticket.path)
		return webDownloadTicket{}, false
	}
	return ticket, true
}

func (r *webDownloadRegistry) expire(key [sha256.Size]byte) {
	r.mu.Lock()
	ticket, ok := r.tickets[key]
	if ok {
		delete(r.tickets, key)
	}
	r.mu.Unlock()
	if ok {
		_ = os.Remove(ticket.path)
	}
}

func registerWebDownload(
	mux *http.ServeMux,
	enabled bool,
	d Deps,
	downloads *webDownloadRegistry,
) {
	mux.HandleFunc("POST "+webDownloadPreparePath, func(w http.ResponseWriter, r *http.Request) {
		if !enabled || d.WebURL == "" {
			writeError(w, NewError(http.StatusServiceUnavailable, "web_unavailable",
				"this daemon is not serving the compiled web application"))
			return
		}
		request, decodeErr := decodeWebDownloadRequest(w, r)
		if decodeErr != nil {
			writeError(w, decodeErr)
			return
		}
		view, err := d.Store.ContentVersionViewByID(
			r.Context(), request.NodeID, request.VersionID,
		)
		if err != nil {
			writeError(w, webDownloadProblem(err))
			return
		}
		node := view.Node
		version := view.Version
		if node.IsDir() {
			writeError(w, NewError(http.StatusUnprocessableEntity, "not_file",
				fmt.Sprintf("node %d is a directory", node.ID)))
			return
		}
		if node.TrashedAt != nil {
			writeError(w, NewError(http.StatusNotFound, "not_found",
				fmt.Sprintf("node %d is not live", node.ID)))
			return
		}
		if node.Revision != request.Revision ||
			version.BlobHash != request.BlobHash ||
			version.Size != request.Size {
			writeError(w, NewError(http.StatusConflict, "download_selection_stale",
				"the selected document or version changed; refresh it before downloading"))
			return
		}

		stream, streamSize, err := d.Blobs.OpenStreamContext(r.Context(), version.BlobHash)
		if err != nil {
			writeError(w, NewError(http.StatusInternalServerError, "internal",
				"opening document content failed; run docbank verify"))
			return
		}
		if streamSize != version.Size {
			_ = stream.Close()
			writeError(w, NewError(http.StatusInternalServerError, "internal",
				"document size authority is inconsistent; run docbank verify"))
			return
		}

		file, stagedPath, err := downloads.createStagingFile()
		if err != nil {
			_ = stream.Close()
			writeError(w, NewError(http.StatusInternalServerError, "internal",
				"creating private download staging failed"))
			return
		}
		keepStaged := false
		defer func() {
			if !keepStaged {
				_ = os.Remove(stagedPath)
			}
		}()

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(w)
		flusher, _ := w.(http.Flusher)
		report := func(event webDownloadEvent) error {
			if err := encoder.Encode(event); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		if err := report(webDownloadEvent{Phase: "progress", Total: version.Size}); err != nil {
			_ = file.Close()
			_ = stream.Close()
			return
		}

		progress := &webDownloadProgressWriter{
			ctx: r.Context(), dst: file, total: version.Size, report: report,
		}
		_, copyErr := io.CopyBuffer(progress, stream, make([]byte, 256<<10))
		closeStreamErr := stream.Close()
		closeFileErr := file.Close()
		if err := errors.Join(copyErr, closeStreamErr, closeFileErr); err != nil ||
			!stream.Verified() || progress.written != version.Size {
			_ = report(webDownloadEvent{
				Phase: "error", Total: version.Size,
				Detail: "Docbank could not verify the complete document; run docbank verify",
			})
			return
		}

		ticket := webDownloadTicket{
			path: stagedPath, name: node.Name, mediaType: version.MimeType,
			versionID: version.ID, blobHash: version.BlobHash, size: version.Size,
		}
		token, err := downloads.issue(ticket)
		if err != nil {
			_ = report(webDownloadEvent{
				Phase: "error", Total: version.Size,
				Detail: "Docbank could not publish the verified browser download",
			})
			return
		}
		keepStaged = true
		_ = report(webDownloadEvent{
			Phase: "ready", Received: version.Size, Total: version.Size,
			URL:  webDownloadFilePath + "?ticket=" + token,
			Name: node.Name, VersionID: version.ID, BlobHash: version.BlobHash,
		})
	})

	mux.HandleFunc("GET "+webDownloadFilePath, func(w http.ResponseWriter, r *http.Request) {
		ticket, ok := downloads.consume(r.URL.Query().Get("ticket"))
		if !ok {
			writeError(w, NewError(http.StatusNotFound, "download_not_found",
				"the browser download is missing, expired, or already used"))
			return
		}
		defer func() { _ = os.Remove(ticket.path) }()

		file, err := os.Open(ticket.path)
		if err != nil {
			writeError(w, NewError(http.StatusGone, "download_unavailable",
				"the verified browser download is no longer available"))
			return
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != ticket.size {
			writeError(w, NewError(http.StatusGone, "download_unavailable",
				"the verified browser download is no longer available"))
			return
		}

		mediaType := ticket.mediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": ticket.name}))
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Length", strconv.FormatInt(ticket.size, 10))
		w.Header().Set("Content-Digest", contentDigest(mustDecodeHash(ticket.blobHash)))
		w.Header().Set(ContentVersionHeader, ticket.versionID)
		w.Header().Set(BlobHashHeader, ticket.blobHash)
		w.Header().Set(BlobSizeHeader, strconv.FormatInt(ticket.size, 10))
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, bufio.NewReaderSize(file, 256<<10))
	})
}

func decodeWebDownloadRequest(w http.ResponseWriter, r *http.Request) (webDownloadRequest, *Error) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request webDownloadRequest
	if err := decoder.Decode(&request); err != nil {
		return webDownloadRequest{}, NewError(http.StatusBadRequest, "validation",
			"download request must be one JSON object with known fields")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return webDownloadRequest{}, NewError(http.StatusBadRequest, "validation",
			"download request must contain exactly one JSON object")
	}
	if request.NodeID < 1 || request.Revision < 1 || request.Size < 0 ||
		request.VersionID == "" || len(request.BlobHash) != 64 {
		return webDownloadRequest{}, NewError(http.StatusUnprocessableEntity, "validation",
			"download request has invalid document authority")
	}
	if _, err := hex.DecodeString(request.BlobHash); err != nil {
		return webDownloadRequest{}, NewError(http.StatusUnprocessableEntity, "validation",
			"download request has invalid document authority")
	}
	return request, nil
}

type webDownloadProgressWriter struct {
	ctx        context.Context
	dst        io.Writer
	total      int64
	written    int64
	lastReport time.Time
	report     func(webDownloadEvent) error
}

func (w *webDownloadProgressWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.dst.Write(p)
	w.written += int64(n)
	if err != nil {
		return n, err
	}
	now := time.Now()
	if w.written == w.total || w.lastReport.IsZero() ||
		now.Sub(w.lastReport) >= 100*time.Millisecond {
		w.lastReport = now
		if err := w.report(webDownloadEvent{
			Phase: "progress", Received: w.written, Total: w.total,
		}); err != nil {
			return n, err
		}
	}
	return n, nil
}

func mustDecodeHash(hash string) []byte {
	decoded, err := hex.DecodeString(hash)
	if err != nil {
		panic("validated blob hash became invalid")
	}
	return decoded
}

func webDownloadProblem(err error) *Error {
	var problem *Error
	if errors.As(FromStoreError(err), &problem) {
		return problem
	}
	return NewError(http.StatusInternalServerError, "internal", err.Error())
}
