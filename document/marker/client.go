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
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"reflect"
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
	timestampForm           = "2006-01-02T15:04:05.000000000Z"
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
	maxSecretBytes          = 64 << 10
	pageSeparator           = "------------------------------------------------"
)

var _ document.RenditionProvider = (*Client)(nil)

// SecretResolver resolves an optional operator-fronted Marker credential.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, name string) (string, error)
}

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
	secrets    SecretResolver
	http       *http.Client
}

// New validates a pinned self-hosted profile. The injected transport is the
// operator's hardened destination policy; Client removes ambient cookies and
// always refuses redirects.
func New(profile Profile, secrets SecretResolver, transport http.RoundTripper) (*Client, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	descriptor, err := document.NewRenditionDescriptor(profile.Descriptor)
	if err != nil || !reflect.DeepEqual(descriptor, profile.Descriptor) {
		if err == nil {
			err = errors.New("descriptor is not canonical")
		}
		return nil, fmt.Errorf("marker: invalid descriptor: %w", err)
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
	if normalized.SecretBinding == "" {
		if !nilValue(secrets) {
			return nil, errors.New("marker: secret resolver requires a named binding")
		}
	} else if nilValue(secrets) {
		return nil, errors.New("marker: named secret binding requires a resolver")
	}
	if transport == nil {
		return nil, errors.New("marker: hardened transport is required")
	}
	normalized.Descriptor = descriptor
	return &Client{profile: normalized, descriptor: providerutil.CloneDescriptor(descriptor), secrets: secrets,
		http: providerhttp.IsolateClient(&http.Client{Transport: transport})}, nil
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
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected, "Marker input exceeds the document byte limit", nil)
	}
	filename, ok := uploadFilename(metadata)
	if !ok {
		return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput, "Marker input filename does not match the authorized format", nil)
	}
	metadata.Filename = filename
	expiresAt, err := time.Parse(timestampForm, authorization.ExpiresAt)
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected, "Marker authorization expiry is invalid", nil)
	}
	operationCtx, cancel := operationContext(ctx, expiresAt, client.profile.RequestTimeout)
	defer cancel()
	if err := checkOperation(operationCtx, expiresAt); err != nil {
		return document.RenditionResult{}, err
	}
	source, err := providerutil.ReadAuthorizedUpload(operationCtx, upload, metadata, "Marker")
	if err != nil {
		return document.RenditionResult{}, err
	}
	defer clear(source)
	expectedNaturalUnits := 0
	switch metadata.MediaFamily {
	case "pdf":
		pages, countErr := formatdetect.CountPDFPages(source)
		if countErr != nil || pages <= 0 {
			return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput, "Marker PDF page authority is invalid", countErr)
		}
		if pages > int64(client.profile.MaxUnits) {
			return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected, "Marker PDF exceeds the unit limit", nil)
		}
		expectedNaturalUnits = int(pages)
	case "image":
		detected, detectErr := media.DetectBytes(source, metadata.MediaType)
		if detectErr != nil {
			return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput, "Marker image authority is invalid", detectErr)
		}
		if detected.Kind != media.KindImage || detected.MediaType != metadata.MediaType {
			return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput, "Marker image identity does not match the authorization", nil)
		}
		if detected.FrameCount != 1 || detected.Animated {
			return document.RenditionResult{}, providerError(document.RenditionErrorUnsupportedInput, "Marker requires a single-frame image", nil)
		}
		expectedNaturalUnits = 1
	}
	body, contentType, err := buildMultipart(metadata, source, client.profile.Mode, client.profile.MaxRequestBytes)
	if err != nil {
		return document.RenditionResult{}, err
	}
	started := time.Now().UTC()
	request, err := http.NewRequestWithContext(operationCtx, http.MethodPost, client.profile.Origin+uploadPath, bytes.NewReader(body))
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorTransient, "could not prepare Marker request", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", contentType)
	if err := client.authorize(request); err != nil {
		return document.RenditionResult{}, err
	}
	if err := checkOperation(operationCtx, expiresAt); err != nil {
		return document.RenditionResult{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		if operationErr := checkOperation(operationCtx, expiresAt); operationErr != nil {
			return document.RenditionResult{}, operationErr
		}
		return document.RenditionResult{}, providerError(document.RenditionErrorAmbiguousSubmission, "Marker submission outcome is unknown", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseLimit := min(client.profile.MaxResponseBytes, int64(authorization.MaxTotalResultBytes))
	responseBody, err := readBounded(operationCtx, expiresAt, response.Body, responseLimit)
	if err != nil {
		return document.RenditionResult{}, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return document.RenditionResult{}, statusError(response.StatusCode)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return document.RenditionResult{}, malformedError("Marker response content type is invalid", mediaErr)
	}
	if len(responseBody) > authorization.MaxTotalResultBytes {
		return document.RenditionResult{}, malformedError("Marker response exceeds authorization", nil)
	}
	result, warnings, err := client.parseResult(responseBody, metadata.MediaFamily, expectedNaturalUnits)
	if err != nil {
		return document.RenditionResult{}, err
	}
	providerMarkdown := result.markdown
	if authorization.MaxProviderMarkdownBytes == 0 {
		providerMarkdown = nil
	} else if len(providerMarkdown) > authorization.MaxProviderMarkdownBytes {
		return document.RenditionResult{}, malformedError("Marker Markdown exceeds authorization", nil)
	}
	completed := time.Now().UTC()
	authorizationFingerprint, err := authorization.Fingerprint()
	if err != nil {
		return document.RenditionResult{}, providerError(document.RenditionErrorPolicyRejected,
			"Marker authorization fingerprint is invalid", err)
	}
	return document.RenditionResult{Evidence: result.evidence, ProviderMarkdown: providerMarkdown,
		Receipt: document.RenditionReceipt{ProviderID: client.descriptor.ID,
			DescriptorFingerprint:       client.descriptor.Fingerprint,
			PolicyFingerprint:           authorization.PolicyFingerprint,
			RenditionRequestFingerprint: authorization.RenditionRequestFingerprint,
			AuthorizationFingerprint:    authorizationFingerprint,
			SourceSHA256:                metadata.SHA256, OperationID: "marker-" + authorization.RenditionRequestFingerprint[:24],
			StartedAt: started.Format(timestampForm), CompletedAt: completed.Format(timestampForm), Warnings: warnings,
			Usage: document.RenditionUsage{Requests: 1, InputBytes: int64(len(source)), OutputBytes: int64(len(responseBody)), Units: int64(len(result.evidence.Units))}}}, nil
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
		return parsedResult{}, nil, malformedError("Marker result JSON is invalid", err)
	}
	if wire.Success == nil || !*wire.Success || wire.Format == nil || *wire.Format != "markdown" ||
		wire.Output == nil || *wire.Output == "" || wire.Images == nil || len(wire.Metadata) == 0 || wire.Error != "" {
		return parsedResult{}, nil, malformedError("Marker result is incomplete or unsuccessful", nil)
	}
	if len(wire.Metadata) > int(client.profile.MaxMetadataBytes) {
		return parsedResult{}, nil, malformedError("Marker metadata exceeds byte limit", nil)
	}
	if err := validateImages(wire.Images, client.profile.MaxImages, client.profile.MaxImageBytes); err != nil {
		return parsedResult{}, nil, err
	}
	stats, err := parseMetadata(wire.Metadata, client.profile.MaxUnits)
	if err != nil {
		return parsedResult{}, nil, err
	}
	if expectedNaturalUnits > 0 && !statsProveUnits(stats, expectedNaturalUnits) {
		return parsedResult{}, nil, malformedError("Marker result does not prove complete source units", nil)
	}
	markdown := []byte(*wire.Output)
	if providerutil.InjectsDocbankFrontmatter(markdown) {
		return parsedResult{}, nil, malformedError("Marker Markdown contains reserved Docbank frontmatter", nil)
	}
	evidence, natural := naturalEvidence(family, *wire.Output, stats)
	if !natural {
		evidence = providerutil.DegradedEvidence(family, *wire.Output,
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
		return nil, malformedError("Marker metadata schema changed", err)
	}
	if len(metadata.TableOfContents) == 0 || metadata.PageStats == nil || len(metadata.PageStats) > maximum {
		return nil, malformedError("Marker metadata is incomplete or exceeds the unit limit", nil)
	}
	stats := make([]pageStat, len(metadata.PageStats))
	for index, stat := range metadata.PageStats {
		if stat.PageID == nil || *stat.PageID < 0 || len(stat.TextExtractionMethod) > 128 ||
			len(stat.BlockCounts) == 0 || len(stat.BlockMetadata) == 0 {
			return nil, malformedError("Marker page metadata is incomplete", nil)
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

func validateImages(images map[string]string, maximum int, maxBytes int64) error {
	if len(images) > maximum {
		return malformedError("Marker returned too many images", nil)
	}
	for name, encoded := range images {
		if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) ||
			int64(base64.StdEncoding.DecodedLen(len(encoded))) > maxBytes {
			return malformedError("Marker image output is invalid or exceeds limits", nil)
		}
		decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(encoded))
		count, err := io.Copy(io.Discard, io.LimitReader(decoder, maxBytes+1))
		if err != nil || count > maxBytes {
			return malformedError("Marker image output is invalid or exceeds limits", err)
		}
	}
	return nil
}

func buildMultipart(metadata document.AuthorizedUploadMetadata, source []byte, mode string, maximum int64) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("file", metadata.Filename))
	header.Set("Content-Type", metadata.MediaType)
	part, err := writer.CreatePart(header)
	if err == nil {
		_, err = part.Write(source)
	}
	for _, field := range [][2]string{{"mode", mode}, {"force_ocr", "false"}, {"paginate_output", "true"}, {"output_format", "markdown"}} {
		if err == nil {
			err = writer.WriteField(field[0], field[1])
		}
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		return nil, "", providerError(document.RenditionErrorTransient, "could not prepare Marker upload", err)
	}
	if int64(body.Len()) > maximum {
		return nil, "", providerError(document.RenditionErrorPolicyRejected, "Marker multipart request exceeds byte limit", nil)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (client *Client) authorize(request *http.Request) error {
	if client.profile.SecretBinding == "" {
		return nil
	}
	secret, err := client.secrets.ResolveSecret(request.Context(), client.profile.SecretBinding)
	if err != nil || secret == "" || len(secret) > maxSecretBytes || strings.ContainsAny(secret, "\r\n\x00") {
		return providerError(document.RenditionErrorAuthentication, "Marker credential is unavailable or invalid", err)
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	return nil
}

func normalizeProfile(profile Profile) (Profile, error) {
	origin, err := validateOrigin(profile.Origin)
	if err != nil {
		return Profile{}, err
	}
	profile.Origin = origin
	if !validFingerprint(profile.DeploymentFingerprint) || !validFingerprint(profile.RuntimeFingerprint) {
		return Profile{}, errors.New("marker: deployment and runtime fingerprints must be lowercase SHA-256")
	}
	if profile.SecretBinding != "" && !validToken(profile.SecretBinding) {
		return Profile{}, errors.New("marker: secret binding is invalid")
	}
	if profile.Mode != "fast" && profile.Mode != "balanced" {
		return Profile{}, errors.New("marker: mode must be fast or balanced")
	}
	if profile.RequestTimeout == 0 {
		profile.RequestTimeout = defaultRequestTimeout
	}
	if profile.MaxDocumentBytes == 0 {
		profile.MaxDocumentBytes = defaultMaxDocumentBytes
	}
	if profile.MaxRequestBytes == 0 {
		profile.MaxRequestBytes = defaultMaxRequestBytes
	}
	if profile.MaxResponseBytes == 0 {
		profile.MaxResponseBytes = defaultMaxResponseBytes
	}
	if profile.MaxMetadataBytes == 0 {
		profile.MaxMetadataBytes = defaultMaxMetadataBytes
	}
	if profile.MaxImages == 0 {
		profile.MaxImages = defaultMaxImages
	}
	if profile.MaxImageBytes == 0 {
		profile.MaxImageBytes = defaultMaxImageBytes
	}
	if profile.MaxUnits == 0 {
		profile.MaxUnits = defaultMaxUnits
	}
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > maxTimeout || profile.MaxDocumentBytes <= 0 || profile.MaxDocumentBytes > maxDocumentBytes ||
		profile.MaxRequestBytes <= profile.MaxDocumentBytes || profile.MaxRequestBytes > maxRequestBytes ||
		profile.MaxResponseBytes <= 0 || profile.MaxResponseBytes > maxResponseBytes || profile.MaxMetadataBytes <= 0 || profile.MaxMetadataBytes > maxMetadataBytes ||
		profile.MaxMetadataBytes > profile.MaxResponseBytes || profile.MaxImages < 1 || profile.MaxImages > maxImages ||
		profile.MaxImageBytes <= 0 || profile.MaxImageBytes > maxImageBytes || profile.MaxImageBytes > profile.MaxResponseBytes ||
		profile.MaxUnits < 1 || profile.MaxUnits > maxUnits {
		return Profile{}, errors.New("marker: execution bounds are invalid")
	}
	return profile, nil
}

func validateOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Opaque != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("marker: origin must be one HTTP(S) origin without path, credentials, query, or fragment")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
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

func operationContext(ctx context.Context, expiresAt time.Time, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if caller, ok := ctx.Deadline(); ok && caller.Before(deadline) {
		deadline = caller
	}
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	return context.WithDeadline(ctx, deadline)
}

func checkOperation(ctx context.Context, expiresAt time.Time) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return providerError(document.RenditionErrorCanceled, "Marker rendering canceled", ctx.Err())
	}
	if !time.Now().Before(expiresAt) {
		return expiredError()
	}
	if err := ctx.Err(); err != nil {
		return providerError(document.RenditionErrorCanceled, "Marker rendering canceled", err)
	}
	return nil
}

func readBounded(ctx context.Context, expiresAt time.Time, reader io.Reader, maximum int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		if operationErr := checkOperation(ctx, expiresAt); operationErr != nil {
			return nil, operationErr
		}
		return nil, providerError(document.RenditionErrorAmbiguousSubmission, "could not read Marker result", err)
	}
	if operationErr := checkOperation(ctx, expiresAt); operationErr != nil {
		return nil, operationErr
	}
	if int64(len(value)) > maximum {
		return nil, malformedError("Marker response exceeds byte limit", nil)
	}
	return value, nil
}

func statusError(status int) error {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError && status <= 599 {
		return providerError(document.RenditionErrorAmbiguousSubmission, "Marker submission outcome is unknown", nil)
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return providerError(document.RenditionErrorAuthentication, "Marker authentication failed", nil)
	case http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return providerError(document.RenditionErrorUnsupportedInput, "Marker rejected the input format", nil)
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return providerError(document.RenditionErrorPolicyRejected, "Marker rejected the upload", nil)
	default:
		return malformedError("Marker returned an unexpected HTTP status", nil)
	}
}

func expiredError() error {
	return providerError(document.RenditionErrorPolicyRejected, "Marker authorization expired", nil)
}
func malformedError(message string, cause error) error {
	return providerError(document.RenditionErrorMalformedEvidence, message, cause)
}

func providerError(code document.RenditionErrorCode, message string, cause error) error {
	return providerutil.ClassifiedError("Marker", code, message, cause)
}

func validFingerprint(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func validToken(value string) bool {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("_.-", character) {
			continue
		}
		return false
	}
	return true
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

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
