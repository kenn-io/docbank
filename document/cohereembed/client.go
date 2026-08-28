package cohereembed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/cohereapi"
)

const (
	maxSecretBytes = 64 << 10
	maxUsageValue  = float64(1 << 50)
)

var _ document.EmbeddingProvider = (*Client)(nil)

type wireRequest struct {
	Model           string      `json:"model"`
	Texts           []string    `json:"texts,omitempty"`
	Inputs          []wireInput `json:"inputs,omitempty"`
	InputType       string      `json:"input_type"`
	EmbeddingTypes  []string    `json:"embedding_types"`
	Truncate        string      `json:"truncate"`
	OutputDimension int         `json:"output_dimension"`
}

type wireInput struct {
	Content []wireContent `json:"content"`
}

type wireContent struct {
	Type     string       `json:"type"`
	ImageURL wireImageURL `json:"image_url"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

type wireResponse struct {
	ID           string `json:"id"`
	ResponseType string `json:"response_type,omitempty"`
	Embeddings   struct {
		Float   [][]float32 `json:"float"`
		Int8    [][]int8    `json:"int8,omitempty"`
		Uint8   [][]uint8   `json:"uint8,omitempty"`
		Binary  [][]int8    `json:"binary,omitempty"`
		UBinary [][]uint8   `json:"ubinary,omitempty"`
		Base64  []string    `json:"base64,omitempty"`
	} `json:"embeddings"`
	Texts  []string          `json:"texts,omitempty"`
	Images []wireImage       `json:"images,omitempty"`
	Meta   *wireResponseMeta `json:"meta,omitempty"`
}

type wireImage struct {
	Width    int64  `json:"width"`
	Height   int64  `json:"height"`
	Format   string `json:"format"`
	BitDepth int    `json:"bit_depth"`
}

type wireResponseMeta struct {
	APIVersion *struct {
		Version        string `json:"version"`
		IsDeprecated   *bool  `json:"is_deprecated,omitempty"`
		IsExperimental *bool  `json:"is_experimental,omitempty"`
	} `json:"api_version,omitempty"`
	BilledUnits *struct {
		Images          *float64 `json:"images,omitempty"`
		InputTokens     *float64 `json:"input_tokens,omitempty"`
		ImageTokens     *float64 `json:"image_tokens,omitempty"`
		OutputTokens    *float64 `json:"output_tokens,omitempty"`
		SearchUnits     *float64 `json:"search_units,omitempty"`
		Classifications *float64 `json:"classifications,omitempty"`
		Pages           *float64 `json:"pages,omitempty"`
	} `json:"billed_units,omitempty"`
	Tokens *struct {
		InputTokens  *float64 `json:"input_tokens,omitempty"`
		OutputTokens *float64 `json:"output_tokens,omitempty"`
	} `json:"tokens,omitempty"`
	CachedTokens *float64 `json:"cached_tokens,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

type preparedRequest struct {
	positions []int
	payload   []byte
	texts     []string
	images    []string
	imageMeta []wireImage
}

// Receipt is bounded provider provenance and numeric usage without request,
// response, vector, source, or credential material.
type Receipt struct {
	ProviderID            string
	DescriptorFingerprint string
	PolicyFingerprint     string
	Model                 string
	ModelRevision         string
	RequestCount          int
	ImageInputs           int
	BilledImages          float64
	InputTokens           float64
	ImageTokens           float64
	OutputTokens          float64
	SearchUnits           float64
	Classifications       float64
	Pages                 float64
	CachedTokens          float64
	ProviderResponseIDs   []string
}

// Execution contains the E1 result plus its sanitized provider receipt.
type Execution struct {
	Result  document.EmbeddingResult
	Receipt Receipt
}

func (client *Client) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	return client.embed(ctx, inputs, authorization, nil)
}

