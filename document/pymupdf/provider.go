package pymupdf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const (
	providerID       = "pymupdf.local-v1"
	protocolVersion  = "docbank-pymupdf/v1"
	profileVersion   = "docbank-pymupdf-profile/v1"
	timestampForm    = "2006-01-02T15:04:05.000000000Z"
	childDrainWindow = 250 * time.Millisecond

	// MaxDocumentBytes is the largest PDF accepted by a local provider profile.
	MaxDocumentBytes = formatdetect.MaxDocumentBytes
	// MaxExecutableBytes is the largest pinned bridge executable accepted by a profile.
	MaxExecutableBytes = int64(512 << 20)
	// MaxResponseBytes is the largest structured child response accepted by a profile.
	MaxResponseBytes = int64(64 << 20)
	// MaxPages is the largest locally verified page sequence accepted by a profile.
	MaxPages = 100_000
	// MaxTimeout is the largest local child deadline accepted by a profile.
	MaxTimeout = 30 * time.Minute
)

var (
	errOutputTooLarge = errors.New("child output exceeds limit")
)

// Profile fixes one executable, immutable runtime identity, and all local bounds.
type Profile struct {
	Executable       string
	ExecutableSHA256 string
	RuntimeIdentity  string
	MaxDocumentBytes int64
	MaxResponseBytes int64
	MaxPages         int
	Timeout          time.Duration
}

// Provider renders authorized PDF bytes through one directly configured executable.
type Provider struct {
	descriptor       document.RenditionDescriptor
	executable       string
	pinnedExecutable *providerutil.PinnedExecutable
	runtimeIdentity  string
	maxDocumentBytes int64
	maxResponseBytes int64
	maxPages         int
	timeout          time.Duration
}

// New constructs one immutable local-process provider profile.
func New(profile Profile) (*Provider, error) {
	if !filepath.IsAbs(profile.Executable) || filepath.Clean(profile.Executable) != profile.Executable {
		return nil, errors.New("pymupdf: executable must be an absolute clean path")
	}
	if pythonInterpreter(filepath.Base(profile.Executable)) {
		return nil, errors.New("pymupdf: executable must not be a Python interpreter")
	}
	pinnedExecutable, err := providerutil.LoadPinnedExecutable(
		profile.Executable, profile.ExecutableSHA256, MaxExecutableBytes)
	if err != nil {
		return nil, fmt.Errorf("pymupdf: pin executable: %w", err)
	}
	if err := validateRuntimeIdentity(profile.RuntimeIdentity); err != nil {
		return nil, err
	}
	if profile.MaxDocumentBytes <= 0 || profile.MaxDocumentBytes > MaxDocumentBytes {
		return nil, fmt.Errorf("pymupdf: max document bytes must be between 1 and %d", MaxDocumentBytes)
	}
	if profile.MaxResponseBytes <= 0 || profile.MaxResponseBytes > MaxResponseBytes {
		return nil, fmt.Errorf("pymupdf: max response bytes must be between 1 and %d", MaxResponseBytes)
	}
	if profile.MaxPages <= 0 || profile.MaxPages > MaxPages {
		return nil, fmt.Errorf("pymupdf: max pages must be between 1 and %d", MaxPages)
	}
	if profile.Timeout <= 0 || profile.Timeout > MaxTimeout {
		return nil, fmt.Errorf("pymupdf: timeout must be between 1ns and %s", MaxTimeout)
	}
	identity := strings.Join([]string{
		profileVersion, protocolVersion, profile.Executable, profile.ExecutableSHA256, profile.RuntimeIdentity,
		strconv.FormatInt(profile.MaxDocumentBytes, 10), strconv.FormatInt(profile.MaxResponseBytes, 10),
		strconv.Itoa(profile.MaxPages), strconv.FormatInt(int64(profile.Timeout), 10),
	}, "\x00")
	policyDigest := sha256.Sum256([]byte(identity))
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: providerID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: hex.EncodeToString(policyDigest[:]),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{{
			MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile,
		}},
		ReturnsStructured: true,
	})
	if err != nil {
		return nil, fmt.Errorf("pymupdf: construct descriptor: %w", err)
	}
	return &Provider{
		descriptor: providerutil.CloneDescriptor(descriptor), executable: profile.Executable,
		pinnedExecutable: pinnedExecutable,
		runtimeIdentity:  profile.RuntimeIdentity, maxDocumentBytes: profile.MaxDocumentBytes,
		maxResponseBytes: profile.MaxResponseBytes, maxPages: profile.MaxPages, timeout: profile.Timeout,
	}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (provider *Provider) Descriptor() document.RenditionDescriptor {
	if provider == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(provider.descriptor)
}

