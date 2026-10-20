// Package vision describes sanitized photo previews through a compatible chat endpoint.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const AdapterContract = document.VisionAdapterContract
const ProviderID = "compatible-vision.describe-v1"
const MaxRequestTimeout = 10 * time.Minute
const InputProfile = "grid-jpeg-512-metadata-stripped-v1"

var captionListMarker = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
var captionStrong = regexp.MustCompile(`\*\*([^*]+)\*\*`)
var captionEmphasis = regexp.MustCompile(`\*([^*\s](?:[^*]*[^*\s])?)\*`)
var captionUnderscore = regexp.MustCompile(`(^|[^\pL\pN_])_([^_\s](?:[^_]*[^_\s])?)_($|[^\pL\pN_])`)
var captionMarkdown = []*regexp.Regexp{
	captionStrong, captionEmphasis,
	regexp.MustCompile(`(?m)^\s*#\s`), regexp.MustCompile(`\[[^\]]+\]\([^)]+\)`), regexp.MustCompile(`(?m)^\s*>\s`),
	regexp.MustCompile(`(?m)^\s*[-*]\s`), regexp.MustCompile(`(?m)^\s*\d+\.\s`), captionUnderscore,
}

const prompt = `Look at this photograph and describe what is visible. Output strictly valid JSON, no markdown or commentary:
{"caption":"...","tags":["..."],"visible_text":"..."}
Caption: 1 to 3 neutral English sentences, at most 300 characters, plain text. Tags: up to 10 free-text English tags, most salient first, describing subjects, scenes, settings, mood, weather, time of day. Visible text: make a best-effort transcription of legible text from this 512px preview, without interpreting it. Leave unclear text out rather than inventing it. Do not name specific people, invent place names, or include camera/lens metadata. Return empty strings and an empty tag array when nothing can be described.`
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
	Model               string    `json:"model"`
	Messages            []message `json:"messages"`
	MaxCompletionTokens int       `json:"max_completion_tokens"`
}
type response struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func (p *Provider) describe(ctx context.Context, data []byte, text string, tokens int, expiresAt string) ([]byte, error) {
	body, err := json.Marshal(request{Model: p.profile.Model, Messages: []message{{Role: "user", Content: []part{{Type: "text", Text: text}, {Type: "image_url", ImageURL: &imageURL{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)}}}}}, MaxCompletionTokens: tokens})
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
		Method: http.MethodPost, Path: "/v1/chat/completions", ContentType: providerutil.JSONMediaType, Body: body,
		// A transport retry would resend the photo after the operation's last check.
		NoReplay: true})
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

