package trafilatura

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"go.kenn.io/docbank/document"
)

const (
	providerID      = "trafilatura.local-v1"
	protocolVersion = "docbank-trafilatura/v2"
	profileVersion  = "docbank-trafilatura-profile/v2"
	timestampForm   = "2006-01-02T15:04:05.000000000Z"

	// MaxDocumentBytes is the largest supplied HTML document accepted by a profile.
	MaxDocumentBytes = int64(50 << 20)
	// MaxResponseBytes is the largest structured child response accepted by a profile.
	MaxResponseBytes = int64(64 << 20)
	// MaxUnits is the largest extracted unit sequence accepted by a profile.
	MaxUnits = 100_000
	// MaxTimeout is the largest local child deadline accepted by a profile.
	MaxTimeout = 30 * time.Minute
	// MaxExecutableBytes bounds executable identity verification.
	MaxExecutableBytes = int64(256 << 20)
)

var (
	errInputIdentity              = errors.New("authorized input identity changed")
	errNativeCanceledBeforeLaunch = errors.New("native isolated runner canceled before launch")
	// ErrIsolationUnavailable means the runner could not enforce the requested isolation policy.
	ErrIsolationUnavailable = errors.New("isolated runner policy unavailable")
	// ErrChildOutputTooLarge means the isolated child exceeded its stdout allowance.
	ErrChildOutputTooLarge = errors.New("isolated child output exceeds limit")
	// ErrChildFailed means the isolated child exited without a valid response.
	ErrChildFailed = errors.New("isolated child failed")
)

// IsolationRequirements are mandatory runner controls. A runner must fail
// closed rather than execute when any requested control is unavailable.
type IsolationRequirements struct {
	NetworkDisabled        bool
	KillProcessTree        bool
	VerifyExecutableSHA256 bool
}

// IsolatedRunRequest is the complete, fixed child execution authority.
type IsolatedRunRequest struct {
	Executable        string
	ExecutableSHA256  string
	Arguments         []string
	Environment       []string
	Directory         string
	Stdin             []byte
	StdinSHA256       string
	MaxStdoutBytes    int64
	PolicyFingerprint string
	Requirements      IsolationRequirements
}

// IsolationAttestation reports the exact controls applied to a completed run.
type IsolationAttestation struct {
	RunnerIdentity       string
	PolicyFingerprint    string
	ExecutableSHA256     string
	StdinSHA256          string
	NetworkDisabled      bool
	ProcessTreeContained bool
	DigestVerifiedLaunch bool
}

// IsolatedRunResult is bounded stdout plus its isolation attestation.
type IsolatedRunResult struct {
	Stdout      []byte
	Attestation IsolationAttestation
}

// IsolatedRunner is the trusted cross-platform process isolation boundary.
// Run must launch the digest-verified executable without a path re-open race,
// deny all network access, contain the process tree, and reap that tree on
// cancellation. It must return ErrIsolationUnavailable rather than weaken a
// requested control.
type IsolatedRunner interface {
	Identity() string
	Run(ctx context.Context, request IsolatedRunRequest) (IsolatedRunResult, error)
}

// Profile fixes one executable, immutable runtime identity, and all local bounds.
type Profile struct {
	Executable       string
	ExecutableSHA256 string
	RuntimeIdentity  string
	Runner           IsolatedRunner
	MaxDocumentBytes int64
	MaxResponseBytes int64
	MaxUnits         int
	Timeout          time.Duration
}

// Provider renders exact, caller-supplied HTML bytes through one local bridge.
type Provider struct {
	descriptor       document.RenditionDescriptor
	executable       string
	executableSHA256 string
	runtimeIdentity  string
	runnerIdentity   string
	runner           IsolatedRunner
	environment      []string
	maxDocumentBytes int64
	maxResponseBytes int64
	maxUnits         int
	timeout          time.Duration
}

