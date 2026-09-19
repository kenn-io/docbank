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
	trustedFODPWarmupSHA256 = "8a013927e44fddb11c48c3daca1ca00cf3863908182ed15cbcd39b886daf4635" //nolint:gosec // pinned trusted fixture digest
	trustedFODSWarmupSHA256 = "3bdc0e38457f1a658fb6175e0e172c5c871f2044fece114feed6863aa37bc3f8"
)

type formatProfile struct {
	ID            string
	kind          string
	inputName     string
	outputName    string
	normalizeMime string
	pdfFilter     string
	sourceWarmup  string
}

var renderProfiles = [...]formatProfile{
	{ID: "docx", kind: FlatTextKind, inputName: "source.docx", outputName: "source.fodt",
		normalizeMime: "OpenDocument Text Flat XML", pdfFilter: "writer_pdf_Export", sourceWarmup: "docx"},
	{ID: "doc", kind: FlatTextKind, inputName: "source.doc", outputName: "source.fodt",
		normalizeMime: "OpenDocument Text Flat XML", pdfFilter: "writer_pdf_Export", sourceWarmup: FlatTextKind},
	{ID: "odt", kind: FlatTextKind, inputName: "source.odt", outputName: "source.fodt",
		normalizeMime: "OpenDocument Text Flat XML", pdfFilter: "writer_pdf_Export", sourceWarmup: FlatTextKind},
	{ID: "rtf", kind: FlatTextKind, inputName: "source.rtf", outputName: "source.fodt",
		normalizeMime: "OpenDocument Text Flat XML", pdfFilter: "writer_pdf_Export", sourceWarmup: FlatTextKind},
	{ID: "ppt", kind: FlatPresKind, inputName: "source.ppt", outputName: "source.fodp",
		normalizeMime: "OpenDocument Presentation Flat XML", pdfFilter: "impress_pdf_Export", sourceWarmup: FlatPresKind},
	{ID: "xls", kind: FlatCalcKind, inputName: "source.xls", outputName: "source.fods",
		normalizeMime: "OpenDocument Spreadsheet Flat XML", pdfFilter: "calc_pdf_Export", sourceWarmup: FlatCalcKind},
	{ID: "ods", kind: FlatCalcKind, inputName: "source.ods", outputName: "source.fods",
		normalizeMime: "OpenDocument Spreadsheet Flat XML", pdfFilter: "calc_pdf_Export", sourceWarmup: FlatCalcKind},
	{ID: "xlsx", kind: FlatCalcKind, inputName: "source.xlsx", outputName: "source.fods",
		normalizeMime: "OpenDocument Spreadsheet Flat XML", pdfFilter: "calc_pdf_Export", sourceWarmup: FlatCalcKind},
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
		return nil, fmt.Errorf("%w: render PDF runner identity changed", ErrRendererChanged)
	}
	if err := source.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceRejected, err)
	}
	if !validExtension(extension) {
		return nil, fmt.Errorf("%w: render PDF extension must be lowercase alphanumeric", ErrSourceRejected)
	}
	if source.Size > policy.limits.MaxSourceBytes {
		return nil, fmt.Errorf("%w: render PDF source exceeds byte limit", ErrSourceTooLarge)
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
		return nil, fmt.Errorf("%w: render PDF source does not match declared size and SHA-256", ErrSourceRejected)
	}
	if err := runCtx.Err(); err != nil {
		return nil, err
	}
	candidate, err := formatdetect.DetectFormat(bytes.NewReader(content), int64(len(content)), source.MediaType)
	if err != nil {
		return nil, fmt.Errorf("%w: render PDF source format is invalid", ErrSourceRejected)
	}
	profile, ok := profileFor(candidate.ID)
	if !ok || extension != profile.ID {
		return nil, fmt.Errorf("%w: render PDF source format has no admitted profile", ErrSourceRejected)
	}
	if int64(len(content)) > policy.limits.MaxWorkBytes {
		return nil, fmt.Errorf("%w: render PDF source exceeds work-byte limit", ErrSourceTooLarge)
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
		return nil, fmt.Errorf("%w: render PDF normalized output exceeds byte limit", ErrOutputTooLarge)
	}
	if _, err := Scan(normalized, profile.kind, policy.limits); err != nil {
		return nil, fmt.Errorf("%w: render PDF normalized output is not admitted: %w", ErrSourceRejected, err)
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
		return nil, fmt.Errorf("%w: render PDF output exceeds byte limit", ErrOutputTooLarge)
	}
	pages, err := media.CountPDFPages(pdf)
	if err != nil {
		return nil, errors.New("render PDF output is not a valid PDF")
	}
	if pages <= 0 || pages > int64(policy.limits.MaxPages) {
		return nil, fmt.Errorf("%w: render PDF output exceeds page limit", ErrPageLimit)
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
	for index := range renderProfiles {
		profile := renderProfiles[index]
		if profile.ID == id {
			return profile, true
		}
	}
	return formatProfile{}, false
}

