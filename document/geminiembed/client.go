package geminiembed

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

const (
	maxSecretBytes         = 64 << 10
	maximumBatchItems      = 10_000
	maximumHeadingDepth    = 64
	maximumSourceSpanCount = 1024
)

var _ document.EmbeddingProvider = (*Client)(nil)

func (client *Client) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	execution, err := client.EmbedWithReceipt(ctx, inputs, authorization)
	return execution.Result, err
}

func (client *Client) EmbedWithReceipt(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (Execution, error) {
	if client == nil || ctx == nil {
		return Execution{}, errors.New("gemini embed: client and context are required")
	}
	receipt := newReceipt(client)
	result, err := client.embed(ctx, inputs, authorization, &receipt)
	if err != nil {
		return Execution{Receipt: receipt}, err
	}
	return Execution{Result: result, Receipt: receipt}, nil
}

func (client *Client) embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization, receipt *Receipt) (result document.EmbeddingResult, retErr error) {
	if len(inputs) == 0 || len(inputs) > maximumBatchItems || authorization.MaxBatchItems < len(inputs) ||
		authorization.MaxBatchItems > maximumBatchItems {
		return document.EmbeddingResult{}, errors.New("gemini embed: embedding batch bounds are invalid")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	frozen, enrolled, enrollmentErr := enrollOriginalUploads(inputs)
	defer func() {
		if closeErr := closeEnrolledUploads(enrolled); closeErr != nil && !errors.Is(retErr, closeErr) {
			retErr = errors.Join(retErr, fmt.Errorf("gemini embed: close original upload: %w", closeErr))
		}
	}()
	if enrollmentErr != nil {
		return document.EmbeddingResult{}, enrollmentErr
	}
	if err := requestCtx.Err(); err != nil {
		return document.EmbeddingResult{}, fmt.Errorf("gemini embed: embedding canceled: %w", err)
	}
	if err := document.ValidateEmbeddingProviderRequest(client, frozen, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxInputBytes > client.profile.MaxInputBytes || authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return document.EmbeddingResult{}, errors.New("gemini embed: embedding authorization exceeds profile capacity")
	}
	sourceGate := newActiveSourceGate()
	interruptFinished := make(chan struct{})
	stopInterrupt := context.AfterFunc(requestCtx, func() {
		sourceGate.Cancel()
		close(interruptFinished)
	})
	defer func() {
		if !stopInterrupt() {
			<-interruptFinished
		}
	}()
	prepared := make([]preparedInput, len(frozen))
	defer clearPreparedInputs(prepared)
	var total int64
	for index, input := range frozen {
		switch {
		case input.Role == document.EmbeddingRoleDocument && input.Kind == document.EmbeddingInputRenditionChunk:
			text := client.descriptor.ModelInput.EncodeDocument(input.Text)
			if int64(len(text)) > client.profile.MaxInputBytes-total {
				return document.EmbeddingResult{}, errors.New("gemini embed: embedding input exceeds profile byte capacity")
			}
			total += int64(len(text))
			payload, err := client.marshalRequest(wirePart{Text: text})
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			prepared[index].payload = payload
		case input.Role == document.EmbeddingRoleQuery && input.Kind == document.EmbeddingInputQueryText:
			text := client.descriptor.ModelInput.EncodeQuery(input.Text)
			if int64(len(text)) > client.profile.MaxInputBytes-total {
				return document.EmbeddingResult{}, errors.New("gemini embed: embedding input exceeds profile byte capacity")
			}
			total += int64(len(text))
			payload, err := client.marshalRequest(wirePart{Text: text})
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			prepared[index].payload = payload
		case input.Role == document.EmbeddingRoleDocument && input.Kind == document.EmbeddingInputOriginalFile:
			frozenUpload, ok := input.Source.(*frozenUpload)
			if !ok {
				return document.EmbeddingResult{}, errors.New("gemini embed: original upload was not frozen")
			}
			if err := client.validateDirectCapability(frozenUpload.enrolled, authorization); err != nil {
				return document.EmbeddingResult{}, err
			}
			if frozenUpload.enrolled.facts.SourceBytes > client.profile.MaxInputBytes-total {
				return document.EmbeddingResult{}, errors.New("gemini embed: embedding input exceeds profile byte capacity")
			}
			total += frozenUpload.enrolled.facts.SourceBytes
			if client.profile.Transport == TransportFilesAPI {
				if err := client.preflightFileRequest(frozenUpload.enrolled.metadata.MediaType, frozenUpload.enrolled.facts.SourceBytes); err != nil {
					return document.EmbeddingResult{}, err
				}
			} else {
				if err := client.preflightInlineRequest(frozenUpload.enrolled.metadata.MediaType, frozenUpload.enrolled.facts.SourceBytes); err != nil {
					return document.EmbeddingResult{}, err
				}
			}
			verified, err := client.readDirectFile(requestCtx, frozenUpload.enrolled, sourceGate)
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			prepared[index].file = &verified
			if client.profile.Transport == TransportInline {
				payload, err := client.marshalRequest(wirePart{InlineData: &wireInlineData{
					MIMEType: verified.metadata.MediaType, Data: base64.StdEncoding.EncodeToString(verified.data),
				}})
				if err != nil {
					return document.EmbeddingResult{}, err
				}
				prepared[index].payload = payload
			}
		default:
			return document.EmbeddingResult{}, errors.New("gemini embed: unsupported input role or kind")
		}
	}
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if contextErr := requestCtx.Err(); contextErr != nil {
		return document.EmbeddingResult{}, fmt.Errorf("gemini embed: credential resolution canceled: %w", contextErr)
	}
	if err != nil || !validSecret(secret) {
		return document.EmbeddingResult{}, errors.New("gemini embed: API-key resolution failed")
	}
	result = document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(frozen))}
	for index := range prepared {
		var vector []float32
		if prepared[index].file != nil && client.profile.Transport == TransportFilesAPI {
			vector, err = client.executeFile(requestCtx, *prepared[index].file, secret, receipt)
		} else {
			vector, err = client.execute(requestCtx, prepared[index].payload, secret, receipt)
		}
		if err != nil {
			return document.EmbeddingResult{}, err
		}
		result.Vectors[index] = document.EmbeddingVector{Key: frozen[index].Key, Values: vector}
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, frozen, authorization, result); err != nil {
		return document.EmbeddingResult{}, errors.New("gemini embed: provider response violates embedding contract")
	}
	return result, nil
}

type preparedInput struct {
	payload []byte
	file    *verifiedFile
}

func clearPreparedInputs(prepared []preparedInput) {
	for index := range prepared {
		clear(prepared[index].payload)
		if prepared[index].file != nil {
			clear(prepared[index].file.data)
		}
	}
}

func validSecret(value string) bool { return validToken(value) && len(value) <= maxSecretBytes }
