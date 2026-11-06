// Package conversionworker owns a bounded durable spool in front of Docling.
package conversionworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/document/workerprotocol"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/safefileio"
)

const protocol = workerprotocol.Contract
const overhead = int64(4096)

// Options fixes a worker's private storage, authenticated backend and bounds.
type Options struct {
	Directory      string
	APIKey         string
	BackendURL     string
	BackendKey     string
	MaxSourceBytes int64
	MaxResultBytes int64
	MaxJobs        int
	MaxSpoolBytes  int64
	PollInterval   time.Duration
	Client         *http.Client
}

type job struct {
	BackendOrigin     string      `json:"backend_origin"`
	ID                string      `json:"id"`
	Key               string      `json:"key"`
	Filename          string      `json:"filename"`
	MediaType         string      `json:"media_type"`
	Fields            [][2]string `json:"fields"`
	SHA256            string      `json:"sha256"`
	Bytes             int64       `json:"bytes"`
	State             string      `json:"state"`
	BackendID         string      `json:"backend_id"`
	BackendTaskStatus string      `json:"backend_task_status,omitempty"`
	Submissions       int         `json:"submissions"`
	ResultSHA256      string      `json:"result_sha256"`
	ResultBytes       int64       `json:"result_bytes"`
}

// Worker serves retained jobs while one runner performs conversion.
// Close must join all users before relinquishing the private spool lock.
type Worker struct {
	lifetime context.Context
	cancel   context.CancelFunc
	options  Options
	client   *http.Client
	lock     *home.Lock
	mu       sync.Mutex
	jobs     map[string]job
	keys     map[string]string
	used     int64
	reserved int64
	fault    error
	closed   bool
	active   sync.WaitGroup
	runMu    sync.Mutex
	uploadMu sync.Mutex
}

// Open locks and validates a private spool before accepting any request.
func Open(ctx context.Context, options Options) (_ *Worker, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.MaxSourceBytes == 0 {
		options.MaxSourceBytes = 64 << 20
	}
	if options.MaxResultBytes == 0 {
		options.MaxResultBytes = 64 << 20
	}
	if options.MaxJobs == 0 {
		options.MaxJobs = 128
	}
	if options.MaxSpoolBytes == 0 {
		options.MaxSpoolBytes = 40 << 30
	}
	if options.PollInterval == 0 {
		options.PollInterval = 5 * time.Second
	}
	backend, err := url.Parse(options.BackendURL)
	if err != nil || backend.Scheme != "http" || backend.User != nil || backend.Path != "" || backend.RawQuery != "" || backend.Fragment != "" {
		return nil, errors.New("worker backend must be a numeric loopback root HTTP origin")
	}
	if strings.ContainsAny(options.BackendURL, "?#") || strings.HasSuffix(backend.Host, ":") {
		return nil, errors.New("worker backend origin is ambiguous")
	}
	if port := backend.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, errors.New("worker backend port is invalid")
		}
	}
	address, err := netip.ParseAddr(backend.Hostname())
	if err != nil || !address.IsLoopback() {
		return nil, errors.New("worker backend must be loopback")
	}
	if !filepath.IsAbs(options.Directory) || len(options.APIKey) < 16 || len(options.APIKey) > 4096 || options.BackendKey == "" || options.MaxSourceBytes <= 0 || options.MaxSourceBytes > 100<<20 || options.MaxResultBytes <= 0 || options.MaxResultBytes > 128<<20 || options.MaxJobs < 1 || options.MaxJobs > 128 || options.MaxSpoolBytes < overhead || options.MaxSpoolBytes > 40<<30 || options.PollInterval <= 0 || options.PollInterval > time.Minute {
		return nil, errors.New("worker configuration is outside bounds")
	}
	if _, err := os.Lstat(options.Directory); err == nil {
		if err := safefileio.ValidatePrivateDir(options.Directory); err != nil {
			return nil, fmt.Errorf("worker spool: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := pack.MkdirAllSynced(options.Directory); err != nil {
			return nil, fmt.Errorf("worker spool: %w", err)
		}
		if err := safefileio.EnsurePrivateDir(options.Directory); err != nil {
			return nil, fmt.Errorf("worker spool: %w", err)
		}
	} else {
		return nil, err
	}
	lock, err := home.TryLockProcessingSpool(options.Directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, lock.Release())
		}
	}()
	client := options.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10}}
	}
	isolated := *client
	isolated.Jar = nil
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	isolated.Timeout = 30 * time.Second
	lifetime, cancel := context.WithCancel(ctx)
	defer func() {
		if retErr != nil {
			cancel()
		}
	}()
	w := &Worker{lifetime: lifetime, cancel: cancel, options: options, client: &isolated, lock: lock, jobs: make(map[string]job), keys: make(map[string]string)}
	entries, err := os.ReadDir(options.Directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(options.Directory, entry.Name())
		if strings.HasPrefix(entry.Name(), ".incoming-") {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil, errors.New("invalid incomplete worker stage")
			}
			if err := os.RemoveAll(path); err != nil {
				return nil, err
			}
			continue
		}
		if !isHex(entry.Name(), 32) || !entry.IsDir() {
			return nil, errors.New("unknown entry in worker spool")
		}
		if err := safefileio.ValidatePrivateDir(path); err != nil {
			return nil, fmt.Errorf("worker spool: %w", err)
		}
		data, err := readPrivate(ctx, filepath.Join(path, "manifest.json"), 16<<10)
		if err != nil {
			return nil, err
		}
		var item job
		if err := json.Unmarshal(data, &item, json.RejectUnknownMembers(true)); err != nil {
			return nil, errors.New("invalid committed worker manifest")
		}
		if item.BackendOrigin != options.BackendURL || item.ID != entry.Name() || !isHex(item.Key, 64) || !isHex(item.SHA256, 64) || item.Bytes <= 0 || item.Bytes > options.MaxSourceBytes || item.Submissions < 0 || item.Submissions > 3 || !slices.Contains([]string{"pending", "started", "success", "failure"}, item.State) || !validBackendTaskStatus(item.BackendTaskStatus) || (item.BackendTaskStatus != "" && item.State != "success") || !validSubmission(item.MediaType, item.Fields) || !validFile(item.Filename, item.MediaType) || (item.BackendID != "" && (!validBackendID(item.BackendID) || item.Submissions == 0)) || (item.State == "started" && item.BackendID == "") || (item.State == "pending" && item.BackendID != "") {
			return nil, errors.New("worker manifest contract is invalid")
		}
		if _, exists := w.keys[item.Key]; exists {
			return nil, errors.New("worker spool has duplicate request key")
		}
		if err := verifyPrivate(ctx, filepath.Join(path, "source"), item.Bytes, item.SHA256); err != nil {
			return nil, err
		}
		if item.State == "success" {
			if item.ResultBytes <= 0 || item.ResultBytes > options.MaxResultBytes {
				return nil, errors.New("worker result size is invalid")
			}
			if err := verifyPrivate(ctx, filepath.Join(path, "result.json"), item.ResultBytes, item.ResultSHA256); err != nil {
				return nil, err
			}
		}
		if err := reconcileJobStages(ctx, path); err != nil {
			return nil, err
		}
		w.jobs[item.ID] = item
		w.keys[item.Key] = item.ID
		w.used += w.reservation()
	}
	if len(w.jobs) > options.MaxJobs || w.used > options.MaxSpoolBytes {
		return nil, errors.New("existing worker spool exceeds configured limits")
	}
	return w, nil
}