// New constructs one immutable local-process provider profile.
func New(profile Profile) (*Provider, error) {
	if !filepath.IsAbs(profile.Executable) || filepath.Clean(profile.Executable) != profile.Executable {
		return nil, errors.New("trafilatura: executable must be an absolute clean path")
	}
	if pythonInterpreter(filepath.Base(profile.Executable)) {
		return nil, errors.New("trafilatura: executable must not be a Python interpreter")
	}
	info, err := os.Lstat(profile.Executable)
	if err != nil {
		return nil, errors.New("trafilatura: configured executable is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("trafilatura: executable must be a regular non-symlink file")
	}
	if info.Size() <= 0 || info.Size() > MaxExecutableBytes {
		return nil, errors.New("trafilatura: executable size is outside the supported bound")
	}
	if err := validateSHA256(profile.ExecutableSHA256, "executable SHA-256"); err != nil {
		return nil, err
	}
	executableDigest, err := hashExecutable(profile.Executable)
	if err != nil || executableDigest != profile.ExecutableSHA256 {
		return nil, errors.New("trafilatura: executable SHA-256 does not match configured content")
	}
	if err := validateImmutableIdentity(profile.RuntimeIdentity, "runtime identity"); err != nil {
		return nil, err
	}
	runner := profile.Runner
	if runner == nil {
		runner, err = newNativeRunner()
		if err != nil {
			return nil, fmt.Errorf("trafilatura: native isolated runner: %w", err)
		}
	}
	runnerIdentity := runner.Identity()
	if err := validateImmutableIdentity(runnerIdentity, "runner identity"); err != nil {
		return nil, err
	}
	if profile.MaxDocumentBytes <= 0 || profile.MaxDocumentBytes > MaxDocumentBytes {
		return nil, fmt.Errorf("trafilatura: max document bytes must be between 1 and %d", MaxDocumentBytes)
	}
	if profile.MaxResponseBytes <= 0 || profile.MaxResponseBytes > MaxResponseBytes {
		return nil, fmt.Errorf("trafilatura: max response bytes must be between 1 and %d", MaxResponseBytes)
	}
	if profile.MaxUnits <= 0 || profile.MaxUnits > MaxUnits {
		return nil, fmt.Errorf("trafilatura: max units must be between 1 and %d", MaxUnits)
	}
	if profile.Timeout <= 0 || profile.Timeout > MaxTimeout {
		return nil, fmt.Errorf("trafilatura: timeout must be between 1ns and %s", MaxTimeout)
	}
	environment := cleanEnvironment()
	identity := strings.Join([]string{
		profileVersion, protocolVersion, profile.Executable, profile.ExecutableSHA256,
		profile.RuntimeIdentity, runnerIdentity, strings.Join(environment, "\x1f"),
		strconv.FormatInt(profile.MaxDocumentBytes, 10), strconv.FormatInt(profile.MaxResponseBytes, 10),
		strconv.Itoa(profile.MaxUnits), strconv.FormatInt(int64(profile.Timeout), 10),
	}, "\x00")
	policyDigest := sha256.Sum256([]byte(identity))
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: providerID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: hex.EncodeToString(policyDigest[:]),
		TrustBoundary:     document.RenditionTrustLocalProcess,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "text", MediaType: "text/html", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "text", MediaType: "application/xhtml+xml", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsStructured: true,
	})
	if err != nil {
		return nil, fmt.Errorf("trafilatura: construct descriptor: %w", err)
	}
	return &Provider{
		descriptor: cloneDescriptor(descriptor), executable: profile.Executable,
		executableSHA256: profile.ExecutableSHA256, runtimeIdentity: profile.RuntimeIdentity,
		runnerIdentity: runnerIdentity, runner: runner, maxDocumentBytes: profile.MaxDocumentBytes,
		environment: slices.Clone(environment), maxResponseBytes: profile.MaxResponseBytes,
		maxUnits: profile.MaxUnits, timeout: profile.Timeout,
	}, nil
}

// Descriptor returns the immutable provider identity fixed by the profile.
func (provider *Provider) Descriptor() document.RenditionDescriptor {
	if provider == nil {
		return document.RenditionDescriptor{}
	}
	return cloneDescriptor(provider.descriptor)
}

