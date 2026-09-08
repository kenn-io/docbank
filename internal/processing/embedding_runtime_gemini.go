package processing

import (
	"encoding/json/v2"
	"errors"
	"mime"
	"reflect"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/geminiembed"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/store"
)

const maxRuntimeProcessingProfileBytes = 1 << 20

func embeddingInspectionPolicy(work EmbeddingWork, provider document.EmbeddingProvider) (media.InspectionPolicy, error) {
	client, ok := provider.(*geminiembed.Client)
	if !ok {
		return directEmbeddingInspectionPolicy(work), nil
	}
	return geminiEmbeddingInspectionPolicy(work, client)
}

func geminiEmbeddingInspectionPolicy(work EmbeddingWork, client *geminiembed.Client) (media.InspectionPolicy, error) {
	if len(work.ProcessingProfile.CanonicalProfile) < 1 ||
		len(work.ProcessingProfile.CanonicalProfile) > maxRuntimeProcessingProfileBytes {
		return media.InspectionPolicy{}, errors.New("gemini embedding processing profile exceeds persistence authority")
	}
	if err := store.ValidateProcessingProfileRecord(work.ProcessingProfile); err != nil {
		return media.InspectionPolicy{}, errors.New("gemini embedding processing profile is invalid")
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(work.ProcessingProfile.CanonicalProfile, &profile, json.RejectUnknownMembers(true)); err != nil {
		return media.InspectionPolicy{}, errors.New("gemini embedding processing profile cannot be decoded")
	}
	var selected *document.EmbeddingBindingV1
	for index := range profile.Embeddings {
		if profile.Embeddings[index].Name == work.Binding.Name {
			selected = &profile.Embeddings[index]
			break
		}
	}
	if selected == nil || !reflect.DeepEqual(*selected, work.Binding) {
		return media.InspectionPolicy{}, errors.New("gemini embedding binding does not match processing profile")
	}
	descriptor := client.Descriptor()
	if !reflect.DeepEqual(descriptor, work.Descriptor) ||
		selected.Descriptor.ID != descriptor.ID || selected.Descriptor.Fingerprint != descriptor.Fingerprint {
		return media.InspectionPolicy{}, errors.New("gemini embedding descriptor does not match processing profile")
	}
	uploadPolicy := client.UploadPolicy()
	if selected.DisclosureFingerprint != uploadPolicy.DisclosureFingerprint {
		return media.InspectionPolicy{}, errors.New("gemini embedding disclosure does not match adapter policy")
	}
	maximum := min(selected.MaxInputBytes, uploadPolicy.MaxInputBytes)
	if maximum < 1 || work.SourceBytes > maximum {
		return media.InspectionPolicy{}, errors.New("gemini embedding source exceeds byte authority")
	}
	mediaType, _, err := mime.ParseMediaType(work.SourceMediaType)
	if err != nil || mediaType == "" {
		return media.InspectionPolicy{}, errors.New("gemini embedding declared media type is invalid")
	}
	semantics := client.Semantics()
	maximumDuration := semantics.MaxVideoDurationMS
	if strings.HasPrefix(mediaType, "audio/") {
		maximumDuration = semantics.MaxAudioDurationMS
	}
	policy := directEmbeddingInspectionPolicy(work)
	policy.ProfileFingerprint = uploadPolicy.CapabilityProfileFingerprint
	policy.DisclosureFingerprint = selected.DisclosureFingerprint
	policy.MaxSourceBytes = maximum
	policy.MaxExpandedBytes = maximum
	policy.MaxEntryBytes = maximum
	policy.MaxPages = semantics.MaxPDFPages
	policy.MaxFrames = uploadPolicy.MaxSourceFrames
	policy.MaxDurationMS = maximumDuration
	return policy, nil
}

func geminiRetentionUnconfirmed(err error) bool {
	return errors.Is(err, geminiembed.ErrRemoteRetentionUnconfirmed)
}
