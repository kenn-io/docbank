// Package marker implements the fixed uploaded-file self-hosted Marker flow.
package marker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/providerhttp"
)

const (
	providerID              = "marker.self-hosted-v1"
	uploadPath              = "/marker/upload"
	adapterContract         = "marker-self-hosted-adapter/v1"
	provider                = providerutil.Provider("Marker")
	defaultRequestTimeout   = 10 * time.Minute
	defaultMaxDocumentBytes = int64(200 << 20)
	defaultMaxRequestBytes  = int64(201 << 20)
	defaultMaxResponseBytes = int64(64 << 20)
	defaultMaxMetadataBytes = int64(4 << 20)
	defaultMaxImages        = 64
	defaultMaxImageBytes    = int64(16 << 20)
	defaultMaxUnits         = 10_000
	maxTimeout              = 24 * time.Hour
	maxDocumentBytes        = int64(1 << 30)
	maxRequestBytes         = int64(1<<30) + (2 << 20)
	maxResponseBytes        = int64(512 << 20)
	maxMetadataBytes        = int64(64 << 20)
	maxImages               = 64
	maxImageBytes           = int64(256 << 20)
	maxUnits                = 1_000_000
	pageSeparator           = "------------------------------------------------"
)

var _ document.RenditionProvider = (*Client)(nil)

// SecretResolver resolves an optional operator-fronted Marker credential.
type SecretResolver = providerutil.SecretResolver

// Profile pins the operator deployment, runtime, credential name, conversion
// mode, fixed wire contract, and every request/result bound.
type Profile struct {
	Origin                string
	Descriptor            document.RenditionDescriptor
	DeploymentFingerprint string
	RuntimeFingerprint    string
	SecretBinding         string
	Mode                  string
	RequestTimeout        time.Duration
	MaxDocumentBytes      int64
	MaxRequestBytes       int64
	MaxResponseBytes      int64
	MaxMetadataBytes      int64
	MaxImages             int
	MaxImageBytes         int64
	MaxUnits              int
}

type policyIdentity struct {
	AdapterContract       string                               `json:"adapter_contract"`
	Origin                string                               `json:"origin"`
	Route                 string                               `json:"route"`
	DeploymentFingerprint string                               `json:"deployment_fingerprint"`
	RuntimeFingerprint    string                               `json:"runtime_fingerprint"`
	CredentialBinding     string                               `json:"credential_binding"`
	Mode                  string                               `json:"mode"`
	OutputFormat          string                               `json:"output_format"`
	ForceOCR              bool                                 `json:"force_ocr"`
	PaginateOutput        bool                                 `json:"paginate_output"`
	RequestTimeoutNanos   int64                                `json:"request_timeout_nanos"`
	MaxDocumentBytes      int64                                `json:"max_document_bytes"`
	MaxRequestBytes       int64                                `json:"max_request_bytes"`
	MaxResponseBytes      int64                                `json:"max_response_bytes"`
	MaxMetadataBytes      int64                                `json:"max_metadata_bytes"`
	MaxImages             int                                  `json:"max_images"`
	MaxImageBytes         int64                                `json:"max_image_bytes"`
	MaxUnits              int                                  `json:"max_units"`
	SupportedFormats      []document.RenditionFormatCapability `json:"supported_formats"`
}

