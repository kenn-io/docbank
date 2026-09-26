package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/daemonconn"
)

const (
	maxMCPExportArchiveBytes int64 = 512 << 20
	maxMCPExportChunkBytes         = 256 << 10
	maxMCPExportHandles            = 16
	exportArchiveHandleLife        = 15 * time.Minute
)

var (
	errExportArchiveHandle   = errors.New("export archive handle is unavailable")
	errExportArchiveCapacity = errors.New("export archive exceeds MCP spool capacity; use the CLI")
	exportArchiveBudget      = struct {
		sync.Mutex

		bytes   int64
		handles int
	}{}
)

type exportArchiveSpool struct {
	jobID   string
	receipt bundle.Receipt
	file    *os.File
	path    string
	dir     string
	pin     *os.File
	expires time.Time
	timer   *time.Timer
}

type exportArchiveRegistry struct {
	sync.Mutex

	spools  map[string]*exportArchiveSpool
	opening int
	closed  bool
}

func newExportArchiveRegistry() *exportArchiveRegistry {
	return &exportArchiveRegistry{spools: make(map[string]*exportArchiveSpool)}
}

func (r *exportArchiveRegistry) reserve(size int64) error {
	if size < 1 || size > maxMCPExportArchiveBytes {
		return errExportArchiveCapacity
	}
	r.Lock()
	defer r.Unlock()
	if r.closed {
		return errExportArchiveHandle
	}
	exportArchiveBudget.Lock()
	defer exportArchiveBudget.Unlock()
	if exportArchiveBudget.handles >= maxMCPExportHandles ||
		exportArchiveBudget.bytes > maxMCPExportArchiveBytes-size {
		return errExportArchiveCapacity
	}
	exportArchiveBudget.bytes += size
	exportArchiveBudget.handles++
	r.opening++
	return nil
}

func (r *exportArchiveRegistry) abort(size int64) {
	r.Lock()
	r.opening--
	exportArchiveBudget.Lock()
	exportArchiveBudget.bytes -= size
	exportArchiveBudget.handles--
	exportArchiveBudget.Unlock()
	r.Unlock()
}

func (r *exportArchiveRegistry) publish(spool *exportArchiveSpool) (string, error) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		r.abort(spool.receipt.Size)
		cleanupExportArchiveSpool(spool)
		return "", fmt.Errorf("generate export archive handle: %w", err)
	}
	token := hex.EncodeToString(random[:])
	r.Lock()
	r.opening--
	if r.closed {
		exportArchiveBudget.Lock()
		exportArchiveBudget.bytes -= spool.receipt.Size
		exportArchiveBudget.handles--
		exportArchiveBudget.Unlock()
		r.Unlock()
		cleanupExportArchiveSpool(spool)
		return "", errExportArchiveHandle
	}
	r.spools[token] = spool
	spool.timer = time.AfterFunc(time.Until(spool.expires), func() { r.drop(token) })
	r.Unlock()
	return token, nil
}

func (r *exportArchiveRegistry) lookup(token string) (string, bundle.Receipt, error) {
	if len(token) != 48 {
		return "", bundle.Receipt{}, errExportArchiveHandle
	}
	r.Lock()
	defer r.Unlock()
	spool := r.spools[token]
	if spool == nil || !time.Now().Before(spool.expires) {
		return "", bundle.Receipt{}, errExportArchiveHandle
	}
	return spool.jobID, spool.receipt, nil
}

func (r *exportArchiveRegistry) read(token string, receipt bundle.Receipt, offset, limit int64, closeAfter bool) ([]byte, error) {
	r.Lock()
	defer r.Unlock()
	spool := r.spools[token]
	if spool == nil || spool.receipt != receipt || !time.Now().Before(spool.expires) ||
		offset < 0 || offset > receipt.Size || limit < 1 || limit > maxMCPExportChunkBytes {
		return nil, errExportArchiveHandle
	}
	info, err := spool.file.Stat()
	if err != nil || info.Size() != receipt.Size {
		r.dropLocked(token)
		return nil, errExportArchiveHandle
	}
	chunk := make([]byte, int(min(limit, receipt.Size-offset)))
	if len(chunk) != 0 {
		if _, err := spool.file.ReadAt(chunk, offset); err != nil {
			r.dropLocked(token)
			return nil, errExportArchiveHandle
		}
	}
	if closeAfter {
		r.dropLocked(token)
	}
	return chunk, nil
}

func (r *exportArchiveRegistry) drop(token string) {
	r.Lock()
	r.dropLocked(token)
	r.Unlock()
}

func (r *exportArchiveRegistry) dropLocked(token string) {
	spool := r.spools[token]
	if spool == nil {
		return
	}
	delete(r.spools, token)
	if spool.timer != nil {
		spool.timer.Stop()
	}
	exportArchiveBudget.Lock()
	exportArchiveBudget.bytes -= spool.receipt.Size
	exportArchiveBudget.handles--
	exportArchiveBudget.Unlock()
	cleanupExportArchiveSpool(spool)
}

func (r *exportArchiveRegistry) closeAll() {
	r.Lock()
	r.closed = true
	for token := range r.spools {
		r.dropLocked(token)
	}
	r.Unlock()
}

func cleanupExportArchiveSpool(spool *exportArchiveSpool) {
	if spool == nil {
		return
	}
	if spool.file != nil {
		_ = spool.file.Close()
	}
	if spool.path != "" {
		_ = os.Remove(spool.path)
	}
	if spool.pin != nil {
		_ = spool.pin.Close()
	}
	if spool.dir != "" {
		_ = os.Remove(spool.dir)
	}
}

