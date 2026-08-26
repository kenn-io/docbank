package embeddingbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"hash"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/manifestjson"
)

var (
	_                       document.EmbeddingProvider = (*Client)(nil)
	errSourceChanged                                   = errors.New("embedding bridge source changed")
	errSourceTransferFailed                            = errors.New("embedding bridge source transfer failed")
	errTransferStopped                                 = errors.New("embedding bridge transfer stopped")
)

// Descriptor returns an immutable copy of the configured vector-space identity.
func (client *Client) Descriptor() document.EmbeddingDescriptor {
	if client == nil {
		return document.EmbeddingDescriptor{}
	}
	return cloneDescriptor(client.descriptor)
}

// Embed performs one bounded synchronous request against the fixed same-origin route.
func (client *Client) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if client == nil {
		return document.EmbeddingResult{}, classified(ErrorPermanent, 0)
	}
	if ctx == nil {
		return document.EmbeddingResult{}, classified(ErrorPermanent, 0)
	}
	frozenInputs, err := freezeInputMetadata(inputs)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	if err := document.ValidateEmbeddingProviderRequest(client, frozenInputs, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxBatchItems > client.maxBatchItems || authorization.MaxInputBytes > client.maxInputBytes ||
		authorization.MaxResponseBytes > client.maxResponseBytes {
		return document.EmbeddingResult{}, classified(ErrorCapacity, 0)
	}
	manifest, encoded, err := client.buildManifest(frozenInputs, authorization)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	boundary := "docbank-embedding-" + manifest.RequestChecksum[:32]
	contentLength, err := multipartLength(boundary, encoded, frozenInputs)
	if err != nil || contentLength > client.maxRequestBytes {
		return document.EmbeddingResult{}, classified(ErrorCapacity, 0)
	}

	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
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
	secret := ""
	if client.secretBinding != "" {
		var resolveErr error
		secret, resolveErr = client.secrets.ResolveSecret(requestCtx, client.secretBinding)
		if resolveErr != nil || !validSecret(secret) {
			if contextErr := requestCtx.Err(); contextErr != nil {
				return document.EmbeddingResult{}, contextErr
			}
			return document.EmbeddingResult{}, classified(ErrorAuthentication, 0)
		}
	}

	bodyReader, bodyWriter := io.Pipe()
	writerDone := make(chan error, 1)
	go func() {
		multipartWriter := multipart.NewWriter(bodyWriter)
		if err := multipartWriter.SetBoundary(boundary); err != nil {
			_ = bodyWriter.CloseWithError(err)
			writerDone <- err
			return
		}
		writeErr := writeMultipart(multipartWriter, encoded, frozenInputs, true, sourceGate)
		if closeErr := multipartWriter.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = bodyWriter.CloseWithError(writeErr)
		writerDone <- writeErr
	}()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.origin+embeddingsPath, bodyReader)
	if err != nil {
		_ = bodyReader.Close()
		<-writerDone
		return document.EmbeddingResult{}, classified(ErrorPermanent, 0)
	}
	request.ContentLength = contentLength
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	request.Header.Set("Accept", responseMediaType)
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	request.Header.Set("Idempotency-Key", manifest.RequestChecksum)
	request.Header.Set("Docbank-Request-Checksum", manifest.RequestChecksum)
	if hasOriginalInput(frozenInputs) {
		request.Header.Set("Expect", "100-continue")
	}

	response, doErr := client.http.Do(request)
	_ = bodyReader.Close()
	sourceGate.Cancel()
	writeErr := <-writerDone
	if contextErr := requestCtx.Err(); contextErr != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return document.EmbeddingResult{}, contextErr
	}
	if errors.Is(writeErr, errSourceChanged) {
		if response != nil {
			_ = response.Body.Close()
		}
		return document.EmbeddingResult{}, classified(ErrorSourceChanged, 0)
	}
	if doErr != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		if contextErr := requestCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, contextErr
		}
		return document.EmbeddingResult{}, classified(ErrorAmbiguousSubmission, 0)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return document.EmbeddingResult{}, statusError(response.StatusCode)
	}
	if writeErr != nil {
		return document.EmbeddingResult{}, classified(ErrorAmbiguousSubmission, 0)
	}
	if err := requireResponseMediaType(response.Header.Get("Content-Type")); err != nil {
		return document.EmbeddingResult{}, classified(ErrorMalformedResponse, response.StatusCode)
	}
	body, err := readBounded(response.Body, client.maxResponseBytes)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, contextErr
		}
		return document.EmbeddingResult{}, classified(ErrorMalformedResponse, response.StatusCode)
	}
	if err := manifestjson.RejectDuplicateKeys(body, "embedding bridge response"); err != nil {
		return document.EmbeddingResult{}, classified(ErrorMalformedResponse, response.StatusCode)
	}
	var envelope Response
	if err := json.Unmarshal(body, &envelope, json.RejectUnknownMembers(true)); err != nil {
		return document.EmbeddingResult{}, classified(ErrorMalformedResponse, response.StatusCode)
	}
	result, err := client.validateResponse(envelope, manifest, frozenInputs, authorization)
	if err != nil {
		return document.EmbeddingResult{}, classified(ErrorMalformedResponse, response.StatusCode)
	}
	return result, nil
}