// EmbedWithReceipt executes the same E1 boundary while returning bounded,
// sanitized provider provenance for callers that persist execution receipts.
func (client *Client) EmbedWithReceipt(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (Execution, error) {
	receipt := Receipt{ProviderID: ProviderID, DescriptorFingerprint: client.Descriptor().Fingerprint,
		PolicyFingerprint: client.Descriptor().PolicyFingerprint, Model: Model,
		ModelRevision: client.Descriptor().ModelRevision}
	result, err := client.embed(ctx, inputs, authorization, &receipt)
	if err != nil {
		return Execution{}, err
	}
	return Execution{Result: result, Receipt: receipt}, nil
}

func (client *Client) embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization, receipt *Receipt) (document.EmbeddingResult, error) {
	if client == nil || ctx == nil {
		return document.EmbeddingResult{}, errors.New("cohere embed: client and context are required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	frozenInputs, enrolled := enrollOriginalUploads(inputs)
	defer closeEnrolledUploads(enrolled)
	if contextErr := requestCtx.Err(); contextErr != nil {
		return document.EmbeddingResult{}, contextErr
	}
	if err := document.ValidateEmbeddingProviderRequest(client, frozenInputs, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxBatchItems > client.profile.MaxBatchItems || authorization.MaxInputBytes > client.profile.MaxInputBytes ||
		authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrCapacityResponse}
	}
	sourceGate := newActiveSourceGate()
	closeFinished := make(chan struct{})
	stopClose := context.AfterFunc(requestCtx, func() {
		sourceGate.Cancel()
		close(closeFinished)
	})
	defer func() {
		if !stopClose() {
			<-closeFinished
		}
	}()
	prepared, err := client.prepareRequests(requestCtx, frozenInputs, sourceGate)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	defer func() {
		for index := range prepared {
			clear(prepared[index].payload)
			for image := range prepared[index].images {
				prepared[index].images[image] = ""
			}
		}
	}()
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !cohereapi.ValidToken(secret, maxSecretBytes) {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, fmt.Errorf("cohere embed: credential resolution canceled: %w", contextErr)
		}
		return document.EmbeddingResult{}, errors.New("cohere embed: API-key resolution failed")
	}
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(frozenInputs))}
	for index := range prepared {
		vectors, err := client.execute(requestCtx, prepared[index], secret, receipt)
		if err != nil {
			return document.EmbeddingResult{}, err
		}
		for local, global := range prepared[index].positions {
			result.Vectors[global] = document.EmbeddingVector{Key: frozenInputs[global].Key, Values: vectors[local]}
		}
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, frozenInputs, authorization, result); err != nil {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	return result, nil
}