// Render validates supplied HTML locally and sends its exact bytes over stdin only.
func (provider *Provider) Render(
	ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if provider == nil {
		return document.RenditionResult{}, errors.New("trafilatura: provider is required")
	}
	if _, err := document.ValidateRenditionProviderRequest(provider, upload, authorization); err != nil {
		return document.RenditionResult{}, err
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > provider.maxDocumentBytes {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"Trafilatura input exceeds the configured byte limit", nil)
	}
	if !validFilename(metadata.Filename, metadata.MediaType) {
		return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput,
			"Trafilatura requires a supplied HTML filename", nil)
	}
	expiresAt, err := time.Parse(timestampForm, authorization.ExpiresAt)
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"Trafilatura authorization is invalid", err)
	}
	startedAt := time.Now().UTC()
	deadline := startedAt.Add(provider.timeout)
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

	source, err := readAuthorizedExact(
		operationCtx, upload, metadata.ByteLength, provider.maxDocumentBytes,
	)
	if err != nil {
		if operationCtx.Err() != nil {
			return document.RenditionResult{}, provider.contextError(ctx, expiryDeadline, operationCtx.Err())
		}
		if errors.Is(err, errInputIdentity) {
			return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
				"Trafilatura input identity does not match authorization", err)
		}
		return document.RenditionResult{}, providerError(document.RenditionErrorTransient,
			"Trafilatura input could not be read", err)
	}
	defer clear(source)
	digest := sha256.Sum256(source)
	if int64(len(source)) != metadata.ByteLength || hex.EncodeToString(digest[:]) != metadata.SHA256 {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"Trafilatura input identity does not match authorization", nil)
	}
	authority, err := inspectHTML(source, metadata.MediaType, provider.maxUnits)
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput,
			"Trafilatura input is not locally verified HTML", err)
	}
	if err := provider.postProcessError(ctx, operationCtx, deadline, expiryDeadline); err != nil {
		return document.RenditionResult{}, err
	}

	if err := provider.reverifyExecutionBoundary(operationCtx); err != nil {
		if operationCtx.Err() != nil {
			return document.RenditionResult{}, provider.contextError(ctx, expiryDeadline, operationCtx.Err())
		}
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"Trafilatura isolated runtime identity changed", err)
	}
	runnerInput := slices.Clone(source)
	defer clear(runnerInput)
	stdoutLimit := min(provider.maxResponseBytes, int64(authorization.MaxTotalResultBytes))
	request := provider.isolatedRequest(runnerInput, stdoutLimit)
	runResult, runErr := provider.runner.Run(operationCtx, request)
	defer func() { clear(runResult.Stdout) }()
	if errors.Is(runErr, errNativeCanceledBeforeLaunch) && operationCtx.Err() != nil {
		return document.RenditionResult{}, provider.contextError(ctx, expiryDeadline, operationCtx.Err())
	}
	if errors.Is(runErr, ErrIsolationUnavailable) {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"Trafilatura isolation policy could not be enforced", runErr)
	}
	if err := provider.validateAttestation(request, runResult.Attestation); err != nil {
		clear(runResult.Stdout)
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"Trafilatura isolation attestation is invalid", err)
	}
	if operationCtx.Err() != nil {
		return document.RenditionResult{}, provider.contextError(ctx, expiryDeadline, operationCtx.Err())
	}
	if errors.Is(runErr, ErrChildOutputTooLarge) {
		return document.RenditionResult{}, providerError(document.RenditionErrorMalformedEvidence,
			"Trafilatura output exceeds the configured byte limit", nil)
	}
	if runErr != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorTransient,
			"Trafilatura executable failed", runErr)
	}
	if int64(len(runResult.Stdout)) > request.MaxStdoutBytes {
		return document.RenditionResult{}, providerError(document.RenditionErrorMalformedEvidence,
			"Trafilatura output exceeds the configured byte limit", nil)
	}
	raw := runResult.Stdout
	if err := provider.postProcessError(ctx, operationCtx, deadline, expiryDeadline); err != nil {
		return document.RenditionResult{}, err
	}

	wire, err := parseResponse(operationCtx, raw, provider.runtimeIdentity, metadata, authority, provider.maxUnits)
	if err != nil {
		if operationErr := provider.postProcessError(ctx, operationCtx, deadline, expiryDeadline); operationErr != nil {
			return document.RenditionResult{}, operationErr
		}
		return document.RenditionResult{}, providerError(document.RenditionErrorMalformedEvidence,
			"Trafilatura output is malformed", err)
	}
	evidence := evidenceFromResponse(wire)
	if err := document.ValidateSourceEvidenceV1(evidence); err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorMalformedEvidence,
			"Trafilatura output is malformed", err)
	}
	if err := provider.postProcessError(ctx, operationCtx, deadline, expiryDeadline); err != nil {
		return document.RenditionResult{}, err
	}
	completedAt := time.Now().UTC()
	warnings := []string(nil)
	if !*wire.ProvenanceComplete {
		warnings = []string{"degraded_provenance"}
	}
	return document.RenditionResult{
		Evidence: evidence,
		Receipt: document.RenditionReceipt{
			ProviderID: provider.descriptor.ID, DescriptorFingerprint: provider.descriptor.Fingerprint,
			PolicyFingerprint: authorization.PolicyFingerprint, SourceSHA256: metadata.SHA256,
			OperationID: "trafilatura-" + authorization.RenditionRequestFingerprint[:24],
			StartedAt:   startedAt.Format(timestampForm), CompletedAt: completedAt.Format(timestampForm),
			Warnings: warnings,
			Usage: document.RenditionUsage{Requests: 1, InputBytes: metadata.ByteLength,
				OutputBytes: int64(len(raw)), Units: int64(len(evidence.Units))},
		},
	}, nil
}