// SupportedFormats returns the exact Marker families for which Docbank has
// bounded original-file upload authority.
func SupportedFormats() []document.RenditionFormatCapability {
	return []document.RenditionFormatCapability{
		{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/webp", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/gif", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "word", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "ebook", MediaType: "application/epub+zip", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/html", InputKind: document.RenditionInputOriginalFile},
	}
}

// PolicyFingerprint returns the canonical profile identity expected in the
// rendition descriptor. Credential values and transports are never included.
func PolicyFingerprint(profile Profile) (string, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return "", err
	}
	identity := policyIdentity{AdapterContract: adapterContract, Origin: normalized.Origin,
		Route: uploadPath, DeploymentFingerprint: normalized.DeploymentFingerprint,
		RuntimeFingerprint: normalized.RuntimeFingerprint, CredentialBinding: normalized.SecretBinding,
		Mode: normalized.Mode, OutputFormat: "markdown", ForceOCR: false, PaginateOutput: true,
		RequestTimeoutNanos: int64(normalized.RequestTimeout), MaxDocumentBytes: normalized.MaxDocumentBytes,
		MaxRequestBytes: normalized.MaxRequestBytes, MaxResponseBytes: normalized.MaxResponseBytes,
		MaxMetadataBytes: normalized.MaxMetadataBytes, MaxImages: normalized.MaxImages,
		MaxImageBytes: normalized.MaxImageBytes, MaxUnits: normalized.MaxUnits,
		SupportedFormats: SupportedFormats()}
	encoded, err := json.Marshal(identity, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("marker: encode policy identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Client renders exact authorized uploads through one fixed Marker route.
type Client struct {
	profile    Profile
	descriptor document.RenditionDescriptor
	executor   providerutil.Executor
}

// New validates a pinned self-hosted profile. The injected transport is the
// operator's hardened destination policy; Client removes ambient cookies and
// always refuses redirects.
func New(profile Profile, secrets SecretResolver, transport http.RoundTripper) (*Client, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := provider.CanonicalDescriptor(profile.Descriptor)
	if err != nil {
		return nil, err
	}
	if descriptor.ID != providerID || descriptor.TrustBoundary != document.RenditionTrustOperatorNetwork ||
		!descriptor.ReturnsMarkdown || !descriptor.ReturnsStructured || len(descriptor.ArtifactRoles) != 0 {
		return nil, errors.New("marker: descriptor result or trust contract is invalid")
	}
	wantFormats := SupportedFormats()
	slices.SortFunc(wantFormats, compareFormats)
	if !slices.Equal(descriptor.SupportedFormats, wantFormats) {
		return nil, errors.New("marker: descriptor must advertise the exact supported format set")
	}
	fingerprint, err := PolicyFingerprint(normalized)
	if err != nil {
		return nil, err
	}
	if descriptor.PolicyFingerprint != fingerprint {
		return nil, errors.New("marker: descriptor policy fingerprint does not match profile")
	}
	credential := providerutil.BearerCredential(normalized.SecretBinding, secrets)
	if err := credential.Validate(provider); err != nil {
		return nil, err
	}
	if transport == nil {
		return nil, errors.New("marker: hardened transport is required")
	}
	normalized.Descriptor = descriptor
	return &Client{profile: normalized, descriptor: descriptor, executor: providerutil.Executor{
		Provider: provider, HTTP: providerhttp.IsolateClient(&http.Client{Transport: transport}),
		Origin: normalized.Origin, RequestTimeout: normalized.RequestTimeout,
		MaxResponseBytes: normalized.MaxResponseBytes, Credential: credential,
	}}, nil
}

func (client *Client) Descriptor() document.RenditionDescriptor {
	if client == nil {
		return document.RenditionDescriptor{}
	}
	return providerutil.CloneDescriptor(client.descriptor)
}

func (client *Client) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	if client == nil {
		return document.RenditionResult{}, errors.New("marker: client is required")
	}
	metadata := upload.Metadata()
	if metadata.ByteLength > client.profile.MaxDocumentBytes {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "Marker input exceeds the document byte limit", nil)
	}
	if metadata.MediaFamily == "image" && metadata.ByteLength > media.MaxBytes {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "Marker image exceeds the verification byte limit", nil)
	}
	if strings.ContainsAny(metadata.Filename, "\r\n") {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "Marker upload filename contains a newline", nil)
	}
	filename, ok := uploadFilename(metadata)
	if !ok {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorUnsupportedInput, "Marker input filename does not match the authorized format", nil)
	}
	metadata.Filename = filename
	operation, err := providerutil.NewOperation(ctx, provider, authorization.ExpiresAt, client.profile.RequestTimeout)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer operation.Cancel()
	source, err := operation.ReadUpload(upload)
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer clear(source)
	expectedNaturalUnits, err := client.localUnits(metadata, source)
	if err != nil {
		return document.RenditionResult{}, err
	}
	body := &providerutil.MultipartUpload{
		FieldName: "file", Filename: metadata.Filename, MediaType: metadata.MediaType,
		Source: bytes.NewReader(source), Length: int64(len(source)),
		Fields: [][2]string{{"mode", client.profile.Mode}, {"force_ocr", "false"}, {"paginate_output", "true"}, {"output_format", "markdown"}},
	}
	encodedLength, err := body.EncodedLength()
	if err != nil {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorTransient, "could not prepare Marker upload", err)
	}
	if encodedLength > client.profile.MaxRequestBytes {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "Marker multipart request exceeds byte limit", nil)
	}
	started := time.Now().UTC()
	var usage providerutil.Usage
	response, err := client.executor.Do(operation, &usage, providerutil.Request{
		Stage: providerutil.StageResult, Method: http.MethodPost, Path: uploadPath, Upload: body,
		MaxResponseBytes: int64(authorization.MaxTotalResultBytes),
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	if !response.Success() {
		return document.RenditionResult{}, provider.StatusError(providerutil.StageResult, response.Status, response.RetryAfter, nil)
	}
	result, warnings, err := client.parseResult(response.Body, metadata.MediaFamily, expectedNaturalUnits)
	if err != nil {
		return document.RenditionResult{}, err
	}
	providerMarkdown := result.markdown
	if authorization.MaxProviderMarkdownBytes == 0 {
		providerMarkdown = nil
	} else if len(providerMarkdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, provider.Malformed("Marker Markdown exceeds authorization", nil)
	}
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{
		Descriptor: client.descriptor, Authorization: authorization, SourceSHA256: metadata.SHA256,
		OperationID: "marker-" + authorization.RenditionRequestFingerprint,
		StartedAt:   started, CompletedAt: time.Now().UTC(), Warnings: warnings,
		Usage: usage.Rendition(int64(len(source)), int64(len(result.evidence.Units))),
	})
	if err != nil {
		return document.RenditionResult{}, err
	}
	return document.RenditionResult{Evidence: result.evidence, ProviderMarkdown: providerMarkdown, Receipt: receipt}, nil
}

