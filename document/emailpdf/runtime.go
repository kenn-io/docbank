package emailpdf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/docbank/document/media"
)

const MaxPDFBytes = 256 << 20
const MaxPDFPages = 1000

// RuntimeConfig pins the worker, bubblewrap, browser bundle, and fonts.
// System libraries come from read-only host mounts; system fonts are not used.
type RuntimeConfig struct {
	Worker           string `json:"worker"`
	WorkerSHA256     string `json:"worker_sha256"`
	Bubblewrap       string `json:"bubblewrap"`
	BubblewrapSHA256 string `json:"bubblewrap_sha256"`
	Chromium         string `json:"chromium"`
	Version          string `json:"version"`
	Bundle           string `json:"bundle"`
	BundleSHA256     string `json:"bundle_sha256"`
	Fonts            string `json:"fonts"`
	FontsSHA256      string `json:"fonts_sha256"`
	Spool            string `json:"-"`
}

type Runtime struct {
	config RuntimeConfig
	slots  chan struct{}
}

// TreeSHA256 fingerprints names and full ordinary file bytes. Symlinks and
// special files are rejected so the pin cannot reach outside the package.
func TreeSHA256(root string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("email PDF runtime tree must be an absolute clean path")
	}
	var names []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error { //nolint:gosec // Caller explicitly selects an absolute runtime tree; links/special files are rejected below.
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return errors.New("email PDF runtime tree contains a link or special file")
		}
		names = append(names, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", errors.New("email PDF runtime tree is empty")
	}
	slices.Sort(names)
	digest := sha256.New()
	for _, path := range names {
		name, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		h := sha256.New()
		_, readErr := io.Copy(h, f)
		err = errors.Join(readErr, f.Close())
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(digest, "%s\x00%x\n", filepath.ToSlash(name), h.Sum(nil))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func NewRuntime(c RuntimeConfig) (*Runtime, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("email PDF unavailable: isolated Chromium currently requires Linux with systemd; configure a qualified Linux daemon")
	}
	if !filepath.IsAbs(c.Chromium) || !filepath.IsAbs(c.Worker) ||
		len(c.WorkerSHA256) != 64 ||
		c.Version == "" || len(c.BundleSHA256) != 64 || len(c.FontsSHA256) != 64 {
		return nil, errors.New("email PDF unavailable: configure a pinned Chromium bundle and fonts")
	}
	rel, err := filepath.Rel(c.Bundle, c.Chromium)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("email PDF Chromium must be inside its pinned bundle")
	}
	r := &Runtime{config: c, slots: make(chan struct{}, 2)}
	if err := r.verifyPin(); err != nil {
		return nil, err
	}
	if c.Bubblewrap == "" {
		return nil, fmt.Errorf("%w: install the distribution's bubblewrap package", ErrUnavailable)
	}
	if !filepath.IsAbs(c.Bubblewrap) || len(c.BubblewrapSHA256) != 64 {
		return nil, errors.New("email PDF bubblewrap pin must name an absolute executable and SHA256")
	}
	return r, nil
}

func (r *Runtime) verifyPin() error {
	for path, want := range map[string]string{
		r.config.Worker:     r.config.WorkerSHA256,
		r.config.Bubblewrap: r.config.BubblewrapSHA256,
	} {
		if path == "" { // Missing bubblewrap is reported after validating configured runtime pins.
			continue
		}
		got, err := FileSHA256(path)
		if err != nil {
			return err
		}
		if got != want {
			return errors.New("email PDF worker or bubblewrap pin changed")
		}
	}
	for path, want := range map[string]string{r.config.Bundle: r.config.BundleSHA256, r.config.Fonts: r.config.FontsSHA256} {
		got, err := TreeSHA256(path)
		if err != nil {
			return err
		}
		if got != want {
			return errors.New("email PDF runtime pin changed")
		}
	}
	return nil
}

func (r *Runtime) Config() RuntimeConfig { return r.config }

func FileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // Explicit operator-owned worker executable, intentionally fingerprinted in full.
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Render bounds the entire Chromium descendant group. Cancellation stops and
// drains the unit before staging is removed or any output is returned.
func (r *Runtime) Render(ctx context.Context, input HTML) (_ []byte, pages int64, retErr error) {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	if err := r.verifyPin(); err != nil {
		return nil, 0, err
	}
	// Full pin verification remains outside the browser's execution budget.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if len(input.Bytes) == 0 || len(input.Bytes) > maxHTMLBytes {
		return nil, 0, errors.New("email PDF HTML size invalid")
	}
	dir, err := os.MkdirTemp(r.config.Spool, "email-pdf-")
	if err != nil {
		return nil, 0, err
	}
	cleanupSafe := true
	defer func() {
		if cleanupSafe {
			retErr = errors.Join(retErr, os.RemoveAll(dir))
		}
	}()
	if err = r.writeInputs(dir, input.Bytes); err != nil {
		return nil, 0, err
	}
	unit := "docbank-email-pdf-" + uuid.New().String() + ".service"
	if err := os.WriteFile(filepath.Join(dir, unitMarker), []byte(unit), 0600); err != nil {
		return nil, 0, err
	}
	cleanupSafe = false
	cmd := r.command(ctx, dir, unit, "/runtime/worker", r.workerArgs()...)
	var diagnostic runtimeDiagnostic
	cmd.Stderr = &diagnostic
	err = cmd.Run()
	// systemd-run is a client, not the browser's parent. Always explicitly stop
	// the unit, including cancellation during submission, before reading output.
	if cleanupErr := stopUnit(context.Background(), unit); cleanupErr != nil {
		return nil, 0, cleanupErr
	}
	cleanupSafe = true
	if err = errors.Join(err, ctx.Err()); err != nil {
		return nil, 0, fmt.Errorf("email PDF isolated Chromium failed (%s): %w", diagnostic.String(), err)
	}
	b, err := readOutput(filepath.Join(dir, "message.pdf"))
	if err != nil {
		return nil, 0, err
	}
	normalizeMetadata(b)
	pages, err = VerifyPDF(b)
	if err != nil {
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return b, pages, nil
}