func freezeInputMetadata(inputs []document.EmbeddingInput) ([]document.EmbeddingInput, error) {
	frozen := slices.Clone(inputs)
	for index := range frozen {
		input := &frozen[index]
		input.HeadingPath = slices.Clone(input.HeadingPath)
		input.SourceSpans = slices.Clone(input.SourceSpans)
		if input.Kind != document.EmbeddingInputOriginalFile || nilInterface(input.Source) {
			continue
		}
		metadata := input.Source.Metadata()
		if !safeMultipartFilename(metadata.Filename) {
			return nil, classified(ErrorPermanent, 0)
		}
		input.Source = frozenUpload{AuthorizedUpload: input.Source, metadata: metadata}
	}
	return frozen, nil
}

func (client *Client) buildManifest(inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (RequestManifest, []byte, error) {
	manifest := RequestManifest{
		ContractVersion: ContractVersion, DescriptorFingerprint: client.descriptor.Fingerprint,
		PolicyFingerprint: client.descriptor.PolicyFingerprint, Authorization: authorization,
		Inputs: make([]ManifestInput, len(inputs)),
	}
	fileIndex := 0
	for index, input := range inputs {
		entry := ManifestInput{
			Index: index, Key: input.Key, Role: input.Role, Kind: input.Kind,
			HeadingPath: slices.Clone(input.HeadingPath), SourceSpans: manifestSpans(input.SourceSpans),
		}
		if input.Kind == document.EmbeddingInputOriginalFile {
			metadata := input.Source.Metadata()
			entry.ByteLength = metadata.ByteLength
			entry.SHA256 = metadata.SHA256
			entry.Upload = new(metadata)
			entry.FilePart = filePartName
			entry.FileIndex = new(fileIndex)
			fileIndex++
		} else {
			if input.Role == document.EmbeddingRoleDocument {
				entry.Text = client.descriptor.ModelInput.EncodeDocument(input.Text)
			} else {
				entry.Text = client.descriptor.ModelInput.EncodeQuery(input.Text)
			}
			entry.ByteLength = int64(len(entry.Text))
			entry.SHA256 = sha256Hex([]byte(entry.Text))
		}
		manifest.Inputs[index] = entry
	}
	identity, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		return RequestManifest{}, nil, classified(ErrorPermanent, 0)
	}
	manifest.RequestChecksum = sha256Hex(identity)
	encoded, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		return RequestManifest{}, nil, classified(ErrorPermanent, 0)
	}
	return manifest, encoded, nil
}

func manifestSpans(values []document.ChunkSpan) []ManifestSpan {
	if values == nil {
		return nil
	}
	result := make([]ManifestSpan, len(values))
	for index, value := range values {
		result[index] = ManifestSpan{UnitIndex: value.UnitIndex, CharStart: value.CharStart, CharEnd: value.CharEnd}
	}
	return result
}

func hasOriginalInput(inputs []document.EmbeddingInput) bool {
	return slices.ContainsFunc(inputs, func(input document.EmbeddingInput) bool {
		return input.Kind == document.EmbeddingInputOriginalFile
	})
}

func multipartLength(boundary string, manifest []byte, inputs []document.EmbeddingInput) (int64, error) {
	counter := new(countingWriter)
	writer := multipart.NewWriter(counter)
	if err := writer.SetBoundary(boundary); err != nil {
		return 0, errors.New("embedding bridge: deterministic multipart boundary is invalid")
	}
	if err := writeMultipart(writer, manifest, inputs, false, nil); err != nil {
		return 0, err
	}
	if err := writer.Close(); err != nil {
		return 0, errors.New("embedding bridge: multipart measurement failed")
	}
	total := counter.written
	for _, input := range inputs {
		if input.Kind == document.EmbeddingInputOriginalFile {
			if input.Source.Metadata().ByteLength > maxRequestBytes-total {
				return 0, errors.New("multipart request length overflow")
			}
			total += input.Source.Metadata().ByteLength
		}
	}
	return total, nil
}