// localUnits proves the natural unit count of PDF and still-image sources
// before egress so provider output can be checked against it.
func (client *Client) localUnits(metadata document.AuthorizedUploadMetadata, source []byte) (int, error) {
	switch metadata.MediaFamily {
	case "pdf":
		pages, err := formatdetect.CountPDFPages(source)
		if err != nil || pages <= 0 {
			return 0, provider.Classified(document.RenditionErrorUnsupportedInput, "Marker PDF page authority is invalid", err)
		}
		if pages > int64(client.profile.MaxUnits) {
			return 0, provider.Classified(document.RenditionErrorPolicyRejected, "Marker PDF exceeds the unit limit", nil)
		}
		return int(pages), nil
	case "image":
		detected, err := media.DetectBytes(source, metadata.MediaType)
		if err != nil {
			return 0, provider.Classified(document.RenditionErrorUnsupportedInput, "Marker image authority is invalid", err)
		}
		if detected.Kind != media.KindImage || detected.MediaType != metadata.MediaType {
			return 0, provider.Classified(document.RenditionErrorUnsupportedInput, "Marker image identity does not match the authorization", nil)
		}
		if detected.FrameCount != 1 || detected.Animated {
			return 0, provider.Classified(document.RenditionErrorUnsupportedInput, "Marker requires a single-frame image", nil)
		}
		return 1, nil
	default:
		return 0, nil
	}
}

type parsedResult struct {
	markdown []byte
	evidence document.SourceEvidenceV1
}

