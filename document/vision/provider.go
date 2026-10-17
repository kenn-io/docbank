// Package vision describes sanitized photo previews through a compatible chat endpoint.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"golang.org/x/text/unicode/norm"
)

const AdapterContract = document.VisionAdapterContract
const ProviderID = "compatible-vision.describe-v1"
const MaxRequestTimeout = 10 * time.Minute
const InputProfile = "fit-jpeg-1024-metadata-stripped-v1"

var captionMarkdown = []*regexp.Regexp{
	regexp.MustCompile(`\*\*[^*]+\*\*`), regexp.MustCompile(`\*[^*\s]([^*]*[^*\s])?\*`),
	regexp.MustCompile(`(?m)^\s*#\s`), regexp.MustCompile(`\[[^\]]+\]\([^)]+\)`), regexp.MustCompile(`(?m)^\s*>\s`),
	regexp.MustCompile(`(?m)^\s*[-*]\s`), regexp.MustCompile(`(?m)^\s*\d+\.\s`), regexp.MustCompile(`(^|[^\pL\pN_])_[^_\s]([^_]*[^_\s])?_($|[^\pL\pN_])`),
}

const prompt = `Look at this photograph and describe what is visible. Output strictly valid JSON, no markdown or commentary:
{"caption":"...","tags":["..."],"visible_text":"..."}
Caption: 1 to 3 neutral English sentences, at most 300 characters, plain text. Tags: up to 10 free-text English tags, most salient first, describing subjects, scenes, settings, mood, weather, time of day. Visible text: transcribe legible text exactly, without interpreting it. Do not name specific people, invent place names, or include camera/lens metadata. Return empty strings and an empty tag array when nothing can be described.`
const provider = providerutil.Provider("vision")

type Profile struct {
	Origin           string
	Model            string
	TrustBoundary    document.RenditionTrustBoundary
	SecretBinding    string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

type Provider struct {
	profile    Profile
	descriptor document.RenditionDescriptor
	http       *http.Client
	credential providerutil.Credential
}

// Descriptor binds model, prompt, and input policy to the provider identity.
func Descriptor(model string, trust document.RenditionTrustBoundary) (document.RenditionDescriptor, error) {
	if model == "" || len(model) > 255 || model != strings.TrimSpace(model) || strings.ContainsAny(model, "\r\n\x00") {
		return document.RenditionDescriptor{}, errors.New("vision model is invalid")
	}
	fingerprint := providerutil.SHA256Hex([]byte(AdapterContract + "\x00" + model + "\x00" + InputProfile + "\x00" + prompt))
	return document.NewRenditionDescriptor(document.RenditionDescriptor{ID: ProviderID, ContractVersion: document.RenditionProviderContractVersion, PolicyFingerprint: fingerprint, TrustBoundary: trust, SupportedFormats: []document.RenditionFormatCapability{{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputDerivedUpload}}, ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}})
}

// DisclosureFingerprint binds consent to the exact destination and deployment.
func DisclosureFingerprint(descriptor document.RenditionDescriptor, endpoint, deployment string) string {
	return providerutil.SHA256Hex([]byte(strings.Join([]string{AdapterContract, descriptor.ID, descriptor.Fingerprint, endpoint, deployment}, "\x00")))
}

func New(profile Profile, secrets providerutil.SecretResolver, client *http.Client) (*Provider, error) {
	descriptor, err := Descriptor(profile.Model, profile.TrustBoundary)
	if err != nil {
		return nil, err
	}
	origin, err := provider.ValidateOrigin(profile.Origin, profile.TrustBoundary)
	if err != nil {
		return nil, err
	}
	profile.Origin = origin
	if profile.RequestTimeout <= 0 || profile.RequestTimeout > MaxRequestTimeout || profile.MaxResponseBytes <= 0 || profile.MaxResponseBytes > 1<<20 {
		return nil, errors.New("vision request bounds are invalid")
	}
	credential := providerutil.BearerCredential(profile.SecretBinding, secrets)
	if err := credential.Validate(provider); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("vision HTTP client is required")
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Provider{profile: profile, descriptor: descriptor, http: &copyClient, credential: credential}, nil
}
func (p *Provider) Descriptor() document.RenditionDescriptor {
	return providerutil.CloneDescriptor(p.descriptor)
}

