package renderpdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/ocr"
)

const (
	docxMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	xlsxMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

type formatProfile struct {
	ID            string
	kind          string
	inputName     string
	outputName    string
	normalizeMime string
	pdfFilter     string
}

// Convert consumes and closes source.Content on every path, then returns the
// exact counted PDF produced from admitted normalized bytes.
func Convert(ctx context.Context, source ocr.Source, extension string, policy Policy) (result *Result, err error) {
	if source.Content != nil {
		defer func() {
			if closeErr := source.Content.Close(); closeErr != nil {
				result = nil
				err = errors.Join(err, errors.New("close render PDF source failed"))
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy.fingerprint == "" || policy.runner == nil {
		return nil, errors.New("render PDF policy is invalid; use NewPolicy")
	}
	if policy.runner.Identity() != policy.runnerID {
		return nil, errors.New("render PDF runner identity changed")
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if !validExtension(extension) {
		return nil, errors.New("render PDF extension must be lowercase alphanumeric")
	}
	if source.Size > policy.limits.MaxSourceBytes {
		return nil, errors.New("render PDF source exceeds byte limit")
	}
	runCtx, cancel := context.WithTimeout(ctx, policy.limits.Timeout)
	defer cancel()
	content, err := io.ReadAll(io.LimitReader(contextReader{ctx: runCtx, reader: source.Content}, source.Size+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if runCtxErr := runCtx.Err(); runCtxErr != nil {
			return nil, errors.New("render PDF conversion timed out")
		}
		return nil, errors.New("read render PDF source failed")
	}
	if int64(len(content)) != source.Size || digest(content) != source.SHA256 {
		return nil, errors.New("render PDF source does not match declared size and SHA-256")
	}
	if err := runCtx.Err(); err != nil {
		return nil, err
	}
	candidate, err := formatdetect.DetectFormat(bytes.NewReader(content), int64(len(content)), source.MediaType)
	if err != nil {
		return nil, errors.New("render PDF source format is invalid")
	}
	profile, ok := profileFor(candidate.ID)
	if !ok || extension != profile.ID {
		return nil, errors.New("render PDF source format has no admitted profile")
	}
	if int64(len(content)) > policy.limits.MaxWorkBytes {
		return nil, errors.New("render PDF source exceeds work-byte limit")
	}
	if err := verifyRenderer(policy); err != nil {
		return nil, err
	}
	normalizeRequest := stageRequest(policy, profile, "normalize", content)
	normalizedResult, err := runStage(runCtx, policy, normalizeRequest)
	if err != nil {
		return nil, stageError(runCtx, "normalize")
	}
	normalized, err := validateStageResult(normalizeRequest, normalizedResult, policy.runnerID)
	if err != nil {
		return nil, errors.New("render PDF normalization attestation is invalid")
	}
	if int64(len(normalized)) > policy.limits.MaxNormalizedBytes || int64(len(normalized)) > policy.limits.MaxWorkBytes {
		return nil, errors.New("render PDF normalized output exceeds byte limit")
	}
	if _, err := Scan(normalized, profile.kind, policy.limits); err != nil {
		return nil, fmt.Errorf("render PDF normalized output is not admitted: %w", err)
	}
	if err := runCtx.Err(); err != nil {
		return nil, err
	}
	if err := verifyRenderer(policy); err != nil {
		return nil, err
	}
	pdfRequest := stageRequest(policy, profile, "pdf", normalized)
	pdfResult, err := runStage(runCtx, policy, pdfRequest)
	if err != nil {
		return nil, stageError(runCtx, "render")
	}
	pdf, err := validateStageResult(pdfRequest, pdfResult, policy.runnerID)
	if err != nil {
		return nil, errors.New("render PDF attestation is invalid")
	}
	if int64(len(pdf)) > policy.limits.MaxPDFBytes {
		return nil, errors.New("render PDF output exceeds byte limit")
	}
	pages, err := media.CountPDFPages(pdf)
	if err != nil {
		return nil, errors.New("render PDF output is not a valid PDF")
	}
	if pages <= 0 || pages > int64(policy.limits.MaxPages) {
		return nil, errors.New("render PDF output exceeds page limit")
	}
	if err := runCtx.Err(); err != nil {
		return nil, err
	}
	normalizedDigest := digest(normalized)
	pdfDigest := digest(pdf)
	receipt := Receipt{
		SourceFormat: candidate.ID, OriginalFormat: candidate.ID, SourceExtension: extension,
		SourceSHA256: source.SHA256, SourceBytes: source.Size,
		NormalizedSHA256: normalizedDigest, NormalizedBytes: int64(len(normalized)),
		PDFSHA256: pdfDigest, PDFBytes: int64(len(pdf)), Pages: int(pages),
		PolicyFingerprint: policy.fingerprint, ConverterVersion: ConverterVersion,
		RuntimeIdentity: policy.renderer.RuntimeIdentity, RunnerIdentity: policy.runnerID,
	}
	return &Result{pdf: bytes.Clone(pdf), receipt: receipt}, nil
}

func profileFor(id string) (formatProfile, bool) {
	switch id {
	case "docx":
		return formatProfile{ID: "docx", kind: FlatTextKind, inputName: "source.docx", outputName: "source.fodt", normalizeMime: "OpenDocument Text Flat XML", pdfFilter: "writer_pdf_Export"}, true
	case "xlsx":
		return formatProfile{ID: "xlsx", kind: FlatSpreadsheetKind, inputName: "source.xlsx", outputName: "source.fods", normalizeMime: "OpenDocument Spreadsheet Flat XML", pdfFilter: "calc_pdf_Export"}, true
	default:
		return formatProfile{}, false
	}
}

func stageRequest(policy Policy, profile formatProfile, stage string, input []byte) Request {
	outputName := profile.outputName
	filter := profile.normalizeMime
	if stage == "pdf" {
		outputName = "source.pdf"
		filter = profile.pdfFilter
	}
	inputName := profile.inputName
	if stage == "pdf" {
		inputName = profile.outputName
	}
	stdinDigest := digest(input)
	return Request{
		Stage: stage, Executable: policy.renderer.Executable,
		ExecutableSHA256: policy.renderer.ExecutableSHA256,
		Arguments:        libreOfficeArguments(inputName, outputName, filter),
		Environment:      libreOfficeEnvironment(), Directory: filepath.Dir(policy.renderer.Executable),
		InputName: inputName, OutputName: outputName, Input: bytes.Clone(input), InputSHA256: stdinDigest,
		MaxOutputBytes: stageOutputLimit(policy.limits, stage), MaxWorkBytes: policy.limits.MaxWorkBytes,
		PolicyFingerprint: policy.fingerprint, AllowLocalIPC: true,
	}
}

func libreOfficeArguments(inputName, outputName, filter string) []string {
	return []string{
		"--headless", "--norestore", "--nolockcheck", "--nodefault", "--nofirststartwizard",
		"-env:UserInstallation=" + profileURL("/tmp/work/profile"),
		"--convert-to", extensionForFilter(outputName) + ":" + filter,
		"--outdir", "/tmp/work", "/tmp/work/" + inputName,
	}
}

func extensionForFilter(outputName string) string {
	return strings.TrimPrefix(filepath.Ext(outputName), ".")
}

func libreOfficeEnvironment() []string {
	return []string{
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC",
		"HOME=/tmp/work/home", "TMPDIR=/tmp/work/tmp", "PATH=/usr/bin:/bin",
		"LD_LIBRARY_PATH=/usr/lib/libreoffice/program",
	}
}

func profileURL(directory string) string {
	p := filepath.ToSlash(directory)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func stageOutputLimit(limits Limits, stage string) int64 {
	if stage == "normalize" {
		return limits.MaxNormalizedBytes
	}
	return limits.MaxPDFBytes
}

func verifyRenderer(policy Policy) error {
	if policy.runner == nil || policy.runner.Identity() != policy.runnerID {
		return errors.New("render PDF runner identity changed")
	}
	if _, err := providerutil.LoadPinnedExecutable(policy.renderer.Executable, policy.renderer.ExecutableSHA256, MaxExecutableBytes); err != nil {
		return errors.New("render PDF renderer identity changed")
	}
	return nil
}

func runStage(ctx context.Context, policy Policy, request Request) (StageResult, error) {
	result, err := policy.runner.Run(ctx, request)
	if !errors.Is(err, sandbox.ErrNormalRestart) || ctx.Err() != nil {
		return result, err
	}
	if err := verifyRenderer(policy); err != nil {
		return StageResult{}, err
	}
	return policy.runner.Run(ctx, request)
}

func validateStageResult(request Request, result StageResult, runnerIdentity string) ([]byte, error) {
	if result.Output != nil && result.Stdout != nil && !bytes.Equal(result.Output, result.Stdout) {
		return nil, errors.New("stage returned two different outputs")
	}
	output := result.Output
	if output == nil {
		output = result.Stdout
	}
	if len(output) == 0 || int64(len(output)) > request.MaxOutputBytes {
		return nil, errors.New("stage output is outside its byte bound")
	}
	attestation := result.Attestation
	if attestation.RunnerIdentity != runnerIdentity || attestation.PolicyFingerprint != request.PolicyFingerprint ||
		attestation.ExecutableSHA256 != request.ExecutableSHA256 || attestation.InputSHA256 != request.InputSHA256 ||
		attestation.OutputSHA256 != digest(output) || !attestation.NetworkDisabled ||
		!attestation.ProcessTreeContained || !attestation.DigestVerifiedLaunch || !attestation.FilesystemIsolated ||
		attestation.LocalIPCAllowed != request.AllowLocalIPC {
		return nil, errors.New("stage did not attest exact policy and bytes")
	}
	return bytes.Clone(output), nil
}

func stageError(ctx context.Context, stage string) error {
	if contextErr := ctx.Err(); contextErr != nil {
		if errors.Is(contextErr, context.DeadlineExceeded) {
			return errors.New("render PDF conversion timed out")
		}
		return contextErr
	}
	return fmt.Errorf("render PDF %s stage failed", stage)
}

func validExtension(extension string) bool {
	if extension == "" {
		return false
	}
	for _, char := range extension {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(value []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.reader.Read(value)
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}

var _ = docxMediaType
var _ = xlsxMediaType
