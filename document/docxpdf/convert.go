package docxpdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/ocr"
)

const (
	docxMediaType    = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	childDrainWindow = 250 * time.Millisecond
)

var errRendererIdentityChanged = errors.New("DOCX renderer identity changed")

// Convert consumes and closes source.Content on every path, then returns the
// exact PDF bytes produced by the pinned renderer.
func Convert(ctx context.Context, source ocr.Source, policy Policy) (result *Result, err error) {
	if source.Content != nil {
		defer func() {
			if closeErr := source.Content.Close(); closeErr != nil {
				result = nil
				err = errors.Join(err, errors.New("close DOCX source failed"))
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy.fingerprint == "" {
		return nil, errors.New("DOCX PDF policy is invalid; use NewPolicy")
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if source.MediaType != docxMediaType {
		return nil, errors.New("DOCX source requires the Word media type")
	}
	if source.Size > policy.limits.MaxSourceBytes {
		return nil, errors.New("DOCX source exceeds byte limit")
	}
	content, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: source.Content}, source.Size+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("read DOCX source failed")
	}
	if int64(len(content)) != source.Size || digest(content) != source.SHA256 {
		return nil, errors.New("DOCX source does not match declared size and SHA-256")
	}
	candidate, err := formatdetect.DetectFormat(bytes.NewReader(content), int64(len(content)), source.MediaType)
	if err != nil || candidate.ID != "docx" {
		return nil, errors.New("DOCX source is not a Word document")
	}
	if _, err := providerutil.LoadPinnedExecutable(
		policy.renderer.Executable, policy.renderer.ExecutableSHA256, MaxExecutableBytes,
	); err != nil {
		return nil, errRendererIdentityChanged
	}
	work, err := os.MkdirTemp("", "docbank-docxpdf-")
	if err != nil {
		return nil, errors.New("create DOCX conversion work directory failed")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(work); cleanupErr != nil {
			result = nil
			err = errors.Join(err, errors.New("remove DOCX conversion work directory failed"))
		}
	}()
	for _, directory := range []string{"input", "output", "profile", "home", "tmp"} {
		if err := os.Mkdir(filepath.Join(work, directory), 0o700); err != nil {
			return nil, errors.New("create DOCX conversion directory failed")
		}
	}
	input, err := os.OpenFile(filepath.Join(work, "input", "source.docx"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.New("create DOCX conversion input failed")
	}
	written, writeErr := input.Write(content)
	closeErr := input.Close()
	if writeErr != nil || written != len(content) {
		return nil, errors.New("write DOCX conversion input failed")
	}
	if closeErr != nil {
		return nil, errors.New("close DOCX conversion input failed")
	}
	runCtx, cancel := context.WithTimeout(ctx, policy.limits.Timeout)
	defer cancel()
	runErr := runRenderer(runCtx, policy, work)
	if errors.Is(runErr, errRendererIdentityChanged) {
		return nil, runErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if runCtx.Err() != nil {
		return nil, errors.New("DOCX renderer timed out")
	}
	if runErr != nil {
		return nil, errors.New("DOCX renderer failed")
	}
	pdf, err := readOutput(filepath.Join(work, "output", "source.pdf"), policy.limits.MaxPDFBytes)
	if err != nil {
		return nil, err
	}
	count, err := media.CountPDFPages(pdf)
	if err != nil {
		return nil, fmt.Errorf("verify generated PDF: %w", err)
	}
	if count <= 0 || count > int64(policy.limits.MaxPages) {
		return nil, errors.New("generated PDF exceeds page limit or has no pages")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Result{pdf: pdf, receipt: Receipt{
		SourceSHA256: source.SHA256, SourceBytes: source.Size,
		PDFSHA256: digest(pdf), PDFBytes: int64(len(pdf)), Pages: int(count),
		PolicyFingerprint: policy.fingerprint, ConverterVersion: ConverterVersion,
	}}, nil
}

func runRenderer(ctx context.Context, policy Policy, work string) error {
	for attempt := 0; ; attempt++ {
		if _, err := providerutil.LoadPinnedExecutable(
			policy.renderer.Executable, policy.renderer.ExecutableSHA256, MaxExecutableBytes,
		); err != nil {
			return errRendererIdentityChanged
		}
		command := exec.CommandContext( //nolint:gosec // the operator pins one executable; document bytes never select arguments
			ctx, policy.renderer.Executable, rendererArguments(work)...,
		)
		command.Dir = work
		command.Env = rendererEnvironment(work)
		command.Stdin = nil
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		command.WaitDelay = childDrainWindow
		managed, err := providerutil.NewManagedCommand(command)
		if err != nil {
			return errors.New("prepare DOCX renderer failed")
		}
		runErr := managed.Run()
		if runtime.GOOS != "windows" && attempt == 0 && ctx.Err() == nil && isRendererNormalRestart(runErr) {
			continue
		}
		return runErr
	}
}

func isRendererNormalRestart(err error) bool {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	return ok && exitErr.ExitCode() == 81
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func rendererArguments(work string) []string {
	return []string{
		"--headless", "--norestore", "--nolockcheck",
		"-env:UserInstallation=" + profileURL(filepath.Join(work, "profile")),
		"--convert-to", "pdf:writer_pdf_Export", "--outdir", filepath.Join(work, "output"),
		filepath.Join(work, "input", "source.docx"),
	}
}

func profileURL(directory string) string {
	p := filepath.ToSlash(directory)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func rendererEnvironment(work string) []string {
	environment := []string{"TZ=UTC", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"SystemRoot", "SystemDrive", "windir"} {
			if value := os.Getenv(name); value != "" {
				environment = append(environment, name+"="+value)
			}
		}
		home := filepath.Join(work, "home")
		temp := filepath.Join(work, "tmp")
		environment = append(environment,
			"TEMP="+temp, "TMP="+temp,
			"USERPROFILE="+home, "APPDATA="+home, "LOCALAPPDATA="+home,
		)
		return environment
	}
	environment = append(environment,
		"HOME="+filepath.Join(work, "home"),
		"TMPDIR="+filepath.Join(work, "tmp"),
		"PATH=/usr/bin:/bin",
	)
	return environment
}

func readOutput(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, errors.New("DOCX renderer produced no PDF")
	}
	if info.Size() > maxBytes {
		return nil, errors.New("generated PDF exceeds byte limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("DOCX renderer produced no PDF")
	}
	opened, err := file.Stat()
	if err != nil || opened.Mode()&os.ModeSymlink != 0 || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, errors.New("DOCX renderer produced no PDF")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) != opened.Size() {
		return nil, errors.New("DOCX renderer produced no PDF")
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("generated PDF exceeds byte limit")
	}
	// ponytail: renderer output size is checked after exit, so a hostile renderer can fill temporary space until Timeout; revisit when a sandboxed renderer runtime or a filesystem quota is available.
	return data, nil
}
