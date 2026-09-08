package geminiembed

import (
	"time"

	"go.kenn.io/docbank/document"
)

type ModalityTokenCounts struct {
	Unspecified int64
	Text        int64
	Image       int64
	Video       int64
	Audio       int64
	Document    int64
}

// Receipt is bounded provider provenance and numeric usage without content.
type Receipt struct {
	ProviderID                 string
	DescriptorFingerprint      string
	PolicyFingerprint          string
	Model                      string
	ModelRevision              string
	Transport                  Transport
	Semantics                  Semantics
	RequestCount               int
	PromptTokens               int64
	PromptTokenDetails         ModalityTokenCounts
	EmbeddingResponseCount     int
	UsageResponseCount         int
	ProviderResponseIDs        []string
	OmittedProviderResponseIDs int
	Warnings                   []string
	ProviderRetentionCeiling   time.Duration
	UnconfirmedFileRetentions  int
}

type Execution struct {
	Result  document.EmbeddingResult
	Receipt Receipt
}

func newReceipt(client *Client) Receipt {
	return Receipt{
		ProviderID: ProviderID, DescriptorFingerprint: client.descriptor.Fingerprint,
		PolicyFingerprint: client.descriptor.PolicyFingerprint, Model: Model,
		ModelRevision: client.descriptor.ModelRevision, Transport: client.profile.Transport,
		Semantics: fixedSemantics(), ProviderRetentionCeiling: profileRetention(client.profile.Transport),
	}
}

func beginReceiptRequest(receipt *Receipt) bool {
	if receipt == nil {
		return true
	}
	if receipt.RequestCount == int(^uint(0)>>1) {
		return false
	}
	receipt.RequestCount++
	return true
}

func recordReceiptResponse(receipt *Receipt, usage *wireUsage, responseID string) bool {
	if receipt == nil {
		return true
	}
	if usage != nil && !validUsage(usage) {
		return false
	}
	if receipt.EmbeddingResponseCount == int(^uint(0)>>1) || usage != nil && receipt.UsageResponseCount == int(^uint(0)>>1) {
		return false
	}
	if !canRecordProviderResponseID(receipt, responseID) {
		return false
	}
	var prompt int64
	details := ModalityTokenCounts{}
	if usage != nil {
		if usage.PromptTokenCount.set {
			prompt = usage.PromptTokenCount.value
			if prompt > maxUsageValue-receipt.PromptTokens {
				return false
			}
		}
		for _, detail := range usage.PromptTokenDetails.values {
			if !detail.TokenCount.set {
				return false
			}
			value := detail.TokenCount.value
			var destination *int64
			switch detail.Modality {
			case "MODALITY_UNSPECIFIED":
				destination = &details.Unspecified
			case "TEXT":
				destination = &details.Text
			case "IMAGE":
				destination = &details.Image
			case "VIDEO":
				destination = &details.Video
			case "AUDIO":
				destination = &details.Audio
			case "DOCUMENT":
				destination = &details.Document
			default:
				return false
			}
			if value > maxUsageValue-*destination {
				return false
			}
			*destination += value
		}
		current := []struct{ existing, addition int64 }{
			{receipt.PromptTokenDetails.Unspecified, details.Unspecified}, {receipt.PromptTokenDetails.Text, details.Text},
			{receipt.PromptTokenDetails.Image, details.Image}, {receipt.PromptTokenDetails.Video, details.Video},
			{receipt.PromptTokenDetails.Audio, details.Audio}, {receipt.PromptTokenDetails.Document, details.Document},
		}
		for _, value := range current {
			if value.addition > maxUsageValue-value.existing {
				return false
			}
		}
	}
	receipt.EmbeddingResponseCount++
	if usage != nil {
		receipt.UsageResponseCount++
		receipt.PromptTokens += prompt
		receipt.PromptTokenDetails.Unspecified += details.Unspecified
		receipt.PromptTokenDetails.Text += details.Text
		receipt.PromptTokenDetails.Image += details.Image
		receipt.PromptTokenDetails.Video += details.Video
		receipt.PromptTokenDetails.Audio += details.Audio
		receipt.PromptTokenDetails.Document += details.Document
	}
	recordProviderResponseIDUnchecked(receipt, responseID)
	return true
}

func recordProviderResponseID(receipt *Receipt, responseID string) bool {
	if receipt == nil {
		return true
	}
	if !canRecordProviderResponseID(receipt, responseID) {
		return false
	}
	recordProviderResponseIDUnchecked(receipt, responseID)
	return true
}

func canRecordProviderResponseID(receipt *Receipt, responseID string) bool {
	if responseID == "" {
		return true
	}
	return validToken(responseID) &&
		(len(receipt.ProviderResponseIDs) < 128 || receipt.OmittedProviderResponseIDs != int(^uint(0)>>1))
}

func recordProviderResponseIDUnchecked(receipt *Receipt, responseID string) {
	if responseID == "" {
		return
	}
	if len(receipt.ProviderResponseIDs) < 128 {
		receipt.ProviderResponseIDs = append(receipt.ProviderResponseIDs, responseID)
	} else {
		receipt.OmittedProviderResponseIDs++
	}
}

func recordUnconfirmedFileRetention(receipt *Receipt) {
	if receipt == nil {
		return
	}
	if receipt.UnconfirmedFileRetentions < int(^uint(0)>>1) {
		receipt.UnconfirmedFileRetentions++
	}
	if len(receipt.Warnings) < 32 {
		receipt.Warnings = append(receipt.Warnings, "provider file retention is unconfirmed")
	}
}
