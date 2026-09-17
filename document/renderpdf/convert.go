package renderpdf

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/ocr"
)

const (
	docxMediaType           = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	trustedDOCXWarmupSHA256 = "41186d7d336070c089c233ef1caa439eefcdb7e502b37da7debd46ecc1495cfd"
	trustedFODTWarmupSHA256 = "ce828df33329c6e60317883a2c0f26a81a60f590497dee9b20625131acb25814"
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
				err = errors.Join(err, fmt.Errorf("close render PDF source: %w", closeErr))
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
			return nil, fmt.Errorf("render PDF conversion timed out: %w", runCtxErr)
		}
		return nil, fmt.Errorf("read render PDF source: %w", err)
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
	normalizedResult, err := policy.runner.Run(runCtx, normalizeRequest)
	if err != nil {
		return nil, stageError(runCtx, "normalize", err)
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
	pdfResult, err := policy.runner.Run(runCtx, pdfRequest)
	if err != nil {
		return nil, stageError(runCtx, "render", err)
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
	return &Result{pdf: pdf, receipt: receipt}, nil
}

func profileFor(id string) (formatProfile, bool) {
	switch id {
	case "docx":
		return formatProfile{ID: "docx", kind: FlatTextKind, inputName: "source.docx", outputName: "source.fodt", normalizeMime: "OpenDocument Text Flat XML", pdfFilter: "writer_pdf_Export"}, true
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
	warmup := warmupFixture(stage)
	return Request{
		Stage: stage, Executable: policy.renderer.Executable,
		ExecutableSHA256: policy.renderer.ExecutableSHA256,
		Arguments:        libreOfficeArguments(inputName, outputName, filter),
		Environment:      libreOfficeEnvironment(), Directory: filepath.Dir(policy.renderer.Executable),
		InputName: inputName, OutputName: outputName, Input: bytes.Clone(input), InputSHA256: stdinDigest,
		WarmupInput: warmup, WarmupInputSHA256: warmupFixtureDigest(stage),
		MaxOutputBytes: stageOutputLimit(policy.limits, stage), MaxWorkBytes: policy.limits.MaxWorkBytes,
		PolicyFingerprint: policy.fingerprint, Runtime: slices.Clone(policy.renderer.Runtime),
		RuntimeSymlinks: slices.Clone(policy.renderer.RuntimeSymlinks), RuntimeIdentity: policy.renderer.RuntimeIdentity,
	}
}

func warmupFixture(stage string) []byte {
	if stage == "normalize" {
		return trustedDOCXFixture()
	}
	return []byte(trustedFODTFixture)
}

func warmupFixtureDigests() []string {
	return []string{trustedDOCXWarmupSHA256, trustedFODTWarmupSHA256}
}

func warmupFixtureDigest(stage string) string {
	if stage == "normalize" {
		return trustedDOCXWarmupSHA256
	}
	return trustedFODTWarmupSHA256
}

const trustedFODTFixture = `<?xml version="1.0" encoding="UTF-8"?><office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:mimetype="application/vnd.oasis.opendocument.text"><office:body><office:text><text:p>Docbank warm-up</text:p></office:text></office:body></office:document>`

func trustedDOCXFixture() []byte {
	entries := []struct{ name, value string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
		{"word/document.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Docbank warm-up</w:t></w:r></w:p><w:sectPr><w:pgSz w:w="12240" w:h="15840"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440"/></w:sectPr></w:body></w:document>`},
		{"word/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style></w:styles>`},
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, entry := range entries {
		data := []byte(entry.value)
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		header.Modified = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)
		header.CRC32 = crc32.ChecksumIEEE(data)
		header.CompressedSize64 = uint64(len(data))
		header.UncompressedSize64 = uint64(len(data))
		file, err := archive.CreateRaw(header)
		if err != nil {
			return nil
		}
		_, _ = file.Write(data)
	}
	if err := archive.Close(); err != nil {
		return nil
	}
	return buffer.Bytes()
}

func libreOfficeArguments(inputName, outputName, filter string) []string {
	return []string{
		"--headless", "--norestore", "--nolockcheck", "--nodefault", "--nofirststartwizard",
		"-env:UserInstallation=file:///work/profile",
		"--convert-to", extensionForFilter(outputName) + ":" + filter,
		"--outdir", "/work", "/work/" + inputName,
	}
}

func extensionForFilter(outputName string) string {
	return strings.TrimPrefix(filepath.Ext(outputName), ".")
}

func libreOfficeEnvironment() []string {
	return []string{
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC",
		"HOME=/work/home", "TMPDIR=/tmp", "XDG_CACHE_HOME=/work/home/cache",
		"PATH=/usr/bin:/bin",
		"LD_LIBRARY_PATH=/usr/lib/libreoffice/program",
	}
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
		return fmt.Errorf("render PDF renderer identity changed: %w", err)
	}
	return nil
}

func validateStageResult(request Request, result StageResult, runnerIdentity string) ([]byte, error) {
	output := result.Output
	if len(output) == 0 || int64(len(output)) > request.MaxOutputBytes {
		return nil, errors.New("stage output is outside its byte bound")
	}
	attestation := result.Attestation
	if attestation.RunnerIdentity != runnerIdentity || attestation.PolicyFingerprint != request.PolicyFingerprint ||
		attestation.ExecutableSHA256 != request.ExecutableSHA256 || attestation.InputSHA256 != request.InputSHA256 ||
		attestation.OutputSHA256 != digest(output) || !attestation.NetworkDisabled ||
		!attestation.ProcessTreeContained || !attestation.DigestVerifiedLaunch || !attestation.FilesystemIsolated ||
		!attestation.PrivateRootInstalled || attestation.FilesystemMode != "private-root-v1" ||
		attestation.RuntimeIdentity != request.RuntimeIdentity || !attestation.UnixIPCAllowed {
		return nil, errors.New("stage did not attest exact policy and bytes")
	}
	return bytes.Clone(output), nil
}

func stageError(ctx context.Context, stage string, cause error) error {
	if contextErr := ctx.Err(); contextErr != nil && !errors.Is(cause, contextErr) {
		cause = errors.Join(contextErr, cause)
	}
	return fmt.Errorf("render PDF %s stage failed: %w", stage, cause)
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