func writeMultipart(writer *multipart.Writer, manifest []byte, inputs []document.EmbeddingInput, writeFiles bool, sourceGate *activeSourceGate) error {
	manifestHeader := make(textproto.MIMEHeader)
	manifestHeader.Set("Content-Disposition", `form-data; name="`+manifestPartName+`"`)
	manifestHeader.Set("Content-Type", manifestMediaType)
	part, err := writer.CreatePart(manifestHeader)
	if err != nil {
		return errors.New("embedding bridge: manifest multipart part failed")
	}
	if _, err := part.Write(manifest); err != nil {
		return err
	}
	for _, input := range inputs {
		if input.Kind != document.EmbeddingInputOriginalFile {
			continue
		}
		metadata := input.Source.Metadata()
		fileHeader := make(textproto.MIMEHeader)
		fileHeader.Set("Content-Disposition", multipart.FileContentDisposition(filePartName, metadata.Filename))
		fileHeader.Set("Content-Type", metadata.MediaType)
		part, err := writer.CreatePart(fileHeader)
		if err != nil {
			return errors.New("embedding bridge: file multipart part failed")
		}
		if !writeFiles {
			continue
		}
		token, ok := sourceGate.Begin(input.Source)
		if !ok {
			return errTransferStopped
		}
		copyErr := copyAuthorizedFile(part, input.Source, metadata.ByteLength, metadata.SHA256)
		sourceGate.End(token)
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}

func copyAuthorizedFile(destination io.Writer, source io.Reader, expectedLength int64, expectedSHA256 string) error {
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, digest), io.LimitReader(source, expectedLength+1))
	if err != nil {
		return errSourceTransferFailed
	}
	if written != expectedLength || !strings.EqualFold(hexDigest(digest), expectedSHA256) {
		return errSourceChanged
	}
	return nil
}

func (client *Client) validateResponse(envelope Response, manifest RequestManifest, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if envelope.ContractVersion != ContractVersion || envelope.DescriptorFingerprint != client.descriptor.Fingerprint ||
		envelope.PolicyFingerprint != client.descriptor.PolicyFingerprint || envelope.RequestChecksum != manifest.RequestChecksum ||
		len(envelope.Vectors) != len(inputs) {
		return document.EmbeddingResult{}, errors.New("response identity mismatch")
	}
	vectors := make([]document.EmbeddingVector, len(inputs))
	seenKeys := make(map[string]struct{}, len(inputs))
	for position, vector := range envelope.Vectors {
		if vector.Index == nil || *vector.Index != position || vector.Key != inputs[position].Key {
			return document.EmbeddingResult{}, errors.New("response order mismatch")
		}
		if _, exists := seenKeys[vector.Key]; exists {
			return document.EmbeddingResult{}, errors.New("response duplicate key")
		}
		seenKeys[vector.Key] = struct{}{}
		if len(vector.Values) != client.descriptor.Dimension {
			return document.EmbeddingResult{}, errors.New("response dimension mismatch")
		}
		for _, value := range vector.Values {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return document.EmbeddingResult{}, errors.New("response non-finite vector")
			}
		}
		vectors[position] = document.EmbeddingVector{Key: vector.Key, Values: slices.Clone(vector.Values)}
	}
	result := document.EmbeddingResult{Vectors: vectors}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, inputs, authorization, result); err != nil {
		return document.EmbeddingResult{}, err
	}
	return result, nil
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return classified(ErrorAuthentication, status)
	case http.StatusRequestEntityTooLarge:
		return classified(ErrorCapacity, status)
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return classified(ErrorTransient, status)
	default:
		return classified(ErrorPermanent, status)
	}
}

func requireResponseMediaType(value string) error {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/vnd.docbank.embedding-result+json" || len(parameters) != 1 || parameters["version"] != "1" {
		return errors.New("response media type mismatch")
	}
	return nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || int64(len(value)) > maximum {
		return nil, errors.New("bounded response read failed")
	}
	return value, nil
}

type countingWriter struct{ written int64 }

func (writer *countingWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > maxRequestBytes-writer.written {
		return 0, errors.New("multipart request length overflow")
	}
	writer.written += int64(len(value))
	return len(value), nil
}

func hexDigest(value hash.Hash) string { return hex.EncodeToString(value.Sum(nil)) }

type frozenUpload struct {
	document.AuthorizedUpload

	metadata document.AuthorizedUploadMetadata
}

func (upload frozenUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

func safeMultipartFilename(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

type activeSourceGate struct {
	mu          sync.Mutex
	canceled    bool
	nextToken   uint64
	activeToken uint64
	active      document.AuthorizedUpload
}

func newActiveSourceGate() *activeSourceGate { return new(activeSourceGate) }

func (gate *activeSourceGate) Begin(source document.AuthorizedUpload) (uint64, bool) {
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
	if !nilInterface(active) {
		_ = active.Close()
	}
}