type response struct {
	ContractVersion    string         `json:"contract_version"`
	RuntimeIdentity    string         `json:"runtime_identity"`
	SourceSHA256       string         `json:"source_sha256"`
	SourceBytes        int64          `json:"source_bytes"`
	ExtractionComplete bool           `json:"extraction_complete"`
	ProvenanceComplete *bool          `json:"provenance_complete"`
	Units              []responseUnit `json:"units"`
}

type responseUnit struct {
	SourcePath string `json:"source_path,omitempty"`
	Heading    string `json:"heading,omitempty"`
	Text       string `json:"text"`
}

type htmlAuthority struct {
	visibleTokens []string
	sections      []htmlSectionAuthority
}

type htmlSectionAuthority struct {
	path    string
	heading string
	tokens  []string
}

func (provider *Provider) isolatedRequest(stdin []byte, maxStdoutBytes int64) IsolatedRunRequest {
	arguments := []string{"--protocol", protocolVersion}
	environment := slices.Clone(provider.environment)
	requirements := IsolationRequirements{
		NetworkDisabled: true, KillProcessTree: true, VerifyExecutableSHA256: true,
	}
	stdinDigest := sha256.Sum256(stdin)
	stdinSHA256 := hex.EncodeToString(stdinDigest[:])
	request := IsolatedRunRequest{
		Executable: provider.executable, ExecutableSHA256: provider.executableSHA256,
		Arguments: arguments, Environment: environment, Directory: filepath.Dir(provider.executable),
		Stdin: stdin, StdinSHA256: stdinSHA256, MaxStdoutBytes: maxStdoutBytes,
		Requirements: requirements,
	}
	request.PolicyFingerprint = isolationRequestPolicyFingerprint(provider.runnerIdentity, request)
	return request
}