// The unit has stopped before this call, so its processes cannot replace the
// checked file with a link or pipe while the daemon reads it.
func readOutput(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > MaxPDFBytes {
		return nil, errors.New("email PDF output must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxPDFBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPDFBytes {
		return nil, errors.New("email PDF exceeds output limit")
	}
	return b, nil
}

func (r *Runtime) writeInputs(dir string, b []byte) error {
	if err := os.WriteFile(filepath.Join(dir, "message.html"), b, 0600); err != nil {
		return err
	}
	fontConfig := `<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd"><fontconfig><dir>/runtime/fonts</dir><cachedir>/work/fontcache</cachedir></fontconfig>`
	return os.WriteFile(filepath.Join(dir, "fonts.conf"), []byte(fontConfig), 0600)
}

func (r *Runtime) workerArgs() []string {
	chromium, _ := filepath.Rel(r.config.Bundle, r.config.Chromium) // Validated by NewRuntime.
	return []string{"internal-email-pdf-worker", filepath.Join("/runtime/chromium", chromium), "/work", r.config.Version}
}

func (r *Runtime) sandboxArgs(dir string) []string {
	return []string{
		"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind-try", "/lib", "/lib", "--ro-bind-try", "/lib64", "/lib64",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache",
		"--ro-bind", r.config.Worker, "/runtime/worker",
		"--ro-bind", r.config.Bundle, "/runtime/chromium",
		"--ro-bind", r.config.Fonts, "/runtime/fonts",
		"--bind", dir, "/work", "--chdir", "/work",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--clearenv", "--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "LANG", "C.UTF-8", "--setenv", "LC_ALL", "C.UTF-8",
		"--setenv", "TZ", "UTC", "--setenv", "HOME", "/work",
		"--setenv", "FONTCONFIG_FILE", "/work/fonts.conf",
	}
}

func (r *Runtime) command(ctx context.Context, dir, unit, executable string, workerArgs ...string) *exec.Cmd {
	args := []string{
		"--user", "--quiet", "--wait", "--pipe", "--collect", "--unit=" + unit,
		"--property=MemoryMax=536870912", "--property=MemorySwapMax=0",
		"--property=KillMode=control-group", "--property=RuntimeMaxSec=60",
		"--property=TimeoutStopSec=2", "--property=SendSIGKILL=yes",
		"--property=LimitFSIZE=268435456", "--property=TasksMax=512",
		"--property=NoNewPrivileges=yes", "--property=UMask=0077", r.config.Bubblewrap,
	}
	args = append(args, r.sandboxArgs(dir)...)
	args = append(args, executable)
	args = append(args, workerArgs...)
	cmd := exec.CommandContext(ctx, "systemd-run", args...) //nolint:gosec // Fixed user-unit properties and pinned executables; email bytes are a separate file.
	cmd.Env = userManagerEnv()
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

func userManagerEnv() []string {
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type runtimeDiagnostic struct{ bytes.Buffer }

func (b *runtimeDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		_, _ = b.Buffer.Write(p[:min(len(p), 4096-b.Len())])
	}
	return n, nil
}

var pdfDate = regexp.MustCompile(`/((?:Creation|Mod)Date) \(D:[^)]*\)`)
var pdfID = regexp.MustCompile(`/ID\s*\[<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>\]`)

// Keep every object byte offset stable while fixing runtime-generated dates
// and IDs. We promise same pinned runtime reproducibility, not cross-build bytes.
func normalizeMetadata(b []byte) {
	for _, loc := range pdfDate.FindAllIndex(b, -1) {
		s := b[loc[0]:loc[1]]
		i := bytes.Index(s, []byte("D:"))
		if i < 0 {
			continue
		}
		for j := i + 2; j < len(s)-1; j++ {
			if s[j] >= '0' && s[j] <= '9' {
				s[j] = '0'
			}
		}
		if len(s) >= i+16 {
			copy(s[i+2:i+16], "19700101000000")
		}
	}
	for _, loc := range pdfID.FindAllSubmatchIndex(b, -1) {
		for i := 2; i < len(loc); i += 2 {
			for j := loc[i]; j < loc[i+1]; j++ {
				b[j] = '0'
			}
		}
	}
}

func VerifyPDF(b []byte) (int64, error) {
	if len(b) == 0 || len(b) > MaxPDFBytes || !bytes.HasPrefix(b, []byte("%PDF-")) || !bytes.HasSuffix(bytes.TrimSpace(b), []byte("%%EOF")) {
		return 0, errors.New("email PDF output is empty, truncated or oversized")
	}
	pages, err := media.CountPDFPages(b)
	if err != nil {
		return 0, fmt.Errorf("email PDF independent structural verification: %w", err)
	}
	if pages < 1 || pages > MaxPDFPages {
		return 0, errors.New("email PDF page count exceeds 1–1000 bound")
	}
	return pages, nil
}