func (client *Client) parseResult(body []byte, family string, expectedNaturalUnits int) (parsedResult, []string, error) {
	var wire struct {
		Format   *string           `json:"format"`
		Output   *string           `json:"output"`
		Images   map[string]string `json:"images"`
		Metadata jsontext.Value    `json:"metadata"`
		Success  *bool             `json:"success"`
		Error    string            `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &wire, json.RejectUnknownMembers(true)); err != nil {
		return parsedResult{}, nil, provider.Malformed("Marker result JSON is invalid", err)
	}
	if wire.Success == nil || !*wire.Success || wire.Format == nil || *wire.Format != "markdown" ||
		wire.Output == nil || *wire.Output == "" || wire.Images == nil || len(wire.Metadata) == 0 || wire.Error != "" {
		return parsedResult{}, nil, provider.Malformed("Marker result is incomplete or unsuccessful", nil)
	}
	if len(wire.Metadata) > int(client.profile.MaxMetadataBytes) {
		return parsedResult{}, nil, provider.Malformed("Marker metadata exceeds byte limit", nil)
	}
	if err := validateImages(wire.Images, client.profile.MaxImages, client.profile.MaxImageBytes); err != nil {
		return parsedResult{}, nil, err
	}
	stats, err := parseMetadata(wire.Metadata, client.profile.MaxUnits)
	if err != nil {
		return parsedResult{}, nil, err
	}
	if expectedNaturalUnits > 0 && !statsProveUnits(stats, expectedNaturalUnits) {
		return parsedResult{}, nil, provider.Malformed("Marker result does not prove complete source units", nil)
	}
	markdown := []byte(*wire.Output)
	if providerutil.InjectsDocbankFrontmatter(markdown) {
		return parsedResult{}, nil, provider.Malformed("Marker Markdown contains reserved Docbank frontmatter", nil)
	}
	evidence, natural := naturalEvidence(family, *wire.Output, stats)
	if !natural {
		degradedMarkdown := stripPaginationMarkers(*wire.Output)
		if degradedMarkdown == "" {
			return parsedResult{}, nil, provider.Malformed("Marker result contains no usable evidence", nil)
		}
		evidence = providerutil.DegradedEvidence(family, degradedMarkdown,
			"Marker returned no source-native unit mapping")
	}
	warnings := []string(nil)
	if len(wire.Images) != 0 {
		warnings = []string{"provider_images_not_retained"}
	}
	return parsedResult{markdown: markdown, evidence: evidence}, warnings, nil
}

type pageStat struct{ PageID int }

func statsProveUnits(stats []pageStat, expected int) bool {
	if len(stats) != expected {
		return false
	}
	for index, stat := range stats {
		if stat.PageID != index {
			return false
		}
	}
	return true
}

func parseMetadata(raw jsontext.Value, maximum int) ([]pageStat, error) {
	var metadata struct {
		TableOfContents jsontext.Value `json:"table_of_contents"`
		PageStats       []struct {
			PageID               *int           `json:"page_id"`
			TextExtractionMethod string         `json:"text_extraction_method"`
			BlockCounts          jsontext.Value `json:"block_counts"`
			BlockMetadata        jsontext.Value `json:"block_metadata"`
		} `json:"page_stats"`
	}
	if err := json.Unmarshal(raw, &metadata, json.RejectUnknownMembers(true)); err != nil {
		return nil, provider.Malformed("Marker metadata schema changed", err)
	}
	if len(metadata.TableOfContents) == 0 || metadata.PageStats == nil || len(metadata.PageStats) > maximum {
		return nil, provider.Malformed("Marker metadata is incomplete or exceeds the unit limit", nil)
	}
	stats := make([]pageStat, len(metadata.PageStats))
	for index, stat := range metadata.PageStats {
		if stat.PageID == nil || *stat.PageID < 0 || len(stat.TextExtractionMethod) > 128 ||
			len(stat.BlockCounts) == 0 || len(stat.BlockMetadata) == 0 {
			return nil, provider.Malformed("Marker page metadata is incomplete", nil)
		}
		stats[index] = pageStat{PageID: *stat.PageID}
	}
	return stats, nil
}

func naturalEvidence(family, markdown string, stats []pageStat) (document.SourceEvidenceV1, bool) {
	if family != "pdf" && family != "image" {
		return document.SourceEvidenceV1{}, false
	}
	parts, ok := splitPages(markdown, stats)
	if !ok {
		return document.SourceEvidenceV1{}, false
	}
	evidence := document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1,
		Completeness: document.EvidenceComplete, Family: family, UnitKind: document.EvidenceUnitPage,
		Units: make([]document.SourceEvidenceUnitV1, len(parts))}
	for index, text := range parts {
		evidence.Units[index] = document.SourceEvidenceUnitV1{Order: index,
			ProviderID: "marker-page-" + strconv.Itoa(index), Text: text,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginZero, Start: int64(index), End: int64(index)}}
	}
	return evidence, true
}

func splitPages(markdown string, stats []pageStat) ([]string, bool) {
	if len(stats) == 0 {
		return nil, false
	}
	lines := strings.Split(markdown, "\n")
	parts := make([]string, 0, len(stats))
	current := make([]string, 0)
	seen := false
	for _, line := range lines {
		index, separator := pageLine(line)
		if separator {
			if seen {
				parts = append(parts, strings.TrimSpace(strings.Join(current, "\n")))
				current = current[:0]
			}
			if index != len(parts) || index >= len(stats) || stats[index].PageID != index {
				return nil, false
			}
			seen = true
			continue
		}
		if !seen && strings.TrimSpace(line) != "" {
			return nil, false
		}
		if seen {
			current = append(current, line)
		}
	}
	if seen {
		parts = append(parts, strings.TrimSpace(strings.Join(current, "\n")))
	}
	return parts, len(parts) == len(stats)
}

func pageLine(line string) (int, bool) {
	open := strings.IndexByte(line, '{')
	closeIndex := strings.IndexByte(line, '}')
	if open != 0 || closeIndex <= 1 || line[closeIndex+1:] != pageSeparator {
		return 0, false
	}
	value, err := strconv.Atoi(line[1:closeIndex])
	return value, err == nil && value >= 0
}

func stripPaginationMarkers(markdown string) string {
	lines := strings.Split(markdown, "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		if _, separator := pageLine(line); !separator {
			clean = append(clean, line)
		}
	}
	return strings.TrimSpace(strings.Join(clean, "\n"))
}

func validateImages(images map[string]string, maximum int, maxBytes int64) error {
	if len(images) > maximum {
		return provider.Malformed("Marker returned too many images", nil)
	}
	for name, encoded := range images {
		if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) ||
			int64(base64.StdEncoding.DecodedLen(len(encoded))) > maxBytes {
			return provider.Malformed("Marker image output is invalid or exceeds limits", nil)
		}
		decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(encoded))
		count, err := io.Copy(io.Discard, io.LimitReader(decoder, maxBytes+1))
		if err != nil || count > maxBytes {
			return provider.Malformed("Marker image output is invalid or exceeds limits", err)
		}
	}
	return nil
}

func normalizeProfile(profile Profile) (Profile, error) {
	origin, err := provider.ValidateOrigin(profile.Origin, document.RenditionTrustOperatorNetwork)
	if err != nil {
		return Profile{}, err
	}
	profile.Origin = origin
	if !validFingerprint(profile.DeploymentFingerprint) || !validFingerprint(profile.RuntimeFingerprint) {
		return Profile{}, errors.New("marker: deployment and runtime fingerprints must be lowercase SHA-256")
	}
	if profile.SecretBinding != "" {
		if err := provider.ValidateIdentifier(profile.SecretBinding, "secret binding"); err != nil {
			return Profile{}, err
		}
	}
	if profile.Mode != "fast" && profile.Mode != "balanced" {
		return Profile{}, errors.New("marker: mode must be fast or balanced")
	}
	if !providerutil.Bounded(&profile.RequestTimeout, defaultRequestTimeout, maxTimeout) ||
		!providerutil.Bounded(&profile.MaxDocumentBytes, defaultMaxDocumentBytes, maxDocumentBytes) ||
		!providerutil.Bounded(&profile.MaxRequestBytes, defaultMaxRequestBytes, maxRequestBytes) ||
		profile.MaxRequestBytes <= profile.MaxDocumentBytes ||
		!providerutil.Bounded(&profile.MaxResponseBytes, defaultMaxResponseBytes, maxResponseBytes) ||
		!providerutil.Bounded(&profile.MaxMetadataBytes, defaultMaxMetadataBytes, maxMetadataBytes) ||
		profile.MaxMetadataBytes > profile.MaxResponseBytes ||
		!providerutil.Bounded(&profile.MaxImages, defaultMaxImages, maxImages) ||
		!providerutil.Bounded(&profile.MaxImageBytes, defaultMaxImageBytes, maxImageBytes) ||
		profile.MaxImageBytes > profile.MaxResponseBytes ||
		!providerutil.Bounded(&profile.MaxUnits, defaultMaxUnits, maxUnits) {
		return Profile{}, errors.New("marker: execution bounds are invalid")
	}
	return profile, nil
}

func uploadFilename(metadata document.AuthorizedUploadMetadata) (string, bool) {
	ext := strings.ToLower(filepath.Ext(metadata.Filename))
	switch metadata.MediaType {
	case "application/pdf":
		return filenameWithExtension(metadata.Filename, ext, ".pdf")
	case "image/jpeg":
		if metadata.Filename == "" {
			return "document.jpg", true
		}
		return metadata.Filename, ext == ".jpg" || ext == ".jpeg"
	case "image/png":
		return filenameWithExtension(metadata.Filename, ext, ".png")
	case "image/webp":
		return filenameWithExtension(metadata.Filename, ext, ".webp")
	case "image/gif":
		return filenameWithExtension(metadata.Filename, ext, ".gif")
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return filenameWithExtension(metadata.Filename, ext, ".docx")
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return filenameWithExtension(metadata.Filename, ext, ".xlsx")
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return filenameWithExtension(metadata.Filename, ext, ".pptx")
	case "application/epub+zip":
		return filenameWithExtension(metadata.Filename, ext, ".epub")
	case "text/html":
		if metadata.Filename == "" {
			return "document.html", true
		}
		return metadata.Filename, ext == ".html" || ext == ".htm"
	default:
		return "", false
	}
}

func filenameWithExtension(filename, extension, required string) (string, bool) {
	if filename == "" {
		return "document" + required, true
	}
	return filename, extension == required
}

func validFingerprint(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func compareFormats(left, right document.RenditionFormatCapability) int {
	if value := strings.Compare(left.MediaFamily, right.MediaFamily); value != 0 {
		return value
	}
	if value := strings.Compare(left.MediaType, right.MediaType); value != 0 {
		return value
	}
	return strings.Compare(string(left.InputKind), string(right.InputKind))
}