func (client *Client) prepareRequests(ctx context.Context, inputs []document.EmbeddingInput, sourceGate *activeSourceGate) ([]preparedRequest, error) {
	documentPositions, queryPositions, imagePositions := []int{}, []int{}, []int{}
	documents, queries := []string{}, []string{}
	sources := make([]*enrolledUpload, 0, len(inputs))
	for _, input := range inputs {
		if input.Kind == document.EmbeddingInputOriginalFile {
			frozen, ok := input.Source.(*frozenUpload)
			if !ok {
				return nil, errors.New("cohere embed: original upload was not frozen")
			}
			sources = append(sources, frozen.enrolled)
		}
	}
	var imageBytes int64
	for index, input := range inputs {
		switch {
		case input.Kind == document.EmbeddingInputOriginalFile:
			metadata := input.Source.Metadata()
			if metadata.ByteLength > client.profile.MaxInputItemBytes || metadata.ByteLength > client.profile.MaxImageBytes-imageBytes {
				return nil, &ProviderError{Kind: ErrCapacityResponse}
			}
			imageBytes += metadata.ByteLength
			imagePositions = append(imagePositions, index)
		case input.Role == document.EmbeddingRoleDocument:
			rendered := client.descriptor.ModelInput.EncodeDocument(input.Text)
			if int64(len(rendered)) > client.profile.MaxInputItemBytes {
				return nil, &ProviderError{Kind: ErrCapacityResponse}
			}
			documentPositions, documents = append(documentPositions, index), append(documents, rendered)
		case input.Role == document.EmbeddingRoleQuery:
			rendered := client.descriptor.ModelInput.EncodeQuery(input.Text)
			if int64(len(rendered)) > client.profile.MaxInputItemBytes {
				return nil, &ProviderError{Kind: ErrCapacityResponse}
			}
			queryPositions, queries = append(queryPositions, index), append(queries, rendered)
		}
	}
	images := make([]string, len(imagePositions))
	imageMeta := make([]wireImage, len(imagePositions))
	for local := range sources {
		source := sources[local]
		if source.liveMetadata() != source.metadata {
			clearStrings(images)
			return nil, errors.New("cohere embed: image source changed or could not be read exactly")
		}
		token, ok := sourceGate.Begin(source)
		if !ok {
			clearStrings(images)
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}
			return nil, errors.New("cohere embed: image source transfer stopped")
		}
		dataURL, detected, readErr := client.readImage(source.source, source.metadata)
		sourceGate.End(token)
		metadataChanged := source.liveMetadata() != source.metadata
		closeErr := source.Close()
		if contextErr := ctx.Err(); contextErr != nil {
			clearStrings(images)
			return nil, contextErr
		}
		if readErr != nil || closeErr != nil || metadataChanged {
			clearStrings(images)
			return nil, errors.New("cohere embed: image source changed or could not be read exactly")
		}
		images[local] = dataURL
		imageMeta[local] = detected
	}
	requests := make([]preparedRequest, 0, 3)
	for _, candidate := range []struct {
		positions []int
		texts     []string
		images    []string
		inputType string
	}{{documentPositions, documents, nil, "search_document"}, {imagePositions, nil, images, "search_document"}, {queryPositions, queries, nil, "search_query"}} {
		if len(candidate.positions) == 0 {
			continue
		}
		payload, err := json.Marshal(wireRequest{Model: Model, Texts: candidate.texts,
			Inputs:    wireInputs(candidate.images),
			InputType: candidate.inputType, EmbeddingTypes: []string{"float"}, Truncate: "NONE",
			OutputDimension: client.descriptor.Dimension})
		if err != nil {
			return nil, errors.New("cohere embed: request encoding failed")
		}
		if int64(len(payload)) > client.profile.MaxRequestBytes {
			clear(payload)
			return nil, &ProviderError{Kind: ErrCapacityResponse}
		}
		requests = append(requests, preparedRequest{positions: slices.Clone(candidate.positions), payload: payload,
			texts: slices.Clone(candidate.texts), images: slices.Clone(candidate.images)})
		if len(candidate.images) != 0 {
			requests[len(requests)-1].imageMeta = slices.Clone(imageMeta)
		}
	}
	return requests, nil
}

func wireInputs(images []string) []wireInput {
	if len(images) == 0 {
		return nil
	}
	inputs := make([]wireInput, len(images))
	for index, image := range images {
		inputs[index] = wireInput{Content: []wireContent{{Type: "image_url", ImageURL: wireImageURL{URL: image}}}}
	}
	return inputs
}

type enrolledUpload struct {
	source   document.AuthorizedUpload
	metadata document.AuthorizedUploadMetadata

	closeOnce sync.Once
	closeErr  error
}

func (upload *enrolledUpload) Close() error {
	upload.closeOnce.Do(func() { upload.closeErr = upload.source.Close() })
	return upload.closeErr
}

func (upload *enrolledUpload) liveMetadata() document.AuthorizedUploadMetadata {
	return upload.source.Metadata()
}

type frozenUpload struct {
	enrolled *enrolledUpload
}

func (upload *frozenUpload) Read(value []byte) (int, error) {
	return upload.enrolled.source.Read(value)
}
func (upload *frozenUpload) Close() error { return upload.enrolled.Close() }
func (upload *frozenUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.enrolled.metadata
}

func enrollOriginalUploads(inputs []document.EmbeddingInput) ([]document.EmbeddingInput, []*enrolledUpload) {
	frozen := slices.Clone(inputs)
	enrolled := make([]*enrolledUpload, 0, len(inputs))
	for index := range frozen {
		frozen[index].HeadingPath = slices.Clone(frozen[index].HeadingPath)
		frozen[index].SourceSpans = slices.Clone(frozen[index].SourceSpans)
		if frozen[index].Kind != document.EmbeddingInputOriginalFile || nilInterface(frozen[index].Source) {
			continue
		}
		upload := &enrolledUpload{source: frozen[index].Source, metadata: frozen[index].Source.Metadata()}
		enrolled = append(enrolled, upload)
		frozen[index].Source = &frozenUpload{enrolled: upload}
	}
	return frozen, enrolled
}

func closeEnrolledUploads(uploads []*enrolledUpload) {
	for _, upload := range uploads {
		_ = upload.Close()
	}
}