// Parse cleans a reply field by field. A caption, tag, or visible text that is
// invalid or of the wrong type is dropped alone, and over-long visible text is
// clipped, so one bad field never discards the rest. A reply that is not a JSON
// object with at least one of the three fields yields no result.
func Parse(raw []byte) (document.PhotoDescription, error) {
	body := strings.TrimSpace(string(raw))
	if rest, ok := strings.CutPrefix(body, "```"); ok {
		body = rest
		body = strings.TrimPrefix(body, "json")
		body = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	var fields struct {
		Caption     jsontext.Value `json:"caption"`
		Tags        jsontext.Value `json:"tags"`
		VisibleText jsontext.Value `json:"visible_text"`
	}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return document.PhotoDescription{}, err
	}
	if len(fields.Caption)+len(fields.Tags)+len(fields.VisibleText) == 0 {
		return document.PhotoDescription{}, errors.New("vision response has no description field")
	}
	out := document.PhotoDescription{Tags: []string{}}
	var caption string
	if json.Unmarshal(fields.Caption, &caption) == nil {
		caption = captionListMarker.ReplaceAllString(caption, "")
		caption = captionStrong.ReplaceAllString(caption, "$1")
		caption = captionEmphasis.ReplaceAllString(caption, "$1")
		caption = captionUnderscore.ReplaceAllString(caption, "$1$2$3")
		out.Caption = strings.Join(strings.Fields(caption), " ")
		if utf8.RuneCountInString(out.Caption) > 300 || strings.ContainsAny(out.Caption, "`\x00") || slices.ContainsFunc(captionMarkdown, func(pattern *regexp.Regexp) bool { return pattern.MatchString(caption) }) {
			out.Caption = ""
		}
	}
	var text string
	if json.Unmarshal(fields.VisibleText, &text) == nil {
		out.VisibleText = strings.TrimSpace(strings.ReplaceAll(text, "\x00", ""))
		if len(out.VisibleText) > document.MaxPhotoTextBytes {
			out.VisibleText = out.VisibleText[:document.MaxPhotoTextBytes]
			for !utf8.ValidString(out.VisibleText) {
				out.VisibleText = out.VisibleText[:len(out.VisibleText)-1]
			}
		}
	}
	var tags []jsontext.Value
	if json.Unmarshal(fields.Tags, &tags) != nil {
		tags = nil
	}
	// Strip disallowed characters only; the store owns NFC identity and deduplication.
	for _, rawTag := range tags {
		var raw string
		if json.Unmarshal(rawTag, &raw) != nil {
			continue
		}
		tag := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.S, r) && r != '+' || r >= 0x180b && r <= 0x180d || r >= 0xfe00 && r <= 0xfe0f || r >= 0xe0100 && r <= 0xe01ef {
				return -1
			}
			return r
		}, raw)
		tag = strings.Join(strings.Fields(tag), " ")
		tag = strings.TrimFunc(tag, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) && r != '+' && r != '#'
		})
		if len(tag) <= document.MaxPhotoTagBytes && strings.ContainsFunc(tag, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			out.Tags = append(out.Tags, tag)
		}
	}
	out.Tags = out.Tags[:min(10, len(out.Tags))]
	if err := document.ValidatePhotoDescription(out); err != nil {
		return document.PhotoDescription{}, err
	}
	return out, nil
}
func (p *Provider) Render(ctx context.Context, upload document.AuthorizedUpload, authorization document.RenditionAuthorization) (document.RenditionResult, error) {
	started := time.Now().UTC()
	meta := upload.Metadata()
	if meta.InputKind != document.RenditionInputDerivedUpload || meta.ByteLength > 4<<20 {
		return document.RenditionResult{}, provider.Classified(document.RenditionErrorPolicyRejected, "vision requires a bounded preview upload", nil)
	}
	data, err := providerutil.ReadAuthorizedUpload(ctx, upload, meta, string(provider))
	if err != nil {
		return document.RenditionResult{}, err
	}
	raw, err := p.describe(ctx, data, prompt, 2048, authorization.ExpiresAt)
	if err != nil {
		return document.RenditionResult{}, err
	}
	description, err := Parse(raw)
	if err != nil {
		return document.RenditionResult{}, provider.Malformed("invalid photo description", err)
	}
	payload, err := json.Marshal(description)
	if err != nil {
		return document.RenditionResult{}, err
	}
	checksum := providerutil.SHA256Hex(payload)
	receipt, err := providerutil.NewReceipt(provider, providerutil.Receipt{Descriptor: p.descriptor, Authorization: authorization, SourceSHA256: authorization.SourceSHA256, UploadSHA256: meta.SHA256, OperationID: "describe-" + authorization.RenditionRequestFingerprint, StartedAt: started, CompletedAt: time.Now().UTC(), Usage: document.RenditionUsage{Requests: 1, InputBytes: meta.ByteLength, OutputBytes: int64(len(raw)), Units: 1}})
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
	response, err := p.describe(ctx, jpegData.Bytes(), "What are the colors of the left and right halves of this image? Reply with two lowercase color names separated by a comma and no other text.", 2048, "")
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
	if len(colors) != 2 || strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(colors[0]), "left:")) != "red" || strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(colors[1]), "right:")) != "blue" {
		return provider.Malformed("vision probe did not identify the synthetic image", nil)
	}
	return nil
}
