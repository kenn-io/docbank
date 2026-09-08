package openaihosted

import (
	"context"

	"go.kenn.io/docbank/document"
)

// Receipt is bounded configured provider identity and numeric usage without
// request, response, vector, source, or credential material.
type Receipt struct {
	ProviderID            string
	DescriptorFingerprint string
	PolicyFingerprint     string
	Model                 string
	ModelRevision         string
	RequestCount          int
	PromptTokens          int64
	TotalTokens           int64
}

// Execution contains the validated embedding result and its request-local receipt.
type Execution struct {
	Result  document.EmbeddingResult
	Receipt Receipt
}

// EmbedWithReceipt executes the same authorized request path as Embed and
// returns the provider's validated numeric usage with configured identity.
func (client *Client) EmbedWithReceipt(
	ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization,
) (Execution, error) {
	return client.embed(ctx, inputs, authorization)
}

func executionReceipt(descriptor document.EmbeddingDescriptor, promptTokens, totalTokens int64) Receipt {
	return Receipt{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint: descriptor.PolicyFingerprint, Model: descriptor.Model,
		ModelRevision: descriptor.ModelRevision, RequestCount: 1,
		PromptTokens: promptTokens, TotalTokens: totalTokens,
	}
}