func clearStrings(values []string) {
	for index := range values {
		values[index] = ""
	}
}

type activeSourceGate struct {
	mu          sync.Mutex
	canceled    bool
	nextToken   uint64
	activeToken uint64
	active      *enrolledUpload
}

func newActiveSourceGate() *activeSourceGate { return new(activeSourceGate) }

func (gate *activeSourceGate) Begin(source *enrolledUpload) (uint64, bool) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.canceled {
		return 0, false
	}
	gate.nextToken++
	gate.activeToken = gate.nextToken
	gate.active = source
	return gate.activeToken, true
}

func (gate *activeSourceGate) End(token uint64) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.activeToken != token {
		return
	}
	gate.active = nil
	gate.activeToken = 0
}

func (gate *activeSourceGate) Cancel() {
	gate.mu.Lock()
	if gate.canceled {
		gate.mu.Unlock()
		return
	}
	gate.canceled = true
	active := gate.active
	gate.active = nil
	gate.activeToken = 0
	gate.mu.Unlock()
	if active != nil {
		_ = active.Close()
	}
}

func (client *Client) readImage(source document.AuthorizedUpload, metadata document.AuthorizedUploadMetadata) (string, wireImage, error) {
	data, readErr := io.ReadAll(io.LimitReader(source, metadata.ByteLength+1))
	defer clear(data)
	if readErr != nil || int64(len(data)) != metadata.ByteLength {
		return "", wireImage{}, errors.New("cohere embed: image source changed or could not be read exactly")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != metadata.SHA256 {
		return "", wireImage{}, errors.New("cohere embed: image source checksum changed")
	}
	detected, reason := media.InspectBytes(data, metadata.MediaType, client.profile.MediaPolicy)
	if reason != media.ReasonEligible || detected.Kind != media.KindImage || detected.MediaType != metadata.MediaType ||
		detected.Size != metadata.ByteLength || !slices.Contains(acceptedImageFormats, detected.MediaType) {
		return "", wireImage{}, errors.New("cohere embed: image source media identity is invalid")
	}
	encoded := make([]byte, 0, len("data:")+len(detected.MediaType)+len(";base64,")+base64.StdEncoding.EncodedLen(len(data)))
	encoded = append(encoded, "data:"...)
	encoded = append(encoded, detected.MediaType...)
	encoded = append(encoded, ";base64,"...)
	encoded = base64.StdEncoding.AppendEncode(encoded, data)
	result := string(encoded)
	clear(encoded)
	return result, wireImage{Width: detected.Width, Height: detected.Height, Format: string(detected.Format)}, nil
}

func (client *Client) execute(ctx context.Context, prepared preparedRequest, secret string, receipt *Receipt) ([][]float32, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+embedPath, bytes.NewReader(prepared.payload))
	if err != nil {
		return nil, errors.New("cohere embed: request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("cohere embed: request canceled: %w", contextErr)
		}
		return nil, &ProviderError{Kind: ErrTransientResponse}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if !cohereapi.IsJSONContentType(response.Header.Get("Content-Type")) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	body, outcome, readErr := cohereapi.ReadBounded(ctx, response.Body, client.profile.MaxResponseBytes)
	switch outcome {
	case cohereapi.ReadOK:
	case cohereapi.ReadCanceled:
		return nil, fmt.Errorf("cohere embed: response read canceled: %w", readErr)
	case cohereapi.ReadTransient:
		return nil, &ProviderError{Kind: ErrTransientResponse}
	case cohereapi.ReadCapacity:
		return nil, &ProviderError{Kind: ErrCapacityResponse}
	}
	defer clear(body)
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if !cohereapi.ValidToken(decoded.ID, 128) || decoded.ResponseType != "" && decoded.ResponseType != "embeddings_by_type" ||
		len(decoded.Embeddings.Float) != len(prepared.positions) ||
		len(decoded.Embeddings.Int8) != 0 || len(decoded.Embeddings.Uint8) != 0 ||
		len(decoded.Embeddings.Binary) != 0 || len(decoded.Embeddings.UBinary) != 0 || len(decoded.Embeddings.Base64) != 0 ||
		decoded.Texts != nil && !slices.Equal(decoded.Texts, prepared.texts) ||
		!validImageMetadata(decoded.Images, prepared.imageMeta) || !validMeta(decoded.Meta, len(prepared.imageMeta)) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	for _, vector := range decoded.Embeddings.Float {
		if len(vector) != client.descriptor.Dimension {
			return nil, &ProviderError{Kind: ErrPermanentResponse}
		}
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, &ProviderError{Kind: ErrPermanentResponse}
			}
		}
	}
	if receipt != nil && !addReceipt(receipt, decoded, len(prepared.imageMeta)) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	return decoded.Embeddings.Float, nil
}