type openExportArchiveOutput struct {
	privateCache

	Handle          string `json:"handle"`
	JobID           string `json:"job_id"`
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
	PlanFingerprint string `json:"plan_fingerprint"`
	ExpiresAt       string `json:"expires_at"`
}

type downloadExportArchiveOutput struct {
	privateCache

	DataBase64 string `json:"data_base64"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	TotalBytes int64  `json:"total_bytes"`
	SHA256     string `json:"sha256"`
	EOF        bool   `json:"eof"`
	Closed     bool   `json:"closed"`
}

type exportArchiveAttempt struct {
	spool    *exportArchiveSpool
	localErr error
}

func openMCPExportArchive(ctx context.Context, lease *daemonLease, registry *exportArchiveRegistry, raw []byte) (openExportArchiveOutput, error) {
	var input struct {
		JobID string `json:"job_id"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return openExportArchiveOutput{}, err
	}
	if !daemonconn.IsCanonicalUUIDv4(input.JobID) {
		return openExportArchiveOutput{}, invalidToolArgumentsError()
	}
	attempt, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (exportArchiveAttempt, error) {
		authority, err := client.ExportArchiveAuthority(ctx, input.JobID)
		if err != nil {
			return exportArchiveAttempt{}, err
		}
		if err := registry.reserve(authority.Size); err != nil {
			return exportArchiveAttempt{localErr: err}, nil
		}
		reserved := true
		defer func() {
			if reserved {
				registry.abort(authority.Size)
			}
		}()
		stream, err := client.OpenExportArchive(ctx, input.JobID)
		if err != nil {
			return exportArchiveAttempt{}, err
		}
		defer func() { _ = stream.Close() }()
		if stream.Size != authority.Size || stream.SHA256 != authority.SHA256 || stream.PlanFingerprint != authority.PlanFingerprint {
			return exportArchiveAttempt{}, bundle.ErrConflict
		}
		spool, err := createPrivateExportArchiveSpoolAt(os.TempDir())
		if err != nil {
			return exportArchiveAttempt{}, err
		}
		keep := false
		defer func() {
			if !keep {
				cleanupExportArchiveSpool(spool)
			}
		}()
		if _, err := stream.CopyVerified(spool.file); err != nil {
			return exportArchiveAttempt{}, err
		}
		verified, err := bundle.Verify(ctx, spool.file, authority.Size, authority.PlanFingerprint)
		if err != nil || verified != authority {
			return exportArchiveAttempt{}, bundle.ErrInvalidArchive
		}
		current, err := client.ExportArchiveAuthority(ctx, input.JobID)
		if err != nil || current != authority {
			return exportArchiveAttempt{}, bundle.ErrConflict
		}
		if err := os.Remove(spool.path); err == nil {
			spool.path = ""
		}
		spool.jobID, spool.receipt = input.JobID, authority
		spool.expires = time.Now().Add(exportArchiveHandleLife).UTC().Truncate(time.Second)
		reserved, keep = false, true
		return exportArchiveAttempt{spool: spool}, nil
	})
	if err != nil {
		return openExportArchiveOutput{}, err
	}
	if attempt.localErr != nil {
		return openExportArchiveOutput{}, attempt.localErr
	}
	if attempt.spool == nil {
		return openExportArchiveOutput{}, errExportArchiveHandle
	}
	token, err := registry.publish(attempt.spool)
	if err != nil {
		return openExportArchiveOutput{}, err
	}
	return openExportArchiveOutput{privateCache: newPrivateCache(), Handle: token, JobID: input.JobID,
		Size: attempt.spool.receipt.Size, SHA256: attempt.spool.receipt.SHA256,
		PlanFingerprint: attempt.spool.receipt.PlanFingerprint,
		ExpiresAt:       attempt.spool.expires.Format(time.RFC3339)}, nil
}

func downloadMCPExportArchive(ctx context.Context, lease *daemonLease, registry *exportArchiveRegistry, raw []byte) (downloadExportArchiveOutput, error) {
	var input struct {
		Handle   string `json:"handle"`
		Offset   int64  `json:"offset"`
		MaxBytes int64  `json:"max_bytes"`
		Close    bool   `json:"close"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return downloadExportArchiveOutput{}, err
	}
	jobID, receipt, err := registry.lookup(input.Handle)
	if err != nil || input.Offset < 0 || input.Offset > receipt.Size || input.MaxBytes < 1 || input.MaxBytes > maxMCPExportChunkBytes {
		return downloadExportArchiveOutput{}, errExportArchiveHandle
	}
	if input.Close {
		defer registry.drop(input.Handle)
	}
	current, err := daemonRead(ctx, lease, func(ctx context.Context, client *daemonconn.Connection) (bundle.Receipt, error) {
		return client.ExportArchiveAuthority(ctx, jobID)
	})
	if err != nil || current != receipt {
		registry.drop(input.Handle)
		if err != nil {
			return downloadExportArchiveOutput{}, err
		}
		return downloadExportArchiveOutput{}, errExportArchiveHandle
	}
	chunk, err := registry.read(input.Handle, receipt, input.Offset, input.MaxBytes, input.Close)
	if err != nil {
		return downloadExportArchiveOutput{}, err
	}
	next := input.Offset + int64(len(chunk))
	return downloadExportArchiveOutput{privateCache: newPrivateCache(), DataBase64: base64.StdEncoding.EncodeToString(chunk),
		Offset: input.Offset, NextOffset: next, TotalBytes: receipt.Size, SHA256: receipt.SHA256,
		EOF: next == receipt.Size, Closed: input.Close}, nil
}