// Render re-verifies one PDF and sends its exact authorized bytes through stdin only.
func (provider *Provider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if provider == nil {
		return document.RenditionResult{}, errors.New("pymupdf: provider is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > provider.maxDocumentBytes {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"PyMuPDF input exceeds the configured byte limit", nil)
	}
	expiresAt, err := time.Parse(timestampForm, authorization.ExpiresAt)
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"PyMuPDF authorization is invalid", err)
	}
	startedAt := time.Now().UTC()
	timeoutAt := startedAt.Add(provider.timeout)
	deadline := timeoutAt
	expiryDeadline := false
	if expiresAt.Before(deadline) {
		deadline = expiresAt
		expiryDeadline = true
	}
	operationCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := operationCtx.Err(); err != nil {
		return document.RenditionResult{}, provider.contextError(ctx, expiryDeadline, err)
	}

	stopInterrupt := context.AfterFunc(operationCtx, func() { _ = document.InterruptAuthorizedUpload(upload) })
	source, err := providerutil.ReadAuthorizedUpload(operationCtx, upload, metadata, "PyMuPDF")
	stopInterrupt()
	if err != nil {
		if operationCtx.Err() != nil {
			return document.RenditionResult{}, provider.contextError(ctx, expiryDeadline, operationCtx.Err())
		}
		return document.RenditionResult{}, err
	}
	defer clear(source)
	candidate, err := formatdetect.DetectFormat(bytes.NewReader(source), int64(len(source)), metadata.MediaType)
	if err != nil || candidate.ID != "pdf" || candidate.Family != "pdf" {
		return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput,
			"PyMuPDF input is not a locally verified PDF", err)
	}
	localPages, err := formatdetect.CountPDFPages(source)
	if err != nil || localPages <= 0 {
		return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput,
			"PyMuPDF page count could not be verified", err)
	}
	if localPages > int64(provider.maxPages) {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"PyMuPDF PDF exceeds the configured page limit", nil)
	}
	executable, cleanupExecutable, err := provider.pinnedExecutable.Materialize()
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"PyMuPDF executable identity changed", err)
	}
	cleanupExecutable = sync.OnceValue(cleanupExecutable)
	defer func() { _ = cleanupExecutable() }()

	responseLimit := min(provider.maxResponseBytes, int64(authorization.MaxTotalResultBytes))
	stdout := &boundedBuffer{limit: responseLimit}
	command := exec.CommandContext( //nolint:gosec // the operator pins one direct executable; source bytes never select it
		operationCtx, executable, "--protocol", protocolVersion,
	)
	command.Dir = filepath.Dir(executable)
	command.Env = cleanEnvironment()
	command.Stdin = bytes.NewReader(source)
	command.Stdout = stdout
	command.Stderr = io.Discard
	command.WaitDelay = childDrainWindow
	managedCommand, err := providerutil.NewManagedCommand(command)
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorTransient,
			"PyMuPDF executable could not be isolated", err)
	}
	stdout.overflow = func() { _ = managedCommand.Kill() }
	runErr := managedCommand.Run()
	cleanupErr := cleanupExecutable()
	if operationCtx.Err() != nil {
		return document.RenditionResult{}, provider.contextError(ctx, expiryDeadline, operationCtx.Err())
	}
	if cleanupErr != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorTransient,
			"PyMuPDF executable cleanup failed", cleanupErr)
	}
	if stdout.exceeded {
		return document.RenditionResult{}, providerError(document.RenditionErrorMalformedEvidence,
			"PyMuPDF output exceeds the configured byte limit", nil)
	}
	if runErr != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorTransient,
			"PyMuPDF executable failed", runErr)
	}

	wire, err := parseResponse(stdout.Bytes(), provider.runtimeIdentity, metadata, int(localPages), provider.maxPages)
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorMalformedEvidence,
			"PyMuPDF output is malformed", err)
	}
	if err := provider.postProcessError(ctx, operationCtx, deadline, expiryDeadline); err != nil {
		return document.RenditionResult{}, err
	}
	evidence := document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: make([]document.SourceEvidenceUnitV1, len(wire.Pages)),
	}
	for index, page := range wire.Pages {
		evidence.Units[index] = document.SourceEvidenceUnitV1{
			Order: index, ProviderID: fmt.Sprintf("pymupdf-page-%d", page.Number), Text: page.Text,
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: int64(page.Number), End: int64(page.Number),
			},
		}
	}
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorMalformedEvidence,
			"PyMuPDF output is malformed", err)
	}
	if err := provider.postProcessError(ctx, operationCtx, deadline, expiryDeadline); err != nil {
		return document.RenditionResult{}, err
	}
	completedAt := time.Now().UTC()
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"PyMuPDF authorization fingerprint is invalid", err)
	}
	result := document.RenditionResult{
		Evidence: evidence,
		Receipt: document.RenditionReceipt{
			ProviderID: provider.descriptor.ID, DescriptorFingerprint: provider.descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint,
			SourceSHA256:                metadata.SHA256,
			OperationID:                 "pymupdf-" + authorization.RenditionRequestFingerprint[:24],
			StartedAt:                   startedAt.Format(timestampForm), CompletedAt: completedAt.Format(timestampForm),
			Usage: document.RenditionUsage{
				Requests: 1, InputBytes: metadata.ByteLength, OutputBytes: int64(stdout.Len()), Units: localPages,
			},
		},
	}
	if err := provider.postProcessError(ctx, operationCtx, deadline, expiryDeadline); err != nil {
		return document.RenditionResult{}, err
	}
	return result, nil
}