func (w *Worker) reservation() int64 {
	return w.options.MaxSourceBytes + 2*w.options.MaxResultBytes + overhead
}
func (w *Worker) enter() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.active.Add(1)
	return true
}
func (w *Worker) fail(err error) { w.mu.Lock(); w.fault = err; w.mu.Unlock() }
func (w *Worker) save(ctx context.Context, item job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(filepath.Join(w.options.Directory, item.ID, "manifest.json"), data, atomicfile.WithPrivate()); err != nil {
		w.fail(err)
		return err
	}
	w.mu.Lock()
	w.jobs[item.ID] = item
	w.mu.Unlock()
	return nil
}

// Close stops admission and joins all HTTP handlers and runners before unlocking.
// The caller must cancel Run and drain its HTTP server before calling Close.
func (w *Worker) Close(ctx context.Context) error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cancel()
	// Ownership must stay held even if the shutdown caller's deadline expires.
	w.active.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lock == nil {
		return nil
	}
	err := w.lock.Release()
	if err == nil {
		w.lock = nil
	}
	return err
}

func readPrivate(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return nil, fmt.Errorf("worker spool: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(f); err != nil {
		return nil, fmt.Errorf("worker spool: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("worker file exceeds bound")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("worker file exceeds bound")
	}
	return data, nil
}
func verifyPrivate(ctx context.Context, path string, size int64, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return fmt.Errorf("worker spool: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(f); err != nil {
		return fmt.Errorf("worker spool: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, size+1))
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("worker retained bytes do not match manifest")
	}
	return nil
}
func isHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
func copyContext(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	var total int64
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buffer[:min(int64(len(buffer)), limit+1-total)])
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
			if total > limit {
				return total, errors.New("worker input exceeds byte limit")
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}
func (w *Worker) task(item job) map[string]string {
	task := map[string]string{"task_id": item.ID, "task_status": item.State}
	if item.BackendTaskStatus != "" {
		task["backend_task_status"] = item.BackendTaskStatus
	}
	return task
}
func (w *Worker) path(id, name string) string { return filepath.Join(w.options.Directory, id, name) }
func (w *Worker) errorCode() error            { w.mu.Lock(); defer w.mu.Unlock(); return w.fault }

func validBackendTaskStatus(status string) bool {
	return status == "" || status == "success" || status == "partial_success"
}

// reconcileJobStages removes only private, incomplete writes owned by atomicfile.
// Committed source, result and manifest bytes are never scavenged.
func reconcileJobStages(ctx context.Context, directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if name == "source" || name == "result.json" || name == "manifest.json" {
			continue
		}
		if !strings.HasPrefix(name, ".manifest.json.tmp-") && !strings.HasPrefix(name, ".result.json.tmp-") {
			return errors.New("unknown file in committed worker job")
		}
		path := filepath.Join(directory, name)
		file, err := safefileio.OpenCurrentUserFile(path)
		if err != nil {
			return fmt.Errorf("open incomplete worker stage: %w", err)
		}
		checkErr := safefileio.ValidatePrivateCurrentUserFile(file)
		info, statErr := file.Stat()
		closeErr := file.Close()
		if checkErr != nil || statErr != nil || closeErr != nil {
			return errors.Join(checkErr, statErr, closeErr)
		}
		if !info.Mode().IsRegular() {
			return errors.New("worker incomplete stage is not regular")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return atomicfile.SyncDir(directory)
	}
	return nil
}