type part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}
type imageURL struct {
	URL string `json:"url"`
}
type message struct {
	Role    string `json:"role"`
	Content []part `json:"content"`
}
type request struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature *int      `json:"temperature,omitempty"`
}
type response struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func (p *Provider) describe(ctx context.Context, data []byte, text string, tokens int, temperature *int, expiresAt string, onSubmission func()) ([]byte, error) {
	body, err := json.Marshal(request{Model: p.profile.Model, Messages: []message{{Role: "user", Content: []part{{Type: "text", Text: text}, {Type: "image_url", ImageURL: &imageURL{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)}}}}}, MaxTokens: tokens, Temperature: temperature})
	if err != nil {
		return nil, err
	}
	var operation *providerutil.Operation
	if expiresAt == "" {
		operation = providerutil.NewResumedOperation(ctx, provider, p.profile.RequestTimeout)
	} else {
		operation, err = providerutil.NewOperation(ctx, provider, expiresAt, p.profile.RequestTimeout)
		if err != nil {
			return nil, err
		}
	}
	defer operation.Cancel()
	executor := providerutil.Executor{Provider: provider, HTTP: p.http, Origin: p.profile.Origin,
		RequestTimeout: p.profile.RequestTimeout, MaxResponseBytes: p.profile.MaxResponseBytes, Credential: p.credential}
	var usage providerutil.Usage
	res, err := executor.Do(operation, &usage, providerutil.Request{Stage: providerutil.StageResult,
		Method: http.MethodPost, Path: "/v1/chat/completions", ContentType: providerutil.JSONMediaType, Body: body, OnSubmission: onSubmission})
	if err != nil {
		return nil, err
	}
	if !res.Success() {
		return nil, provider.StatusError(providerutil.StageResult, res.Status, res.RetryAfter, nil)
	}
	raw := res.Body
	var decoded response
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, provider.Malformed("invalid vision response", err)
	}
	if len(decoded.Choices) != 1 || decoded.Choices[0].FinishReason == "length" {
		return nil, provider.Malformed("vision response is incomplete", nil)
	}
	return []byte(decoded.Choices[0].Message.Content), nil
}

// Parse validates a complete description and normalizes tags.
func Parse(raw []byte) (document.PhotoDescription, error) {
	body := strings.TrimSpace(string(raw))
	if rest, ok := strings.CutPrefix(body, "```"); ok {
		body = rest
		body = strings.TrimPrefix(body, "json")
		body = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	var value struct {
		Caption     *string   `json:"caption"`
		Tags        *[]string `json:"tags"`
		VisibleText *string   `json:"visible_text"`
	}
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		return document.PhotoDescription{}, err
	}
	if value.Caption == nil || value.Tags == nil || value.VisibleText == nil {
		return document.PhotoDescription{}, errors.New("vision response is missing a field")
	}
	out := document.PhotoDescription{Caption: strings.TrimSpace(*value.Caption), Tags: []string{}, VisibleText: strings.TrimSpace(*value.VisibleText)}
	if utf8.RuneCountInString(out.Caption) > 300 || strings.ContainsAny(out.Caption, "`\n") {
		return document.PhotoDescription{}, errors.New("vision caption exceeds plain-text contract")
	}
	for _, pattern := range captionMarkdown {
		if pattern.MatchString(*value.Caption) {
			return document.PhotoDescription{}, errors.New("vision caption contains markdown")
		}
	}

	seen := map[string]bool{}
	for _, raw := range *value.Tags {
		tag := strings.ToLower(norm.NFKC.String(raw))
		tag = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.S, r) && r != '+' || r >= 0x180b && r <= 0x180d || r >= 0xfe00 && r <= 0xfe0f || r >= 0xe0100 && r <= 0xe01ef {
				return -1
			}
			return r
		}, tag)
		tag = norm.NFC.String(tag)
		tag = strings.Join(strings.Fields(tag), " ")
		tag = strings.TrimFunc(tag, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '#' })
		if strings.ContainsFunc(tag, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) && !seen[tag] {
			seen[tag] = true
			out.Tags = append(out.Tags, tag)
		}
		if len(out.Tags) == 10 {
			break
		}
	}
	if err := document.ValidatePhotoDescription(out); err != nil {
		return document.PhotoDescription{}, err
	}
	return out, nil
}
func (p *Provider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	return p.RenderWithRetryAuthorization(ctx, upload, authorization, nil)
}