func isolationRequestPolicyFingerprint(runnerIdentity string, request IsolatedRunRequest) string {
	identity := strings.Join([]string{
		"docbank-isolated-run/v1", runnerIdentity, request.Executable,
		request.ExecutableSHA256, strings.Join(request.Arguments, "\x1f"), strings.Join(request.Environment, "\x1f"),
		request.Directory, request.StdinSHA256, strconv.FormatInt(request.MaxStdoutBytes, 10),
		"network-disabled", "kill-process-tree", "digest-verified-launch",
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func (provider *Provider) reverifyExecutionBoundary(ctx context.Context) error {
	if provider.runner == nil || provider.runner.Identity() != provider.runnerIdentity {
		return errors.New("isolated runner identity changed")
	}
	digest, err := hashExecutableContext(ctx, provider.executable)
	if err != nil || digest != provider.executableSHA256 {
		return errors.New("executable content changed")
	}
	return nil
}

func (provider *Provider) validateAttestation(
	request IsolatedRunRequest, attestation IsolationAttestation,
) error {
	stdinDigest := sha256.Sum256(request.Stdin)
	if attestation.RunnerIdentity != provider.runnerIdentity ||
		attestation.PolicyFingerprint != request.PolicyFingerprint ||
		attestation.ExecutableSHA256 != provider.executableSHA256 ||
		request.StdinSHA256 != hex.EncodeToString(stdinDigest[:]) || attestation.StdinSHA256 != request.StdinSHA256 ||
		!attestation.NetworkDisabled || !attestation.ProcessTreeContained || !attestation.DigestVerifiedLaunch {
		return errors.New("isolated runner did not attest the exact required policy")
	}
	return nil
}

func parseResponse(
	ctx context.Context, raw []byte, runtimeIdentity string,
	metadata document.AuthorizedUploadMetadata, authority htmlAuthority, maxUnits int,
) (response, error) {
	if len(raw) == 0 {
		return response{}, errors.New("empty response")
	}
	var wire response
	if err := json.Unmarshal(raw, &wire, json.RejectUnknownMembers(true)); err != nil {
		return response{}, err
	}
	if wire.ContractVersion != protocolVersion || wire.RuntimeIdentity != runtimeIdentity {
		return response{}, errors.New("local protocol identity changed")
	}
	if wire.SourceSHA256 != metadata.SHA256 || wire.SourceBytes != metadata.ByteLength {
		return response{}, errors.New("source identity changed")
	}
	if !wire.ExtractionComplete {
		return response{}, errors.New("response does not attest complete extraction")
	}
	if wire.ProvenanceComplete == nil {
		return response{}, errors.New("response does not attest provenance completeness")
	}
	if len(wire.Units) == 0 || len(wire.Units) > maxUnits {
		return response{}, errors.New("unit count is invalid")
	}
	outputTokens := make([]string, 0)
	for index := range wire.Units {
		if err := ctx.Err(); err != nil {
			return response{}, err
		}
		unit := &wire.Units[index]
		if !validOutputText(unit.Text) {
			return response{}, errors.New("unit text is invalid")
		}
		unit.Text = strings.TrimSpace(unit.Text)
		unitTokens := strings.Fields(unit.Text)
		outputTokens = append(outputTokens, unitTokens...)
	}
	if !slices.Equal(outputTokens, authority.visibleTokens) {
		return response{}, errors.New("output does not cover exact supplied HTML text")
	}
	if !*wire.ProvenanceComplete {
		for _, unit := range wire.Units {
			if unit.SourcePath != "" || unit.Heading != "" {
				return response{}, errors.New("degraded output claims unverified natural provenance")
			}
		}
		return wire, nil
	}
	if len(authority.sections) == 0 || len(wire.Units) != len(authority.sections) {
		return response{}, errors.New("complete section structure is not locally verified")
	}
	for index, unit := range wire.Units {
		section := authority.sections[index]
		if unit.SourcePath != section.path || strings.Join(strings.Fields(unit.Heading), " ") != section.heading ||
			!slices.Equal(strings.Fields(unit.Text), section.tokens) {
			return response{}, errors.New("section output does not match locally verified HTML structure")
		}
	}
	return wire, nil
}

func evidenceFromResponse(wire response) document.SourceEvidenceV1 {
	if !*wire.ProvenanceComplete {
		parts := make([]string, len(wire.Units))
		for index, unit := range wire.Units {
			parts[index] = unit.Text
		}
		return document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1,
			Completeness:    document.EvidenceDegradedProvenance,
			Family:          "text", UnitKind: document.EvidenceUnitGeneric,
			Omissions: []document.SourceEvidenceOmissionV1{{
				Kind: document.EvidenceOmissionField, Field: "natural_structure",
				Reason: "All supplied text is extracted but natural HTML section boundaries are not proven",
			}},
			Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: strings.Join(parts, "\n\n"),
				Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorGeneric,
					IndexOrigin: document.EvidenceIndexOriginNone}}},
		}
	}
	units := make([]document.SourceEvidenceUnitV1, len(wire.Units))
	for index, unit := range wire.Units {
		units[index] = document.SourceEvidenceUnitV1{
			Order: index, ProviderID: fmt.Sprintf("trafilatura-section-%d", index+1), Text: unit.Text,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorSection,
				IndexOrigin: document.EvidenceIndexOriginNone, Name: unit.Heading},
		}
	}
	return document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "text", UnitKind: document.EvidenceUnitSection, Units: units,
	}
}