type response struct {
	ContractVersion string `json:"contract_version"`
	RuntimeIdentity string `json:"runtime_identity"`
	SourceSHA256    string `json:"source_sha256"`
	SourceBytes     int64  `json:"source_bytes"`
	Complete        bool   `json:"complete"`
	PageCount       int    `json:"page_count"`
	Pages           []page `json:"pages"`
}

type page struct {
	Number      int    `json:"number"`
	Text        string `json:"text"`
	EmptyReason string `json:"empty_reason,omitempty"`
}

func parseResponse(
	raw []byte, runtimeIdentity string, metadata document.AuthorizedUploadMetadata,
	localPages, maxPages int,
) (response, error) {
	if len(raw) == 0 {
		return response{}, errors.New("empty response")
	}
	var wire response
	if err := json.Unmarshal(raw, &wire, json.RejectUnknownMembers(true)); err != nil {
		return response{}, err
	}
	if wire.ContractVersion != protocolVersion {
		return response{}, errors.New("protocol version changed")
	}
	if wire.RuntimeIdentity != runtimeIdentity {
		return response{}, errors.New("runtime identity changed")
	}
	if wire.SourceSHA256 != metadata.SHA256 || wire.SourceBytes != metadata.ByteLength {
		return response{}, errors.New("source identity changed")
	}
	if !wire.Complete {
		return response{}, errors.New("response is partial")
	}
	if wire.PageCount <= 0 || wire.PageCount > maxPages || wire.PageCount != localPages || len(wire.Pages) != wire.PageCount {
		return response{}, errors.New("page count changed")
	}
	for index, page := range wire.Pages {
		if page.Number != index+1 {
			return response{}, errors.New("page sequence is not complete and contiguous")
		}
		if !utf8.ValidString(page.Text) || strings.ContainsRune(page.Text, '\x00') {
			return response{}, errors.New("page text is invalid")
		}
		empty := strings.TrimSpace(page.Text) == ""
		if empty == (strings.TrimSpace(page.EmptyReason) == "") {
			return response{}, errors.New("page emptiness is not explained exactly once")
		}
		if page.EmptyReason != "" && !validExplanation(page.EmptyReason) {
			return response{}, errors.New("empty page explanation is invalid")
		}
	}
	return wire, nil
}

type boundedBuffer struct {
	data     bytes.Buffer
	limit    int64
	exceeded bool
	overflow func()
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - int64(buffer.data.Len())
	if remaining <= 0 {
		buffer.failOverflow()
		return 0, errOutputTooLarge
	}
	if int64(len(data)) > remaining {
		written, _ := buffer.data.Write(data[:remaining])
		buffer.failOverflow()
		return written, errOutputTooLarge
	}
	written, _ := buffer.data.Write(data)
	return written, nil
}

func (buffer *boundedBuffer) Bytes() []byte { return buffer.data.Bytes() }

func (buffer *boundedBuffer) Len() int { return buffer.data.Len() }

func (buffer *boundedBuffer) failOverflow() {
	if buffer.exceeded {
		return
	}
	buffer.exceeded = true
	if buffer.overflow != nil {
		buffer.overflow()
	}
}

func (provider *Provider) contextError(parent context.Context, expiry bool, cause error) error {
	if parent.Err() != nil {
		return providerError(document.RenditionErrorCanceled, "PyMuPDF rendering canceled", parent.Err())
	}
	if expiry {
		return providerError(document.RenditionErrorPolicyRejected,
			"PyMuPDF authorization expired during rendering", cause)
	}
	return providerError(document.RenditionErrorTransient, "PyMuPDF rendering timed out", cause)
}

func (provider *Provider) postProcessError(
	parent, operation context.Context, deadline time.Time, expiry bool,
) error {
	if parentErr := parent.Err(); parentErr != nil {
		return provider.contextError(parent, expiry, parentErr)
	}
	if operationErr := operation.Err(); operationErr != nil {
		return provider.contextError(parent, expiry, operationErr)
	}
	if !time.Now().UTC().Before(deadline) {
		return provider.contextError(parent, expiry, context.DeadlineExceeded)
	}
	return nil
}

func providerError(code document.RenditionErrorCode, message string, cause error) error {
	return providerutil.ClassifiedError("PyMuPDF", code, message, 0, cause)
}

func cleanEnvironment() []string {
	return []string{
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "PYTHONHASHSEED=0",
		"PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1",
	}
}

func validateRuntimeIdentity(value string) error {
	if value == "" || len(value) > 512 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return errors.New("pymupdf: runtime identity must be non-empty bounded UTF-8")
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return errors.New("pymupdf: runtime identity contains a control character")
		}
	}
	return nil
}

func validExplanation(value string) bool {
	if len(value) > 256 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func pythonInterpreter(base string) bool {
	name := strings.TrimSuffix(strings.ToLower(base), ".exe")
	if name == "py" {
		return true
	}
	for _, prefix := range []string{"python", "pypy"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if suffix == "" {
			return true
		}
		for _, char := range suffix {
			if (char < '0' || char > '9') && char != '.' {
				return false
			}
		}
		return true
	}
	return false
}

var _ document.RenditionProvider = (*Provider)(nil)