func addReceipt(receipt *Receipt, response wireResponse, imageInputs int) bool {
	values := make([]*float64, 8)
	if response.Meta != nil {
		if response.Meta.BilledUnits != nil {
			values = []*float64{response.Meta.BilledUnits.Images, response.Meta.BilledUnits.InputTokens,
				response.Meta.BilledUnits.ImageTokens, response.Meta.BilledUnits.OutputTokens,
				response.Meta.BilledUnits.SearchUnits, response.Meta.BilledUnits.Classifications,
				response.Meta.BilledUnits.Pages, response.Meta.CachedTokens}
		} else {
			values[7] = response.Meta.CachedTokens
		}
		if response.Meta.Tokens != nil {
			if values[1] == nil {
				values[1] = response.Meta.Tokens.InputTokens
			}
			if values[3] == nil {
				values[3] = response.Meta.Tokens.OutputTokens
			}
		}
	}
	totals := []*float64{&receipt.BilledImages, &receipt.InputTokens, &receipt.ImageTokens,
		&receipt.OutputTokens, &receipt.SearchUnits, &receipt.Classifications, &receipt.Pages, &receipt.CachedTokens}
	for index, value := range values {
		if value != nil {
			if *value > maxUsageValue-*totals[index] {
				return false
			}
			*totals[index] += *value
		}
	}
	receipt.RequestCount++
	receipt.ImageInputs += imageInputs
	receipt.ProviderResponseIDs = append(receipt.ProviderResponseIDs, response.ID)
	return true
}

func validImageMetadata(actual, expected []wireImage) bool {
	if actual == nil {
		return len(expected) == 0
	}
	if len(actual) != len(expected) {
		return false
	}
	for index, image := range actual {
		if image.Width != expected[index].Width || image.Height != expected[index].Height ||
			image.Format != expected[index].Format || image.BitDepth < 1 || image.BitDepth > 64 {
			return false
		}
	}
	return true
}

func validMeta(metadata *wireResponseMeta, expectedImages int) bool {
	if metadata == nil {
		return true
	}
	if metadata.APIVersion != nil && metadata.APIVersion.Version == "" {
		return false
	}
	if metadata.BilledUnits != nil && metadata.Tokens != nil &&
		(!matchingUsage(metadata.BilledUnits.InputTokens, metadata.Tokens.InputTokens) ||
			!matchingUsage(metadata.BilledUnits.OutputTokens, metadata.Tokens.OutputTokens)) {
		return false
	}
	values := []*float64{}
	if metadata.BilledUnits != nil {
		values = append(values, metadata.BilledUnits.Images, metadata.BilledUnits.InputTokens,
			metadata.BilledUnits.ImageTokens, metadata.BilledUnits.OutputTokens,
			metadata.BilledUnits.SearchUnits, metadata.BilledUnits.Classifications, metadata.BilledUnits.Pages)
	}
	if metadata.BilledUnits != nil && metadata.BilledUnits.Images != nil && *metadata.BilledUnits.Images != float64(expectedImages) {
		return false
	}
	if expectedImages == 0 && metadata.BilledUnits != nil && metadata.BilledUnits.ImageTokens != nil &&
		*metadata.BilledUnits.ImageTokens != 0 {
		return false
	}
	if metadata.Tokens != nil {
		values = append(values, metadata.Tokens.InputTokens, metadata.Tokens.OutputTokens)
	}
	values = append(values, metadata.CachedTokens)
	for _, value := range values {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > maxUsageValue) {
			return false
		}
	}
	return true
}

func matchingUsage(left, right *float64) bool {
	return left == nil || right == nil || *left == *right
}