func inspectHTML(source []byte, mediaType string, maxSections int) (htmlAuthority, error) {
	if len(source) == 0 || !utf8.Valid(source) || bytes.IndexByte(source, 0) >= 0 {
		return htmlAuthority{}, errors.New("HTML must be nonempty safe UTF-8")
	}
	if mediaType == "application/xhtml+xml" {
		if err := validateXHTML(source); err != nil {
			return htmlAuthority{}, err
		}
	} else if mediaType != "text/html" || !hasHTMLStructure(source) {
		return htmlAuthority{}, errors.New("HTML structure is missing")
	}
	node, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return htmlAuthority{}, fmt.Errorf("parse supplied HTML: %w", err)
	}
	if err := validateVisibleText(node); err != nil {
		return htmlAuthority{}, err
	}
	visible := visibleTokens(node)
	if len(visible) == 0 {
		return htmlAuthority{}, errors.New("HTML has no visible supplied text")
	}
	sections := naturalSections(node, maxSections)
	sectionTokens := make([]string, 0, len(visible))
	for _, section := range sections {
		sectionTokens = append(sectionTokens, section.tokens...)
	}
	if !slices.Equal(sectionTokens, visible) {
		sections = nil
	}
	return htmlAuthority{visibleTokens: visible, sections: sections}, nil
}

func validateVisibleText(node *html.Node) error {
	var walk func(*html.Node, bool) error
	walk = func(current *html.Node, hidden bool) error {
		if current.Type == html.ElementNode {
			hidden = hidden || hiddenElement(strings.ToLower(current.Data))
		}
		if current.Type == html.TextNode && !hidden {
			for _, char := range current.Data {
				if unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t' {
					return errors.New("HTML visible text contains an unsupported control character")
				}
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			if err := walk(child, hidden); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(node, false)
}

func visibleTokens(node *html.Node) []string {
	var tokens []string
	var walk func(*html.Node, bool)
	walk = func(current *html.Node, hidden bool) {
		if current.Type == html.ElementNode {
			hidden = hidden || hiddenElement(strings.ToLower(current.Data))
		}
		if current.Type == html.TextNode && !hidden {
			tokens = append(tokens, strings.Fields(current.Data)...)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child, hidden)
		}
	}
	walk(node, false)
	return tokens
}

func hiddenElement(tag string) bool {
	return tag == "head" || tag == "script" || tag == "style" ||
		tag == "noscript" || tag == "template" || tag == "svg"
}

func naturalSections(root *html.Node, maxSections int) []htmlSectionAuthority {
	var sections []htmlSectionAuthority
	var path []string
	var walk func(*html.Node) bool
	walk = func(current *html.Node) bool {
		isSection := current.Type == html.ElementNode && strings.EqualFold(current.Data, "section")
		if isSection {
			if len(sections) >= maxSections {
				return false
			}
			sections = append(sections, htmlSectionAuthority{
				path: "/" + strings.Join(path, "/"), heading: firstSectionHeading(current), tokens: visibleTokens(current),
			})
			return true
		}
		var ordinals map[string]int
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode {
				if ordinals == nil {
					ordinals = make(map[string]int)
				}
				tag := strings.ToLower(child.Data)
				ordinals[tag]++
				path = append(path, tag+"["+strconv.Itoa(ordinals[tag])+"]")
			}
			if !walk(child) {
				return false
			}
			if child.Type == html.ElementNode {
				path = path[:len(path)-1]
			}
		}
		return true
	}
	if !walk(root) {
		return nil
	}
	return sections
}

func firstSectionHeading(section *html.Node) string {
	var find func(*html.Node) *html.Node
	find = func(current *html.Node) *html.Node {
		if current.Type == html.ElementNode {
			tag := strings.ToLower(current.Data)
			if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
				return current
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			if found := find(child); found != nil {
				return found
			}
		}
		return nil
	}
	heading := find(section)
	if heading == nil {
		return ""
	}
	return strings.Join(visibleTokens(heading), " ")
}

func hasHTMLStructure(source []byte) bool {
	tokenizer := html.NewTokenizer(bytes.NewReader(source))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return false
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			if strings.EqualFold(string(name), "html") || strings.EqualFold(string(name), "body") {
				return true
			}
		default:
		}
	}
}

