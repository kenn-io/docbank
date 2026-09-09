package media

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/uploadcapability"
)

// UploadCapability projects a local, eligible original-file inspection into
// filename-free evidence for an embedding provider. Portable, changed, or
// ineligible records return a proof with no authority.
func (record CapabilityRecord) UploadCapability() document.UploadCapability {
	policy, local := record.InspectionPolicy()
	if !local || !record.Eligible || record.InputKind != document.RenditionInputOriginalFile {
		return document.UploadCapability{}
	}
	measurements := record.Measurements
	return uploadcapability.New(uploadcapability.Facts{
		Checksum: record.Checksum, SourceSHA256: record.SourceSHA256, SourceBytes: record.SourceBytes,
		MediaFamily: record.MediaFamily, MediaType: record.MediaType,
		DescriptorFingerprint: record.DescriptorFingerprint, ProfileFingerprint: record.ProfileFingerprint,
		DisclosureFingerprint: record.DisclosureFingerprint,
		Pixels:                measurements.Pixels, Frames: measurements.Frames, DurationMS: measurements.DurationMS, Pages: measurements.Pages,
		MaxSourceBytes: policy.MaxSourceBytes, MaxPixels: policy.MaxPixels, MaxFrames: policy.MaxFrames,
		MaxDurationMS: policy.MaxDurationMS, MaxPages: policy.MaxPages,
	})
}
