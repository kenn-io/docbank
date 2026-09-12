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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document/media"
)

const MaxPDFBytes = 256 << 20
const MaxPDFPages = 1000

// RuntimeConfig is an explicit operator pin. Both trees are content addressed;
// no browser, library, or font is chosen from a developer's PATH or font config.
type RuntimeConfig struct {
	Worker       string `json:"worker"`
	WorkerSHA256 string `json:"worker_sha256"`
	Chromium     string `json:"chromium"`
	Version      string `json:"version"`
	Bundle       string `json:"bundle"`
	BundleSHA256 string `json:"bundle_sha256"`
	Fonts        string `json:"fonts"`
	FontsSHA256  string `json:"fonts_sha256"`
	Spool        string `json:"-"`
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
	if !filepath.IsAbs(c.Chromium) || !filepath.IsAbs(c.Worker) || len(c.WorkerSHA256) != 64 || c.Version == "" || len(c.BundleSHA256) != 64 || len(c.FontsSHA256) != 64 {
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
	return r, nil
}

func (r *Runtime) verifyPin() error {
	workerHash, err := FileSHA256(r.config.Worker)
	if err != nil {
		return err
	}
	if workerHash != r.config.WorkerSHA256 {
		return errors.New("email PDF worker pin changed")
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
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := r.verifyPin(); err != nil {
		return nil, 0, err
	}
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
	outputPath := filepath.Join(dir, "message.pdf")
	if err = r.writeInputs(dir, input.Bytes); err != nil {
		return nil, 0, err
	}
	unit := "docbank-email-pdf-" + uuid.NewString() + ".service"
	if err := os.WriteFile(filepath.Join(dir, unitMarker), []byte(unit), 0600); err != nil {
		return nil, 0, err
	}
	cleanupSafe = false
	cmd := r.command(ctx, dir, unit)
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
	f, err := os.Open(outputPath)
	if err != nil {
		return nil, 0, err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxPDFBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, 0, err
	}
	if len(b) > MaxPDFBytes {
		return nil, 0, errors.New("email PDF exceeds output limit")
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

func (r *Runtime) writeInputs(dir string, b []byte) error {
	if err := os.WriteFile(filepath.Join(dir, "message.html"), b, 0600); err != nil {
		return err
	}
	fontConfig := `<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd"><fontconfig><dir>` + escapeXML(r.config.Fonts) + `</dir><cachedir>` + escapeXML(filepath.Join(dir, "fontcache")) + `</cachedir></fontconfig>`
	return os.WriteFile(filepath.Join(dir, "fonts.conf"), []byte(fontConfig), 0600)
}

func (r *Runtime) command(ctx context.Context, dir, unit string) *exec.Cmd {
	args := []string{"-n", "systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--unit=" + unit, "--uid=" + strconv.Itoa(os.Getuid()), "--working-directory=" + dir,
		"--property=MemoryMax=536870912", "--property=MemorySwapMax=0", "--property=PrivateNetwork=yes", "--property=KillMode=control-group", "--property=RuntimeMaxSec=60", "--property=TimeoutStopSec=2", "--property=SendSIGKILL=yes", "--property=LimitFSIZE=268435456", "--property=TasksMax=512", "--property=NoNewPrivileges=yes", "--property=UMask=0077",
		"--setenv=LANG=C.UTF-8", "--setenv=LC_ALL=C.UTF-8", "--setenv=TZ=UTC", "--setenv=FONTCONFIG_FILE=" + filepath.Join(dir, "fonts.conf"), "--setenv=HOME=" + dir,
		r.config.Worker, "internal-email-pdf-worker", r.config.Chromium, dir, r.config.Version}
	cmd := exec.CommandContext(ctx, "sudo", args...) //nolint:gosec // Fixed systemd command and verified operator pin; email bytes are a separate local file.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

type runtimeDiagnostic struct{ bytes.Buffer }

func (b *runtimeDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		_, _ = b.Buffer.Write(p[:min(len(p), 4096-b.Len())])
	}
	return n, nil
}

func escapeXML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
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