// RenderWithRetryAuthorization checks current authority before a second request.
func (p *Provider) RenderWithRetryAuthorization(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization, beforeRetry func(context.Context) (context.Context, error)) (document.RenditionResult, error) {
	started := time.Now().UTC()
	meta := upload.Metadata()
	if meta.InputKind != document.RenditionInputDerivedUpload || meta.ByteLength > 4<<20 {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "vision requires a bounded preview upload", nil)
	}
	data, err := providerutil.ReadAuthorizedUpload(ctx, upload, meta, string(provider))
	if err != nil {
		return document.RenditionResult{}, err
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || max(cfg.Width, cfg.Height) > 1024 {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorUnsupportedInput, "vision input must be a JPEG at most 1024px per edge", err)
	}
	var raw []byte
	var description document.PhotoDescription
	var requests int64
	for attempt := range 2 {
		if attempt > 0 && beforeRetry != nil {
			ctx, err = beforeRetry(ctx)
			if err != nil {
				return document.RenditionResult{}, err
			}
		}
		requests++
		raw, err = p.describe(ctx, data, prompt, 2048, nil, authorization.ExpiresAt, nil)
		if err == nil {
			description, err = Parse(raw)
			if err != nil {
				err = provider.Malformed("invalid photo description", err)
			}
		}
		if err == nil {
			break
		}
		classified, ok := errors.AsType[*document.RenditionProviderError](err)
		if !ok || classified.Code() != document.RenditionErrorMalformedEvidence || attempt == 1 {
			return document.RenditionResult{}, err
		}
	}
	payload, err := json.Marshal(description)
	if err != nil {
		return document.RenditionResult{}, err
	}
	checksum := providerutil.SHA256Hex(payload)
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{Descriptor: p.descriptor, Authorization: authorization, SourceSHA256: authorization.SourceSHA256, UploadSHA256: meta.SHA256, OperationID: "describe-" + authorization.RenditionRequestFingerprint, StartedAt: started, CompletedAt: time.Now().UTC(), Usage: document.RenditionUsage{Requests: requests, Retries: requests - 1, InputBytes: requests * meta.ByteLength, OutputBytes: int64(len(raw)), Units: 1}})
	if err != nil {
		return document.RenditionResult{}, err
	}
	evidence := providerutil.DegradedEvidence("image", "", "photo description is a proposal without source coordinates")
	evidence.Artifacts = []document.SourceEvidenceArtifactV1{{ProviderID: "description", Pointer: "description.json", Role: document.EvidenceArtifactStructured, SHA256: checksum}}
	return document.RenditionResult{Evidence: evidence, Artifacts: []document.RenditionArtifact{{Role: document.EvidenceArtifactStructured, MediaType: "application/json", Payload: payload, SHA256: checksum}}, Receipt: receipt}, nil
}

// Probe exercises the same image request with synthetic bytes, without accessing a vault.
func (p *Provider) Probe(ctx context.Context) error {
	probeImage := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := range 32 {
		for x := range 64 {
			pixel := color.RGBA{R: 255, A: 255}
			if x >= 32 {
				pixel = color.RGBA{B: 255, A: 255}
			}
			probeImage.SetRGBA(x, y, pixel)
		}
	}
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, probeImage, nil); err != nil {
		return fmt.Errorf("encode probe JPEG: %w", err)
	}
	response, err := p.describe(ctx, jpegData.Bytes(), "What are the colors of the left and right halves of this image? Reply with two lowercase color names separated by a comma and no other text.", 24, new(0), "", nil)
	if err != nil {
		// Synthetic probes can retry safely and retain the provider's delay.
		if classified, ok := errors.AsType[*document.RenditionProviderError](err); ok && classified.Code() == document.RenditionErrorAmbiguousSubmission {
			if cause, ok := errors.AsType[*document.RenditionProviderError](classified.Unwrap()); ok {
				return cause
			}
		}
		return err
	}
	answer := strings.TrimRight(strings.ToLower(strings.TrimSpace(string(response))), ".!?")
	colors := strings.Split(answer, ",")
	if len(colors) != 2 || strings.TrimSpace(colors[0]) != "red" || strings.TrimSpace(colors[1]) != "blue" {
		return provider.Malformed("vision probe did not identify the synthetic image", nil)
	}
	return nil
}