func validateXHTML(source []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(source))
	depth := 0
	roots := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if roots == 1 && depth == 0 {
				return nil
			}
			return errors.New("XHTML root is invalid")
		}
		if err != nil {
			return errors.New("XHTML is malformed")
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || value.Name.Local != "html" ||
					(value.Name.Space != "" && value.Name.Space != "http://www.w3.org/1999/xhtml") {
					return errors.New("XHTML root is invalid")
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 {
				return errors.New("XHTML is malformed")
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(value)) != 0 {
				return errors.New("XHTML has text outside its root")
			}
		}
	}
}

func readExact(ctx context.Context, reader io.Reader, expected, maximum int64) ([]byte, error) {
	data := make([]byte, 0, expected)
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, err := reader.Read(buffer)
		if read > 0 {
			if int64(len(data))+int64(read) > maximum {
				return nil, errInputIdentity
			}
			data = append(data, buffer[:read]...)
		}
		switch {
		case errors.Is(err, io.EOF):
			if int64(len(data)) != expected {
				return nil, errInputIdentity
			}
			return data, nil
		case err != nil:
			return nil, err
		case read == 0:
			return nil, io.ErrNoProgress
		}
	}
}

func readAuthorizedExact(
	ctx context.Context, upload document.AuthorizedUpload, expected, maximum int64,
) ([]byte, error) {
	closeDone := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() {
		_ = upload.Close()
		close(closeDone)
	})
	data, err := readExact(ctx, upload, expected, maximum)
	if !stopClose() {
		<-closeDone
	}
	return data, err
}

func (provider *Provider) contextError(parent context.Context, expiry bool, cause error) error {
	if parent.Err() != nil {
		return providerError(document.RenditionErrorCanceled, "Trafilatura rendering canceled", parent.Err())
	}
	if expiry {
		return providerError(document.RenditionErrorPolicyRejected,
			"Trafilatura authorization expired during rendering", cause)
	}
	return providerError(document.RenditionErrorTransient, "Trafilatura rendering timed out", cause)
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
	classified, err := document.NewRenditionProviderError(code, message, 0, cause)
	if err != nil {
		return fmt.Errorf("trafilatura: classify provider error: %w", err)
	}
	return classified
}

func cleanEnvironment() []string {
	environment := []string{
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "PYTHONHASHSEED=0",
		"PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1",
	}
	if runtime.GOOS == "windows" {
		if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
			environment = append(environment, "SystemRoot="+systemRoot)
		}
	}
	return environment
}

func validFilename(filename, mediaType string) bool {
	if filename == "" || filename != strings.TrimSpace(filename) || strings.ContainsAny(filename, "/\\:\x00") {
		return false
	}
	extension := strings.ToLower(filepath.Ext(filename))
	switch mediaType {
	case "text/html":
		return extension == ".html" || extension == ".htm"
	case "application/xhtml+xml":
		return extension == ".xhtml"
	default:
		return false
	}
}

func validateImmutableIdentity(value, subject string) error {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return fmt.Errorf("trafilatura: %s must be an immutable sha256 identity", subject)
	}
	return validateSHA256(strings.TrimPrefix(value, "sha256:"), subject)
}

func validateSHA256(value, subject string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("trafilatura: %s must be a lowercase SHA-256 digest", subject)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return fmt.Errorf("trafilatura: %s must be a lowercase SHA-256 digest", subject)
	}
	return nil
}

func hashExecutable(path string) (string, error) {
	return hashExecutableContext(context.Background(), path)
}

func hashExecutableContext(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(contextReader{ctx: ctx, reader: file}, MaxExecutableBytes+1))
	if err != nil || written <= 0 || written > MaxExecutableBytes {
		return "", errors.New("executable content could not be bounded and hashed")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(value []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(value)
}

func validOutputText(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\n' && char != '\r' && char != '\t' {
			return false
		}
	}
	return true
}

func pythonInterpreter(base string) bool {
	name := strings.TrimSuffix(strings.ToLower(base), ".exe")
	if name == "py" || name == "pyw" {
		return true
	}
	for _, prefix := range []string{"python", "pypy"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if prefix == "python" {
			suffix = strings.TrimPrefix(suffix, "w")
		}
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

func cloneDescriptor(value document.RenditionDescriptor) document.RenditionDescriptor {
	value.SupportedFormats = slices.Clone(value.SupportedFormats)
	value.ArtifactRoles = slices.Clone(value.ArtifactRoles)
	return value
}

var _ document.RenditionProvider = (*Provider)(nil)