// Supports reports whether formatID has a configured render-to-PDF profile.
func Supports(formatID string) bool {
	_, ok := profileFor(formatID)
	return ok
}

func stageRequest(policy Policy, profile formatProfile, stage string, input []byte) Request {
	outputName := profile.outputName
	if stage == "pdf" {
		outputName = "source.pdf"
	}
	inputName := profile.inputName
	if stage == "pdf" {
		inputName = profile.outputName
	}
	stdinDigest := digest(input)
	warmup := warmupFixture(profile, stage)
	return Request{
		Stage: stage, Executable: policy.renderer.Executable,
		ExecutableSHA256: policy.renderer.ExecutableSHA256,
		Arguments:        stageArguments(profile, stage),
		Environment:      libreOfficeEnvironment(), Directory: filepath.Dir(policy.renderer.Executable),
		InputName: inputName, OutputName: outputName, Input: bytes.Clone(input), InputSHA256: stdinDigest,
		WarmupInput: warmup, WarmupInputSHA256: warmupFixtureDigest(profile, stage),
		MaxOutputBytes: stageOutputLimit(policy.limits, stage), MaxWorkBytes: policy.limits.MaxWorkBytes,
		PolicyFingerprint: policy.fingerprint, Runtime: slices.Clone(policy.renderer.Runtime),
		RuntimeSymlinks: slices.Clone(policy.renderer.RuntimeSymlinks), RuntimeIdentity: policy.renderer.RuntimeIdentity,
	}
}

func stageArguments(profile formatProfile, stage string) []string {
	if stage == "pdf" {
		return libreOfficeArguments(profile.outputName, "source.pdf", profile.pdfFilter)
	}
	return libreOfficeArguments(profile.inputName, profile.outputName, profile.normalizeMime)
}

func warmupFixture(profile formatProfile, stage string) []byte {
	return trustedWarmupFixture(warmupKey(profile, stage))
}

func warmupFixtureDigests() []string {
	keys := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	for index := range renderProfiles {
		profile := renderProfiles[index]
		for _, key := range []string{profile.sourceWarmup, profile.kind} {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	digests := make([]string, 0, len(keys))
	for _, key := range keys {
		digests = append(digests, trustedWarmupDigest(key))
	}
	return digests
}

func warmupFixtureDigest(profile formatProfile, stage string) string {
	return trustedWarmupDigest(warmupKey(profile, stage))
}

func warmupKey(profile formatProfile, stage string) string {
	if stage == "normalize" {
		return profile.sourceWarmup
	}
	return profile.kind
}

func trustedWarmupFixture(key string) []byte {
	switch key {
	case "docx":
		return trustedDOCXFixture()
	case FlatTextKind:
		return []byte(trustedFODTFixture)
	case FlatPresKind:
		return []byte(trustedFODPFixture)
	case FlatCalcKind:
		return []byte(trustedFODSFixture)
	default:
		return nil
	}
}

func trustedWarmupDigest(key string) string {
	switch key {
	case "docx":
		return trustedDOCXWarmupSHA256
	case FlatTextKind:
		return trustedFODTWarmupSHA256
	case FlatPresKind:
		return trustedFODPWarmupSHA256
	case FlatCalcKind:
		return trustedFODSWarmupSHA256
	default:
		return ""
	}
}

const trustedFODTFixture = `<?xml version="1.0" encoding="UTF-8"?><office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:mimetype="application/vnd.oasis.opendocument.text"><office:body><office:text><text:p>Docbank warm-up</text:p></office:text></office:body></office:document>`

const trustedFODPFixture = `<?xml version="1.0" encoding="UTF-8"?><office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" office:version="1.3" office:mimetype="application/vnd.oasis.opendocument.presentation"><office:body><office:presentation><draw:page draw:name="Slide 1"><draw:frame svg:x="1cm" svg:y="1cm" svg:width="10cm" svg:height="3cm"><draw:text-box><text:p>Docbank warm-up</text:p></draw:text-box></draw:frame></draw:page></office:presentation></office:body></office:document>`

const trustedFODSFixture = `<?xml version="1.0" encoding="UTF-8"?><office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.3" office:mimetype="application/vnd.oasis.opendocument.spreadsheet"><office:body><office:spreadsheet><table:table table:name="Sheet 1"><table:table-row><table:table-cell office:value-type="string"><text:p>Docbank warm-up</text:p></table:table-cell></table:table-row></table:table></office:spreadsheet></office:body></office:document>`

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
		return fmt.Errorf("%w: render PDF runner identity changed", ErrRendererChanged)
	}
	if _, err := providerutil.LoadPinnedExecutable(policy.renderer.Executable, policy.renderer.ExecutableSHA256, MaxExecutableBytes); err != nil {
		return fmt.Errorf("%w: render PDF renderer identity changed: %w", ErrRendererChanged, err)
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
